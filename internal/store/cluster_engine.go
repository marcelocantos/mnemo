// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// HeuristicThreshold is the single-link cut for the heuristic engine
// (docs/design/vault-clustering.md § Engine B): two documents are in the
// same theme when their TF-IDF cosine is at least this. The design's
// default; config wiring (vault_clustering.heuristic_threshold) lands in
// a later phase.
const HeuristicThreshold = 0.35

// heuristicEngineName is the source_engine / cluster_runs.engine tag for
// the fully-local default engine.
const heuristicEngineName = "heuristic"

// clusterDocKinds are the four corpus stream kinds the heuristic engine
// owns. A RecomputeThemes pass rewrites only themes built from these, so
// the dormant segment clusterer's rows (doc_kind='segment') are never
// touched.
var clusterDocKinds = []string{"decision", "compaction", "pattern", "vault_user"}

// themeCluster is one emitted theme: its member doc indices plus the
// fields derived from them.
type themeCluster struct {
	members []int
	label   string
	slug    string
	weight  float64
	repos   []string
	summary string // representative (centroid-closest) member text
	// simTo is centroid cosine per member index, for theme_members.similarity.
	simTo map[int]float64
}

// ClusterRun is the telemetry for one RecomputeThemes pass, mirroring a
// cluster_runs row.
type ClusterRun struct {
	ID           int64
	StartedAt    string
	EndedAt      string
	Engine       string
	InputDocs    int
	OutputThemes int
	Trigger      string
	FailureMode  string
}

// RecomputeThemes runs a full heuristic clustering pass: load the merged
// corpus, cluster it, and rewrite the heuristic engine's themes /
// theme_members rows, recording the pass in cluster_runs.
//
// vaultRoot is passed to ClusterCorpus (empty omits the vault_user
// stream). trigger is a free label for the cluster_runs row
// ("manual" / "interval" / "opportunistic"). p carries the resolved
// engine parameters (threshold, label config); a zero p resolves to
// DefaultClusterParams.
func (s *Store) RecomputeThemes(vaultRoot, trigger string, p ClusterParams) (*ClusterRun, error) {
	started := time.Now().UTC()
	if trigger == "" {
		trigger = "manual"
	}
	if p.Threshold <= 0 {
		p = DefaultClusterParams()
	}

	docs, err := s.ClusterCorpus(vaultRoot)
	if err != nil {
		return nil, err
	}
	clusters := clusterDocs(docs, p.Threshold)

	run, err := s.writeThemes(docs, clusters, started, trigger)
	if err != nil {
		return nil, err
	}
	return run, nil
}

// clusterDocs is the pure heuristic engine: TF-IDF vectorise, then form
// themes as the connected components of the "cosine ≥ threshold" graph.
//
// Single-link agglomerative clustering with a fixed cut is exactly
// connected-components on that graph, so this is one O(n²) similarity
// sweep with union-find — not the super-quadratic dendrogram rebuild
// that got the segment clusterer (🎯T64.11) retired. That cost profile
// is the thing T64.8's acceptance requires the document engine not to
// reproduce.
func clusterDocs(docs []ClusterCorpusDoc, threshold float64) []themeCluster {
	n := len(docs)
	if n == 0 {
		return nil
	}
	vecs := tfidfVectors(docs)

	uf := newUnionFind(n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if cosine(vecs[i], vecs[j]) >= threshold {
				uf.union(i, j)
			}
		}
	}

	// Group indices by component root, preserving first-seen order for a
	// stable output.
	groups := map[int][]int{}
	var roots []int
	for i := 0; i < n; i++ {
		r := uf.find(i)
		if _, seen := groups[r]; !seen {
			roots = append(roots, r)
		}
		groups[r] = append(groups[r], i)
	}

	out := make([]themeCluster, 0, len(roots))
	for _, r := range roots {
		members := groups[r]
		out = append(out, buildThemeCluster(docs, vecs, members))
	}
	return out
}

// buildThemeCluster derives a theme's label, weight, repos, and
// representative text from its member documents.
func buildThemeCluster(docs []ClusterCorpusDoc, vecs []map[string]float64, members []int) themeCluster {
	tc := themeCluster{members: members, simTo: map[int]float64{}}

	cent := map[string]float64{}
	repoSet := map[string]struct{}{}
	for _, mi := range members {
		tc.weight += docs[mi].Weight
		if r := docs[mi].Repo; r != "" {
			repoSet[r] = struct{}{}
		}
		for t, v := range vecs[mi] {
			cent[t] += v
		}
	}
	normalize(cent)
	tc.repos = sortedKeys(repoSet)

	// Representative = member closest to the centroid.
	best := -1.0
	rep := members[0]
	for _, mi := range members {
		sim := cosine(vecs[mi], cent)
		tc.simTo[mi] = sim
		if sim > best {
			best = sim
			rep = mi
		}
	}
	tc.summary = strings.TrimSpace(docs[rep].Text)

	tc.label = bigramLabel(docs, members)
	tc.slug = slugify(tc.label)
	return tc
}

// tfidfVectors builds L2-normalised TF-IDF vectors for the corpus.
// IDF is smoothed (log((1+N)/(1+df)) + 1) so a term shared by every
// document keeps a small positive weight rather than collapsing to
// zero — important for tiny corpora where df can equal N.
func tfidfVectors(docs []ClusterCorpusDoc) []map[string]float64 {
	n := len(docs)
	toks := make([][]string, n)
	df := map[string]int{}
	for i, d := range docs {
		toks[i] = tokenize(d.Text)
		seen := map[string]struct{}{}
		for _, t := range toks[i] {
			if _, ok := seen[t]; !ok {
				seen[t] = struct{}{}
				df[t]++
			}
		}
	}
	idf := map[string]float64{}
	for t, c := range df {
		idf[t] = math.Log(float64(1+n)/float64(1+c)) + 1
	}

	vecs := make([]map[string]float64, n)
	for i := range docs {
		tf := map[string]float64{}
		for _, t := range toks[i] {
			tf[t]++
		}
		v := make(map[string]float64, len(tf))
		for t, c := range tf {
			v[t] = c * idf[t]
		}
		normalize(v)
		vecs[i] = v
	}
	return vecs
}

// normalize scales a sparse vector to unit L2 norm in place.
func normalize(v map[string]float64) {
	var norm float64
	for _, c := range v {
		norm += c * c
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return
	}
	for t := range v {
		v[t] /= norm
	}
}

// bigramLabel picks the most frequent non-stopword bigram across a
// cluster's member texts, weighted by document weight (§ Engine B →
// Labelling). Falls back to the most frequent single token, then to a
// generic label. This is the offline default; the user-anchored and LLM
// label paths land in a later phase.
func bigramLabel(docs []ClusterCorpusDoc, members []int) string {
	bigrams := map[string]float64{}
	unigrams := map[string]float64{}
	for _, mi := range members {
		w := docs[mi].Weight
		toks := tokenize(docs[mi].Text)
		for k, t := range toks {
			unigrams[t] += w
			if k+1 < len(toks) {
				bigrams[toks[k]+" "+toks[k+1]] += w
			}
		}
	}
	if best := topKey(bigrams); best != "" {
		return titleCase(best)
	}
	if best := topKey(unigrams); best != "" {
		return titleCase(best)
	}
	return "theme"
}

// topKey returns the highest-weighted key, breaking ties lexically for
// determinism.
func topKey(m map[string]float64) string {
	best := ""
	var bestW float64
	for k, w := range m {
		if w > bestW || (w == bestW && k < best) {
			best, bestW = k, w
		}
	}
	if bestW <= 0 {
		return ""
	}
	return best
}

func titleCase(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// slugify renders a label as a kebab-case slug for theme page filenames.
func slugify(label string) string {
	var b strings.Builder
	lastDash := true // avoid leading dash
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// writeThemes rewrites the heuristic engine's themes / theme_members
// rows in one transaction and appends a cluster_runs row. Themes built
// from segment docs (the dormant clusterer) are left untouched: the
// delete is scoped to clusterDocKinds, and only themes orphaned by that
// delete are removed.
func (s *Store) writeThemes(docs []ClusterCorpusDoc, clusters []themeCluster, started time.Time, trigger string) (*ClusterRun, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.writeDB.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Drop this engine's prior members, then any theme left with none.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(clusterDocKinds)), ",")
	args := make([]any, len(clusterDocKinds))
	for i, k := range clusterDocKinds {
		args[i] = k
	}
	if _, err := tx.Exec(
		`DELETE FROM theme_members WHERE doc_kind IN (`+placeholders+`)`, args...); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`DELETE FROM themes WHERE id NOT IN (SELECT DISTINCT theme_id FROM theme_members)`); err != nil {
		return nil, err
	}

	for _, tc := range clusters {
		ids := make([]string, len(tc.members))
		for i, mi := range tc.members {
			ids[i] = docs[mi].DocID
		}
		sort.Strings(ids)
		themeID := themeIDFromMembers(ids)

		if _, err := tx.Exec(`
			INSERT INTO themes (id, label, summary, weight, repos, depth, first_seen, last_touched, computed_at)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				label = excluded.label, summary = excluded.summary, weight = excluded.weight,
				repos = excluded.repos, last_touched = excluded.last_touched, computed_at = excluded.computed_at
		`, themeID, tc.label, tc.summary, tc.weight, reposJSON(tc.repos), now, now, now); err != nil {
			return nil, fmt.Errorf("insert theme %s: %w", themeID, err)
		}
		for _, mi := range tc.members {
			if _, err := tx.Exec(`
				INSERT INTO theme_members (theme_id, doc_kind, entity_id, membership_kind, similarity)
				VALUES (?, ?, ?, 'primary', ?)
				ON CONFLICT(theme_id, doc_kind, entity_id) DO UPDATE SET
					similarity = excluded.similarity
			`, themeID, docs[mi].Kind, docs[mi].EntityID, tc.simTo[mi]); err != nil {
				return nil, err
			}
		}
	}

	ended := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.Exec(`
		INSERT INTO cluster_runs (started_at, ended_at, engine, input_docs, output_themes, trigger)
		VALUES (?, ?, ?, ?, ?, ?)
	`, started.Format(time.RFC3339), ended, heuristicEngineName, len(docs), len(clusters), trigger)
	if err != nil {
		return nil, err
	}
	runID, _ := res.LastInsertId()

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ClusterRun{
		ID: runID, StartedAt: started.Format(time.RFC3339), EndedAt: ended,
		Engine: heuristicEngineName, InputDocs: len(docs), OutputThemes: len(clusters),
		Trigger: trigger,
	}, nil
}

func reposJSON(repos []string) string {
	if len(repos) == 0 {
		return "[]"
	}
	parts := make([]string, len(repos))
	for i, r := range repos {
		parts[i] = fmt.Sprintf("%q", r)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// --- union-find --------------------------------------------------------

type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (uf *unionFind) find(x int) int {
	for uf.parent[x] != x {
		uf.parent[x] = uf.parent[uf.parent[x]] // path halving
		x = uf.parent[x]
	}
	return x
}

func (uf *unionFind) union(a, b int) {
	ra, rb := uf.find(a), uf.find(b)
	if ra == rb {
		return
	}
	if uf.rank[ra] < uf.rank[rb] {
		ra, rb = rb, ra
	}
	uf.parent[rb] = ra
	if uf.rank[ra] == uf.rank[rb] {
		uf.rank[ra]++
	}
}
