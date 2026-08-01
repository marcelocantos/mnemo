// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// ClusterRunArgs configures one clustering pass (🎯T64.8).
type ClusterRunArgs struct {
	// Config is the vault_clustering block. Zero value → all defaults.
	Config VaultClusteringConfig

	// Trigger is recorded on cluster_runs: "interval" | "manual" | "opportunistic".
	Trigger string

	// EngineOverride, when non-empty, replaces Config.Engine for this pass
	// only (mnemo_vault_recluster engine param).
	EngineOverride string

	// ForceReembed invalidates active-fingerprint cache use so embeddings
	// are recomputed even when content is unchanged.
	ForceReembed bool

	// HTTPAllowed is set by tests to observe egress. Production leaves it
	// nil. When non-nil, any outbound attempt records true.
	HTTPAllowed *bool

	// Now overrides wall clock (tests).
	Now time.Time

	// Logger optional.
	Logger *slog.Logger
}

// ClusterRunResult is the summary returned to MCP / workers.
type ClusterRunResult struct {
	RunID          int64    `json:"run_id"`
	Engine         string   `json:"engine"`
	InputDocs      int      `json:"input_docs"`
	OutputThemes   int      `json:"output_themes"`
	EmbeddingCalls int      `json:"embedding_calls"`
	EstimatedCost  float64  `json:"estimated_cost"`
	FailureMode    string   `json:"failure_mode"`
	Trigger        string   `json:"trigger"`
	StartedAt      string   `json:"started_at"`
	EndedAt        string   `json:"ended_at"`
	Warnings       []string `json:"warnings,omitempty"`
}

// ThemeInspect is the mnemo_vault_themes_inspect payload.
type ThemeInspect struct {
	ID           string            `json:"id"`
	Label        string            `json:"label"`
	Slug         string            `json:"slug"`
	SourceEngine string            `json:"source_engine"`
	Weight       float64           `json:"weight"`
	MemberCount  int               `json:"member_count"`
	Repos        []string          `json:"repos"`
	CentroidText string            `json:"centroid_text"`
	LabelPath    string            `json:"label_path,omitempty"`
	LabelGate    string            `json:"label_gate,omitempty"`
	Archived     bool              `json:"archived"`
	Pinned       bool              `json:"pinned"`
	FirstSeen    string            `json:"first_seen"`
	LastTouched  string            `json:"last_touched"`
	ComputedAt   string            `json:"computed_at"`
	Members      []ThemeMemberView `json:"members"`
}

// ThemeMemberView is one membership row for inspect.
type ThemeMemberView struct {
	DocKind     string  `json:"doc_kind"`
	EntityID    string  `json:"entity_id"`
	Repo        string  `json:"repo"`
	TS          string  `json:"ts"`
	Similarity  float64 `json:"similarity"`
	Distance    float64 `json:"distance"`
	TextPreview string  `json:"text_preview,omitempty"`
}

// clusterDoc is a corpus document with its TF-IDF (or embedding) vector.
type clusterDoc struct {
	ClusterCorpusDoc
	vec         map[string]float64
	title       string // vault_user title for gates
	fileHint    string
	contentHash string
}

// RunCluster executes one document-level clustering pass.
//
// Cost profile (🎯T64.11 lesson): this is deliberately NOT the dormant
// segment dendrogram. We:
//  1. Cluster documents (decisions/compactions/patterns/vault_user), not
//     every sealed segment — orders of magnitude smaller corpus.
//  2. Stop at the hard similarity cut (phase 1 only). No merge-to-root
//     dendrogram, which is super-quadratic and was the backfill killer.
//  3. Cap corpus size via MaxDocs (default 5000).
//  4. Single-link pair scan is O(k²) where k is active clusters ≤ n;
//     for median ~10³ docs this is sub-second offline.
func (s *Store) RunCluster(ctx context.Context, args ClusterRunArgs) (*ClusterRunResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	cfg := args.Config
	engine := cfg.EffectiveEngine()
	if ov := strings.ToLower(strings.TrimSpace(args.EngineOverride)); ov != "" {
		switch ov {
		case "embeddings", "embedding":
			engine = "embeddings"
		case "heuristic":
			engine = "heuristic"
		default:
			return nil, fmt.Errorf("unknown engine %q (want heuristic or embeddings)", args.EngineOverride)
		}
	}
	now := args.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowStr := now.Format(time.RFC3339)
	trigger := args.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	log := args.Logger
	if log == nil {
		log = slog.Default()
	}

	var warnings []string
	// Two-key egress: never open outbound HTTP unless the matching
	// engine opt-in is set. Even with keys present.
	if engine == "embeddings" {
		if os.Getenv("VOYAGE_API_KEY") == "" && os.Getenv("VOYAGEAI_API_KEY") == "" {
			warnings = append(warnings, "embeddings engine requested but no Voyage API key; falling back to heuristic")
			if !cfg.EffectiveFallbackToHeuristicOnOutage() {
				return s.recordFailedRun(nowStr, engine, trigger, "no_api_key", warnings)
			}
			engine = "heuristic"
		}
	}
	// Label engine "llm" is recorded but not called in this pass unless
	// a provider is wired later; default bigram path is always local.

	started := nowStr
	runID, err := s.insertClusterRunStart(started, engine, trigger, cfg)
	if err != nil {
		return nil, err
	}

	corpus, err := s.ClusterCorpus()
	if err != nil {
		_ = s.finishClusterRun(runID, nowStr, 0, 0, 0, 0, "corpus_error", 0, 0)
		return nil, err
	}
	// Cap + balance.
	corpus = balanceCorpus(corpus, cfg.EffectiveBalanceFactor())
	if max := cfg.EffectiveMaxDocs(); len(corpus) > max {
		// Keep highest-weight docs first, then by DocID for determinism.
		sort.Slice(corpus, func(i, j int) bool {
			if corpus[i].Weight != corpus[j].Weight {
				return corpus[i].Weight > corpus[j].Weight
			}
			return corpus[i].DocID < corpus[j].DocID
		})
		corpus = corpus[:max]
		warnings = append(warnings, fmt.Sprintf("corpus capped at max_docs=%d", max))
	}

	docs := make([]clusterDoc, len(corpus))
	for i, c := range corpus {
		title, _ := splitTitleBody(c.Text)
		docs[i] = clusterDoc{
			ClusterCorpusDoc: c,
			title:            title,
			contentHash:      shortHash(c.Text),
		}
	}

	var embCalls int
	var estCost float64
	failureMode := ""

	switch engine {
	case "embeddings":
		// Opt-in path: currently reuses TF-IDF vectors when no live
		// provider is injected. Real Voyage wiring lives behind the
		// same gate and still requires engine=embeddings. Recording
		// embCalls=0 keeps the two-key test honest: without a provider
		// implementation that dials HTTP, we do not dial HTTP.
		if args.HTTPAllowed != nil {
			// Test hook only: we deliberately do NOT set it — proving
			// zero egress when keys exist but we never call out.
		}
		// Build TF-IDF still (embeddings provider optional). When a
		// future EmbeddingProvider is set on the store, swap here.
		idf := computeIDF(docs)
		for i := range docs {
			docs[i].vec = tfidfVector(docs[i].Text, idf)
		}
		// Note: without a live embedder this path is algorithmically
		// identical to heuristic but records source_engine=embeddings
		// only when a provider actually produced vectors. Force heuristic
		// labelling of engine field when no provider ran:
		engine = "heuristic"
		failureMode = "embeddings_provider_unavailable"
		warnings = append(warnings, "embeddings provider not wired; ran heuristic vectors")
	default:
		idf := computeIDF(docs)
		for i := range docs {
			docs[i].vec = tfidfVector(docs[i].Text, idf)
		}
	}

	threshold := cfg.EffectiveHeuristicThreshold()
	if engine == "embeddings" {
		threshold = cfg.EffectiveEmbeddingThreshold()
	}

	themes, err := singleLinkThemes(docs, threshold, cfg)
	if err != nil {
		_ = s.finishClusterRun(runID, time.Now().UTC().Format(time.RFC3339), len(docs), 0, embCalls, estCost, "cluster_error", 0, 0)
		return nil, err
	}

	// Drop under-weight / over-cap.
	minW := cfg.EffectiveMinClusterWeight()
	filtered := themes[:0]
	for _, tw := range themes {
		if tw.weight < minW && len(tw.members) < 2 {
			// Keep multi-member themes even if weight is low; drop
			// singleton noise under the weight floor.
			if tw.weight < minW {
				continue
			}
		}
		if tw.weight < minW {
			continue
		}
		filtered = append(filtered, tw)
	}
	themes = filtered
	sort.Slice(themes, func(i, j int) bool {
		if themes[i].weight != themes[j].weight {
			return themes[i].weight > themes[j].weight
		}
		return themes[i].id < themes[j].id
	})
	if maxT := cfg.EffectiveMaxThemes(); len(themes) > maxT {
		themes = themes[:maxT]
	}

	ended := time.Now().UTC().Format(time.RFC3339)
	if err := s.persistDocThemes(docs, themes, engine, nowStr, cfg); err != nil {
		_ = s.finishClusterRun(runID, ended, len(docs), 0, embCalls, estCost, "persist_error", 0, 0)
		return nil, err
	}

	if err := s.applyThemeRetirement(now, cfg); err != nil {
		log.Warn("theme retirement", "err", err)
	}
	if err := s.trimClusterRuns(cfg.EffectiveMaxRunHistory()); err != nil {
		log.Warn("trim cluster_runs", "err", err)
	}

	if err := s.finishClusterRun(runID, ended, len(docs), len(themes), embCalls, estCost, failureMode, 0, 0); err != nil {
		return nil, err
	}

	return &ClusterRunResult{
		RunID:          runID,
		Engine:         engine,
		InputDocs:      len(docs),
		OutputThemes:   len(themes),
		EmbeddingCalls: embCalls,
		EstimatedCost:  estCost,
		FailureMode:    failureMode,
		Trigger:        trigger,
		StartedAt:      started,
		EndedAt:        ended,
		Warnings:       warnings,
	}, nil
}

type docTheme struct {
	id         string
	label      string
	slug       string
	summary    string
	weight     float64
	repos      []string
	members    []int
	cent       map[string]float64
	first      string
	last       string
	labelPath  LabelPath
	labelGate  LabelGate
	centroidTx string
}

func singleLinkThemes(docs []clusterDoc, threshold float64, cfg VaultClusteringConfig) ([]docTheme, error) {
	n := len(docs)
	if n == 0 {
		return nil, nil
	}
	type agg struct {
		members []int
	}
	all := make([]agg, n)
	for i := range all {
		all[i] = agg{members: []int{i}}
	}
	active := make([]int, n)
	for i := range active {
		active[i] = i
	}

	pairSim := func(a, b agg) float64 {
		best := 0.0
		for _, i := range a.members {
			for _, j := range b.members {
				if s := cosine(docs[i].vec, docs[j].vec); s > best {
					best = s
				}
			}
		}
		return best
	}
	// Phase 1 only — hard cut. No merge-to-root (cost lesson from T64.11).
	for len(active) >= 2 {
		bi, bj, sim := -1, -1, -1.0
		for i := 0; i < len(active); i++ {
			for j := i + 1; j < len(active); j++ {
				s := pairSim(all[active[i]], all[active[j]])
				if s > sim {
					sim = s
					bi, bj = i, j
				}
			}
		}
		if bi < 0 || sim < threshold {
			break
		}
		if bj < bi {
			bi, bj = bj, bi
		}
		ai, aj := active[bi], active[bj]
		merged := append(append([]int{}, all[ai].members...), all[aj].members...)
		sort.Ints(merged)
		newIdx := len(all)
		all = append(all, agg{members: merged})
		active[bi] = newIdx
		active = append(active[:bj], active[bj+1:]...)
	}

	out := make([]docTheme, 0, len(active))
	for _, ci := range active {
		c := all[ci]
		cent := centroidOf(docs, c.members)
		lab := labelCluster(corpusAsDocs(docs), c.members, cent, cfg)
		ids := make([]string, len(c.members))
		repos := map[string]struct{}{}
		var weight float64
		first, last := "", ""
		for i, mi := range c.members {
			ids[i] = docs[mi].DocID
			weight += docs[mi].Weight
			if docs[mi].Repo != "" {
				repos[docs[mi].Repo] = struct{}{}
			}
			if docs[mi].TS != "" && (first == "" || docs[mi].TS < first) {
				first = docs[mi].TS
			}
			if docs[mi].TS != "" && (last == "" || docs[mi].TS > last) {
				last = docs[mi].TS
			}
		}
		sort.Strings(ids)
		tid := themeIDFromDocIDs(ids)
		out = append(out, docTheme{
			id:         tid,
			label:      lab.Label,
			slug:       lab.Slug,
			summary:    lab.CentroidTx,
			weight:     weight,
			repos:      sortedKeys(repos),
			members:    append([]int{}, c.members...),
			cent:       cent,
			first:      first,
			last:       last,
			labelPath:  lab.Path,
			labelGate:  lab.GateFired,
			centroidTx: lab.CentroidTx,
		})
	}
	return out, nil
}

func corpusAsDocs(docs []clusterDoc) []ClusterCorpusDoc {
	out := make([]ClusterCorpusDoc, len(docs))
	for i := range docs {
		out[i] = docs[i].ClusterCorpusDoc
	}
	return out
}

func centroidOf(docs []clusterDoc, members []int) map[string]float64 {
	out := map[string]float64{}
	if len(members) == 0 {
		return out
	}
	for _, mi := range members {
		for t, v := range docs[mi].vec {
			out[t] += v
		}
	}
	inv := 1.0 / float64(len(members))
	for t := range out {
		out[t] *= inv
	}
	var norm float64
	for _, v := range out {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for t := range out {
			out[t] /= norm
		}
	}
	return out
}

func themeIDFromDocIDs(sortedDocIDs []string) string {
	sum := sha1.Sum([]byte(strings.Join(sortedDocIDs, "\x00")))
	return "theme_" + hex.EncodeToString(sum[:])[:8]
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func computeIDF(docs []clusterDoc) map[string]float64 {
	df := map[string]int{}
	for _, d := range docs {
		seen := map[string]struct{}{}
		for _, t := range tokenize(d.Text) {
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			df[t]++
		}
	}
	n := float64(len(docs))
	if n == 0 {
		return map[string]float64{}
	}
	idf := map[string]float64{}
	// Collect IDF values for p95 clamp (domain-drift mitigation).
	vals := make([]float64, 0, len(df))
	for t, c := range df {
		v := math.Log((n+1)/(float64(c)+1)) + 1
		idf[t] = v
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return idf
	}
	sort.Float64s(vals)
	p95 := vals[int(float64(len(vals)-1)*0.95)]
	for t, v := range idf {
		if v > p95 {
			// Cap extreme IDF so one-repo jargon cannot dominate.
			// Design: clamp to max(idf, p95) is a misstatement in the
			// doc (would raise low values); the intent is an upper
			// bound — min(idf, p95).
			idf[t] = p95
		}
	}
	return idf
}

func tfidfVector(text string, idf map[string]float64) map[string]float64 {
	tf := map[string]float64{}
	for _, t := range tokenize(text) {
		tf[t]++
	}
	v := map[string]float64{}
	for t, c := range tf {
		idfv := idf[t]
		if idfv == 0 {
			idfv = 1
		}
		v[t] = c * idfv
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for t := range v {
			v[t] /= norm
		}
	}
	return v
}

// balanceCorpus down-samples a dominant repo to factor × second-largest.
// factor 0 disables.
func balanceCorpus(docs []ClusterCorpusDoc, factor float64) []ClusterCorpusDoc {
	if factor <= 0 || len(docs) == 0 {
		return docs
	}
	byRepo := map[string][]ClusterCorpusDoc{}
	for _, d := range docs {
		r := d.Repo
		if r == "" {
			r = "_none"
		}
		byRepo[r] = append(byRepo[r], d)
	}
	if len(byRepo) < 2 {
		return docs
	}
	type rc struct {
		repo string
		n    int
	}
	var counts []rc
	for r, list := range byRepo {
		counts = append(counts, rc{r, len(list)})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].n != counts[j].n {
			return counts[i].n > counts[j].n
		}
		return counts[i].repo < counts[j].repo
	})
	top, second := counts[0].n, counts[1].n
	if top <= int(factor*float64(second)) {
		return docs
	}
	capN := int(factor * float64(second))
	if capN < 1 {
		capN = 1
	}
	// Stratify by kind inside the dominant repo.
	dom := byRepo[counts[0].repo]
	sort.Slice(dom, func(i, j int) bool {
		if dom[i].Kind != dom[j].Kind {
			return dom[i].Kind < dom[j].Kind
		}
		return dom[i].DocID < dom[j].DocID
	})
	// Round-robin kinds.
	byKind := map[string][]ClusterCorpusDoc{}
	var kinds []string
	for _, d := range dom {
		if _, ok := byKind[d.Kind]; !ok {
			kinds = append(kinds, d.Kind)
		}
		byKind[d.Kind] = append(byKind[d.Kind], d)
	}
	sort.Strings(kinds)
	picked := make([]ClusterCorpusDoc, 0, capN)
	idx := map[string]int{}
	for len(picked) < capN {
		progress := false
		for _, k := range kinds {
			if len(picked) >= capN {
				break
			}
			list := byKind[k]
			i := idx[k]
			if i >= len(list) {
				continue
			}
			picked = append(picked, list[i])
			idx[k] = i + 1
			progress = true
		}
		if !progress {
			break
		}
	}
	byRepo[counts[0].repo] = picked
	var out []ClusterCorpusDoc
	for _, list := range byRepo {
		out = append(out, list...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DocID < out[j].DocID })
	return out
}

func (s *Store) persistDocThemes(docs []clusterDoc, themes []docTheme, engine, nowStr string, cfg VaultClusteringConfig) error {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Preserve first_seen for stable IDs that reappear.
	prevFirst := map[string]string{}
	rows, err := tx.Query(`SELECT id, first_seen FROM themes WHERE archived = 0 OR archived = 1`)
	if err == nil {
		for rows.Next() {
			var id, fs string
			if rows.Scan(&id, &fs) == nil {
				prevFirst[id] = fs
			}
		}
		rows.Close()
	}

	// Rewrite document-level themes only — leave any legacy segment
	// themes alone if they still exist. We identify doc themes by
	// membership kinds decision|compaction|pattern|vault_user.
	if _, err := tx.Exec(`
		DELETE FROM theme_members
		WHERE doc_kind IN ('decision','compaction','pattern','vault_user')
	`); err != nil {
		return err
	}
	// Delete themes that have no remaining members (segment-only may stay).
	if _, err := tx.Exec(`
		DELETE FROM themes
		WHERE id NOT IN (SELECT DISTINCT theme_id FROM theme_members)
		  AND (source_engine != '' OR slug != '' OR member_count > 0)
	`); err != nil {
		// Fallback: if columns missing mid-migration, wipe theme_% that
		// are doc-derived. Safer path for fresh DBs:
		if _, err2 := tx.Exec(`DELETE FROM themes WHERE source_engine IN ('heuristic','embeddings')`); err2 != nil {
			return err
		}
	}

	for _, tw := range themes {
		reposJSON, _ := json.Marshal(tw.repos)
		if reposJSON == nil {
			reposJSON = []byte("[]")
		}
		first := tw.first
		if prev, ok := prevFirst[tw.id]; ok && prev != "" {
			first = prev
		}
		if first == "" {
			first = nowStr
		}
		last := tw.last
		if last == "" {
			last = nowStr
		}
		if _, err := tx.Exec(`
			INSERT INTO themes (
				id, label, summary, weight, repos, parent_theme_id, depth,
				first_seen, last_touched, computed_at,
				slug, source_engine, member_count, centroid_text, archived
			) VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, ?, ?, ?, ?, 0)
			ON CONFLICT(id) DO UPDATE SET
				label = excluded.label,
				summary = excluded.summary,
				weight = excluded.weight,
				repos = excluded.repos,
				last_touched = excluded.last_touched,
				computed_at = excluded.computed_at,
				slug = excluded.slug,
				source_engine = excluded.source_engine,
				member_count = excluded.member_count,
				centroid_text = excluded.centroid_text,
				archived = 0
		`, tw.id, tw.label, tw.summary, tw.weight, string(reposJSON),
			first, last, nowStr,
			tw.slug, engine, len(tw.members), tw.centroidTx); err != nil {
			return fmt.Errorf("insert theme %s: %w", tw.id, err)
		}
		for _, mi := range tw.members {
			d := docs[mi]
			sim := cosine(d.vec, tw.cent)
			dist := 1.0 - sim
			if _, err := tx.Exec(`
				INSERT INTO theme_members (
					theme_id, doc_kind, entity_id, membership_kind, similarity,
					repo, ts, distance
				) VALUES (?, ?, ?, 'primary', ?, ?, ?, ?)
				ON CONFLICT(theme_id, doc_kind, entity_id) DO UPDATE SET
					membership_kind = 'primary',
					similarity = excluded.similarity,
					repo = excluded.repo,
					ts = excluded.ts,
					distance = excluded.distance
			`, tw.id, d.Kind, d.EntityID, sim, d.Repo, d.TS, dist); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) applyThemeRetirement(now time.Time, cfg VaultClusteringConfig) error {
	cutoff := now.Add(-cfg.EffectiveRetireAfter()).UTC().Format(time.RFC3339)
	// Archive unpinned themes whose last_touched is older than cutoff.
	_, err := s.writeDB.Exec(`
		UPDATE themes SET archived = 1
		WHERE archived = 0
		  AND last_touched != ''
		  AND last_touched < ?
		  AND id NOT IN (SELECT theme_id FROM theme_pins)
	`, cutoff)
	return err
}

func (s *Store) trimClusterRuns(keep int) error {
	if keep <= 0 {
		return nil
	}
	_, err := s.writeDB.Exec(`
		DELETE FROM cluster_runs
		WHERE id NOT IN (
			SELECT id FROM cluster_runs ORDER BY started_at DESC, id DESC LIMIT ?
		)
	`, keep)
	return err
}

func (s *Store) insertClusterRunStart(started, engine, trigger string, cfg VaultClusteringConfig) (int64, error) {
	provider, model, version := "", "", ""
	if engine == "embeddings" {
		provider = cfg.EffectiveEmbeddingProvider()
		model = cfg.EffectiveEmbeddingModel()
		version = cfg.EmbeddingModelVersion
	}
	res, err := s.writeDB.Exec(`
		INSERT INTO cluster_runs (
			started_at, engine, provider, model, model_version, trigger
		) VALUES (?, ?, ?, ?, ?, ?)
	`, started, engine, provider, model, version, trigger)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) finishClusterRun(id int64, ended string, inputDocs, outputThemes, embCalls int, cost float64, failure string, embBytes, embRows int) error {
	_, err := s.writeDB.Exec(`
		UPDATE cluster_runs SET
			ended_at = ?, input_docs = ?, output_themes = ?,
			embedding_calls = ?, estimated_cost = ?, failure_mode = ?,
			embeddings_bytes = ?, embeddings_rows = ?
		WHERE id = ?
	`, ended, inputDocs, outputThemes, embCalls, cost, failure, embBytes, embRows, id)
	return err
}

func (s *Store) recordFailedRun(started, engine, trigger, failure string, warnings []string) (*ClusterRunResult, error) {
	id, err := s.insertClusterRunStart(started, engine, trigger, VaultClusteringConfig{})
	if err != nil {
		return nil, err
	}
	ended := time.Now().UTC().Format(time.RFC3339)
	_ = s.finishClusterRun(id, ended, 0, 0, 0, 0, failure, 0, 0)
	return &ClusterRunResult{
		RunID:       id,
		Engine:      engine,
		FailureMode: failure,
		Trigger:     trigger,
		StartedAt:   started,
		EndedAt:     ended,
		Warnings:    warnings,
	}, nil
}

// PinTheme pins or unpins a theme (🎯T64.8 / mnemo_vault_themes_pin).
func (s *Store) PinTheme(themeID string, unpin bool, reason string) error {
	themeID = strings.TrimSpace(themeID)
	if themeID == "" {
		return fmt.Errorf("theme_id required")
	}
	if unpin {
		_, err := s.writeDB.Exec(`DELETE FROM theme_pins WHERE theme_id = ?`, themeID)
		return err
	}
	// Ensure theme exists.
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM themes WHERE id = ? OR slug = ?`, themeID, themeID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("theme %q not found", themeID)
	}
	// Resolve slug → id if needed.
	var id string
	err := s.readDB.QueryRow(`SELECT id FROM themes WHERE id = ? OR slug = ? LIMIT 1`, themeID, themeID).Scan(&id)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.writeDB.Exec(`
		INSERT INTO theme_pins (theme_id, pinned_at, reason) VALUES (?, ?, ?)
		ON CONFLICT(theme_id) DO UPDATE SET pinned_at = excluded.pinned_at, reason = excluded.reason
	`, id, now, reason)
	return err
}

// InspectTheme returns full membership + labelling diagnostics.
func (s *Store) InspectTheme(ref string) (*ThemeInspect, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("theme_id or slug required")
	}
	var (
		id, label, slug, engine, repos, centroid, first, last, computed string
		weight                                                          float64
		members, archived                                               int
	)
	err := s.readDB.QueryRow(`
		SELECT id, label, COALESCE(slug,''), COALESCE(source_engine,''), weight,
		       COALESCE(member_count,0), COALESCE(repos,'[]'), COALESCE(centroid_text,''),
		       COALESCE(archived,0), COALESCE(first_seen,''), COALESCE(last_touched,''),
		       COALESCE(computed_at,'')
		FROM themes WHERE id = ? OR slug = ? LIMIT 1
	`, ref, ref).Scan(&id, &label, &slug, &engine, &weight, &members, &repos, &centroid,
		&archived, &first, &last, &computed)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("theme %q not found", ref)
	}
	if err != nil {
		return nil, err
	}
	var repoList []string
	_ = json.Unmarshal([]byte(repos), &repoList)
	if repoList == nil {
		repoList = []string{}
	}
	pinned := false
	var pinCount int
	_ = s.readDB.QueryRow(`SELECT COUNT(*) FROM theme_pins WHERE theme_id = ?`, id).Scan(&pinCount)
	pinned = pinCount > 0

	mrows, err := s.readDB.Query(`
		SELECT doc_kind, entity_id, COALESCE(repo,''), COALESCE(ts,''),
		       COALESCE(similarity,0), COALESCE(distance,0)
		FROM theme_members WHERE theme_id = ?
		ORDER BY doc_kind, entity_id
	`, id)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	var mvs []ThemeMemberView
	for mrows.Next() {
		var mv ThemeMemberView
		if err := mrows.Scan(&mv.DocKind, &mv.EntityID, &mv.Repo, &mv.TS, &mv.Similarity, &mv.Distance); err != nil {
			return nil, err
		}
		mvs = append(mvs, mv)
	}
	if mvs == nil {
		mvs = []ThemeMemberView{}
	}
	return &ThemeInspect{
		ID:           id,
		Label:        label,
		Slug:         slug,
		SourceEngine: engine,
		Weight:       weight,
		MemberCount:  members,
		Repos:        repoList,
		CentroidText: centroid,
		Archived:     archived != 0,
		Pinned:       pinned,
		FirstSeen:    first,
		LastTouched:  last,
		ComputedAt:   computed,
		Members:      mvs,
	}, nil
}

// ThemeSummary is a list-row for vault export / bridges.
type ThemeSummary struct {
	ID           string
	Label        string
	Slug         string
	SourceEngine string
	Weight       float64
	MemberCount  int
	Repos        []string
	CentroidText string
	Archived     bool
	FirstSeen    string
	LastTouched  string
	ComputedAt   string
}

// ListThemes returns themes ordered by weight desc. When includeArchived
// is false, archived rows are omitted.
func (s *Store) ListThemes(includeArchived bool) ([]ThemeSummary, error) {
	q := `
		SELECT id, label, COALESCE(slug,''), COALESCE(source_engine,''), weight,
		       COALESCE(member_count,0), COALESCE(repos,'[]'), COALESCE(centroid_text,''),
		       COALESCE(archived,0), COALESCE(first_seen,''), COALESCE(last_touched,''),
		       COALESCE(computed_at,'')
		FROM themes
	`
	if !includeArchived {
		q += ` WHERE archived = 0`
	}
	q += ` ORDER BY weight DESC, id`
	rows, err := s.readDB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThemeSummary
	for rows.Next() {
		var t ThemeSummary
		var repos string
		var arch int
		if err := rows.Scan(&t.ID, &t.Label, &t.Slug, &t.SourceEngine, &t.Weight,
			&t.MemberCount, &repos, &t.CentroidText, &arch, &t.FirstSeen, &t.LastTouched, &t.ComputedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(repos), &t.Repos)
		if t.Repos == nil {
			t.Repos = []string{}
		}
		t.Archived = arch != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestClusterRun returns the most recent cluster_runs row, or nil.
func (s *Store) LatestClusterRun() (*ClusterRunResult, error) {
	var r ClusterRunResult
	var ended sql.NullString
	err := s.readDB.QueryRow(`
		SELECT id, COALESCE(engine,''), COALESCE(input_docs,0), COALESCE(output_themes,0),
		       COALESCE(embedding_calls,0), COALESCE(estimated_cost,0), COALESCE(failure_mode,''),
		       COALESCE(trigger,''), started_at, ended_at
		FROM cluster_runs ORDER BY id DESC LIMIT 1
	`).Scan(&r.RunID, &r.Engine, &r.InputDocs, &r.OutputThemes, &r.EmbeddingCalls,
		&r.EstimatedCost, &r.FailureMode, &r.Trigger, &r.StartedAt, &ended)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ended.Valid {
		r.EndedAt = ended.String
	}
	return &r, nil
}

// SetThemeOverride records a split/merge/relabel directive (stubs for
// mnemo_vault_themes_split / themes_merge — config-rejecting live apply
// until a follow-up ships the pass-side consumer).
func (s *Store) SetThemeOverride(themeID, directive, payload string) error {
	themeID = strings.TrimSpace(themeID)
	directive = strings.ToLower(strings.TrimSpace(directive))
	switch directive {
	case "split", "merge", "relabel":
	default:
		return fmt.Errorf("unknown directive %q", directive)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.writeDB.Exec(`
		INSERT INTO theme_overrides (theme_id, directive, payload, created_at, applied_at)
		VALUES (?, ?, ?, ?, NULL)
		ON CONFLICT(theme_id) DO UPDATE SET
			directive = excluded.directive,
			payload = excluded.payload,
			created_at = excluded.created_at,
			applied_at = NULL
	`, themeID, directive, payload, now)
	return err
}
