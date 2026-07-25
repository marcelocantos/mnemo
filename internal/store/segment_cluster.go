// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// SecondaryThemeThreshold: centroid cosine above this → secondary membership.
const SecondaryThemeThreshold = 0.35

// ClusterMergeThreshold: single-link merge while nearest pair ≥ this
// defines the hard cut (primary theme assignment).
const ClusterMergeThreshold = 0.45

type segDoc struct {
	id      string
	session string
	repo    string
	label   string
	summary string
	firstTS string
	lastTS  string
	vec     map[string]float64
}

type aggCluster struct {
	members []int // indices into docs
	parent  int   // index in all, -1 if none
	depth   int
}

// ClusterSealedSegments runs single-link agglomerative clustering over
// sealed topic segments.
//
// DORMANT as of 🎯T64.11 — nothing in production calls this, and
// TestNoClusteringOnIngestPath fails the build if anything starts to.
//
// It was written to make retrieval thematic by assigning every span to
// a precomputed theme object. That turned out to be the wrong shape for
// the goal: thematic retrieval is a search problem over span text, and
// a query already returns related spans from many sessions ranked by
// score without anyone naming a theme. Meanwhile the cost was real —
// every pass rebuilt the whole dendrogram from scratch (bestPair scans
// all active pairs, pairSim is member×member cosine, phase 2 merges to
// a single root), which is super-quadratic in sealed-span count and was
// the dominant CPU cost of a backfill on a full-sized index.
//
// It is kept rather than deleted because cross-session theme objects
// may return as an offline analytic over span summaries — a batch
// question ("show me everything about X as a group"), not something the
// ingest path should compute. Anything reviving it needs an incremental
// or approximate design; this implementation must not go back on a hot
// path.
//
// Phase 1 (cut): merge while pair sim ≥ ClusterMergeThreshold → leaf
// themes (primary membership; multi-member when similar segments merge).
//
// Phase 2 (dendrogram): continue merging past the cut until one root
// remains, creating parent themes and setting parent_theme_id/depth so
// ancestors are navigable (reverses flat-themes non-goal).
//
// Secondary memberships: cosine to another leaf-theme centroid ≥
// SecondaryThemeThreshold (never below).
func (s *Store) ClusterSealedSegments() error {
	rows, err := s.readDB.Query(`
		SELECT id, session_id, COALESCE(repo,''), COALESCE(label,''), COALESCE(summary,''),
		       COALESCE(first_ts,''), COALESCE(last_ts,'')
		FROM topic_segments
		WHERE sealed = 1
		ORDER BY id
	`)
	if err != nil {
		return err
	}
	var docs []segDoc
	for rows.Next() {
		var d segDoc
		if err := rows.Scan(&d.id, &d.session, &d.repo, &d.label, &d.summary, &d.firstTS, &d.lastTS); err != nil {
			rows.Close()
			return err
		}
		d.vec = tfVector(strings.TrimSpace(d.label + " " + d.summary))
		docs = append(docs, d)
	}
	rows.Close()
	if len(docs) == 0 {
		return nil
	}

	n := len(docs)
	all := make([]aggCluster, n)
	for i := range all {
		all[i] = aggCluster{members: []int{i}, parent: -1, depth: 0}
	}
	active := make([]int, n)
	for i := range active {
		active[i] = i
	}

	pairSim := func(a, b aggCluster) float64 {
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
	centroid := func(c aggCluster) map[string]float64 {
		out := map[string]float64{}
		if len(c.members) == 0 {
			return out
		}
		for _, mi := range c.members {
			for t, v := range docs[mi].vec {
				out[t] += v
			}
		}
		inv := 1.0 / float64(len(c.members))
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
	memberSegIDs := func(c aggCluster) []string {
		ids := make([]string, len(c.members))
		for i, mi := range c.members {
			ids[i] = docs[mi].id
		}
		sort.Strings(ids)
		return ids
	}
	bestPair := func() (bi, bj int, sim float64) {
		bi, bj, sim = -1, -1, -1
		for i := 0; i < len(active); i++ {
			for j := i + 1; j < len(active); j++ {
				s := pairSim(all[active[i]], all[active[j]])
				if s > sim {
					sim = s
					bi, bj = i, j
				}
			}
		}
		return
	}
	mergeActive := func(bestI, bestJ int) int {
		ai, aj := active[bestI], active[bestJ]
		merged := append(append([]int{}, all[ai].members...), all[aj].members...)
		sort.Ints(merged)
		depth := all[ai].depth
		if all[aj].depth > depth {
			depth = all[aj].depth
		}
		newIdx := len(all)
		all = append(all, aggCluster{members: merged, parent: -1, depth: depth + 1})
		all[ai].parent = newIdx
		all[aj].parent = newIdx
		active[bestI] = newIdx
		active = append(active[:bestJ], active[bestJ+1:]...)
		return newIdx
	}

	// Phase 1: hard cut.
	for len(active) >= 2 {
		bi, bj, sim := bestPair()
		if bi < 0 || sim < ClusterMergeThreshold {
			break
		}
		// bestJ must be > bestI for slice delete — ensure order
		if bj < bi {
			bi, bj = bj, bi
		}
		mergeActive(bi, bj)
	}

	type themeRow struct {
		id      string
		label   string
		summary string
		repos   []string
		parent  string
		depth   int
		first   string
		last    string
		cent    map[string]float64
		members []int
		isLeaf  bool
	}
	themes := map[string]*themeRow{}
	leafOf := make([]string, n)      // primary theme per doc
	clusterTheme := map[int]string{} // active cluster idx → theme id

	makeThemeFromCluster := func(c aggCluster, isLeaf bool) *themeRow {
		ids := memberSegIDs(c)
		tid := themeIDFromMembers(ids)
		label, summary := docs[c.members[0]].label, docs[c.members[0]].summary
		repos := map[string]struct{}{}
		first, last := docs[c.members[0]].firstTS, docs[c.members[0]].lastTS
		for _, mi := range c.members {
			d := docs[mi]
			if len(d.label) > len(label) {
				label = d.label
			}
			if len(d.summary) > len(summary) {
				summary = d.summary
			}
			if d.repo != "" {
				repos[d.repo] = struct{}{}
			}
			if d.firstTS != "" && (first == "" || d.firstTS < first) {
				first = d.firstTS
			}
			if d.lastTS != "" && (last == "" || d.lastTS > last) {
				last = d.lastTS
			}
		}
		if label == "" {
			label = "theme"
		}
		return &themeRow{
			id: tid, label: label, summary: summary, repos: sortedKeys(repos),
			depth: 0, first: first, last: last, cent: centroid(c),
			members: append([]int{}, c.members...), isLeaf: isLeaf,
		}
	}

	// Leaf themes at the cut (primary membership).
	for _, ci := range active {
		tw := makeThemeFromCluster(all[ci], true)
		themes[tw.id] = tw
		clusterTheme[ci] = tw.id
		for _, mi := range all[ci].members {
			leafOf[mi] = tw.id
		}
	}

	// Phase 2: continue merges to a single root for dendrogram parents.
	// Even weak pairs are merged so parent_theme_id is always navigable
	// when ≥2 leaf themes exist.
	for len(active) >= 2 {
		bi, bj, _ := bestPair()
		if bi < 0 {
			break
		}
		if bj < bi {
			bi, bj = bj, bi
		}
		// Capture child theme ids before merge mutates active.
		childAI, childAJ := active[bi], active[bj]
		tidA := clusterTheme[childAI]
		tidB := clusterTheme[childAJ]
		newIdx := mergeActive(bi, bj)
		parentTW := makeThemeFromCluster(all[newIdx], false)
		parentTW.label = "merged-theme"
		parentTW.summary = fmt.Sprintf("dendrogram merge of %d segments", len(all[newIdx].members))
		// Parent depth = 1 + max(child depths)
		dA, dB := 0, 0
		if tw, ok := themes[tidA]; ok {
			dA = tw.depth
		}
		if tw, ok := themes[tidB]; ok {
			dB = tw.depth
		}
		depth := dA
		if dB > depth {
			depth = dB
		}
		parentTW.depth = depth + 1
		// Children point at parent; child depth stays (distance from leaf).
		// Design: depth is dendrogram depth — leaves 0, parents higher.
		if tw, ok := themes[tidA]; ok {
			tw.parent = parentTW.id
			// leaf keeps depth 0; intermediate nodes already have depth
		}
		if tw, ok := themes[tidB]; ok {
			tw.parent = parentTW.id
		}
		themes[parentTW.id] = parentTW
		clusterTheme[newIdx] = parentTW.id
	}

	// Recompute leaf depths as distance-to-root for navigability consistency:
	// leaf depth = 0; after parents set, set depth = parent.depth - 1 walking down.
	// Already: leaves 0, parents increasing. Ensure intermediate child depths
	// that are themselves parents stay correct (they got depth when created).

	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM theme_members WHERE doc_kind = 'segment'`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM themes WHERE id LIKE 'theme_%'`); err != nil {
		return err
	}

	// Parents (higher depth) before children.
	ordered := make([]*themeRow, 0, len(themes))
	for _, tw := range themes {
		ordered = append(ordered, tw)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].depth != ordered[j].depth {
			return ordered[i].depth > ordered[j].depth
		}
		return ordered[i].id < ordered[j].id
	})

	for _, tw := range ordered {
		reposJSON := "[]"
		if len(tw.repos) > 0 {
			parts := make([]string, len(tw.repos))
			for i, r := range tw.repos {
				parts[i] = fmt.Sprintf("%q", r)
			}
			reposJSON = "[" + strings.Join(parts, ",") + "]"
		}
		var parent any
		if tw.parent != "" {
			parent = tw.parent
		}
		if _, err := tx.Exec(`
			INSERT INTO themes (id, label, summary, weight, repos, parent_theme_id, depth, first_seen, last_touched, computed_at)
			VALUES (?, ?, ?, 0.9, ?, ?, ?, ?, ?, ?)
		`, tw.id, tw.label, tw.summary, reposJSON, parent, tw.depth, tw.first, tw.last, now); err != nil {
			return fmt.Errorf("insert theme %s: %w", tw.id, err)
		}
	}

	for mi, tid := range leafOf {
		if tid == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO theme_members (theme_id, doc_kind, entity_id, membership_kind, similarity)
			VALUES (?, 'segment', ?, 'primary', 1.0)
		`, tid, docs[mi].id); err != nil {
			return err
		}
	}

	leafThemes := map[string]*themeRow{}
	for _, tid := range leafOf {
		if tid == "" {
			continue
		}
		if tw, ok := themes[tid]; ok && tw.isLeaf {
			leafThemes[tid] = tw
		}
	}
	for mi, primary := range leafOf {
		if primary == "" {
			continue
		}
		for tid, tw := range leafThemes {
			if tid == primary {
				continue
			}
			sim := cosine(docs[mi].vec, tw.cent)
			if sim < SecondaryThemeThreshold {
				continue
			}
			if _, err := tx.Exec(`
				INSERT INTO theme_members (theme_id, doc_kind, entity_id, membership_kind, similarity)
				VALUES (?, 'segment', ?, 'secondary', ?)
				ON CONFLICT(theme_id, doc_kind, entity_id) DO UPDATE SET
					membership_kind = 'secondary',
					similarity = excluded.similarity
			`, tid, docs[mi].id, sim); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

func themeIDFromMembers(sortedSegIDs []string) string {
	sum := sha1.Sum([]byte("theme|" + strings.Join(sortedSegIDs, ",")))
	return "theme_" + hex.EncodeToString(sum[:])[:12]
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func tfVector(text string) map[string]float64 {
	v := map[string]float64{}
	for _, tok := range tokenize(text) {
		v[tok]++
	}
	var norm float64
	for _, c := range v {
		norm += c * c
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return v
	}
	for t := range v {
		v[t] /= norm
	}
	return v
}

func tokenize(text string) []string {
	text = strings.ToLower(text)
	var b strings.Builder
	var out []string
	flush := func() {
		if b.Len() == 0 {
			return
		}
		w := b.String()
		b.Reset()
		if len(w) < 2 || isStop(w) {
			return
		}
		out = append(out, w)
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func isStop(w string) bool {
	switch w {
	case "the", "a", "an", "and", "or", "to", "of", "in", "on", "for", "is", "it", "that", "this", "with", "as", "be", "at", "by", "from":
		return true
	}
	return false
}

func cosine(a, b map[string]float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	var dot float64
	for t, va := range a {
		if vb, ok := b[t]; ok {
			dot += va * vb
		}
	}
	if dot < 0 {
		return 0
	}
	if dot > 1 {
		return 1
	}
	return dot
}
