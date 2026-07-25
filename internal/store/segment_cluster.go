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

// ClusterMergeThreshold: single-link merge while nearest pair ≥ this.
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
// sealed topic segments. Multi-member themes share one theme_id (stable
// hash of sorted member segment ids). Dendrogram parents populate
// themes.parent_theme_id/depth. Each segment gets exactly one primary
// membership; secondary rows are written only when cosine to another
// leaf-theme centroid ≥ SecondaryThemeThreshold.
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
		// re-L2
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

	for len(active) >= 2 {
		bestI, bestJ := -1, -1
		bestSim := -1.0
		for i := 0; i < len(active); i++ {
			for j := i + 1; j < len(active); j++ {
				sim := pairSim(all[active[i]], all[active[j]])
				if sim > bestSim {
					bestSim = sim
					bestI, bestJ = i, j
				}
			}
		}
		if bestSim < ClusterMergeThreshold {
			break
		}
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
	}
	themes := map[string]*themeRow{}
	leafOf := make([]string, n) // primary theme per doc index

	// Leaf themes = active cut clusters.
	for _, ci := range active {
		c := all[ci]
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
			leafOf[mi] = tid
		}
		repoList := sortedKeys(repos)
		themes[tid] = &themeRow{
			id: tid, label: label, summary: summary, repos: repoList,
			depth: 0, first: first, last: last, cent: centroid(c), members: append([]int{}, c.members...),
		}
	}

	// Dendrogram parents: climb from each active leaf; when a merge node
	// contains multiple leaf themes, emit a parent theme for that node.
	for _, ci := range active {
		leafTID := leafOf[all[ci].members[0]]
		cur := all[ci].parent
		childTID := leafTID
		depth := 0
		for cur >= 0 {
			// Leaf themes represented under this merge node.
			leafSet := map[string]struct{}{}
			for _, mi := range all[cur].members {
				if lt := leafOf[mi]; lt != "" {
					leafSet[lt] = struct{}{}
				}
			}
			if len(leafSet) >= 2 {
				ids := memberSegIDs(all[cur])
				pid := themeIDFromMembers(ids)
				if _, ok := themes[pid]; !ok {
					themes[pid] = &themeRow{
						id:      pid,
						label:   "merged-theme",
						summary: fmt.Sprintf("dendrogram merge of %d segments", len(ids)),
						depth:   depth + 1,
						cent:    centroid(all[cur]),
						members: append([]int{}, all[cur].members...),
					}
				}
				if tw, ok := themes[childTID]; ok && tw.parent == "" {
					tw.parent = pid
					tw.depth = depth
				}
				childTID = pid
				depth++
			}
			cur = all[cur].parent
		}
	}

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

	// Insert parents before children: higher depth first.
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
		label := tw.label
		if label == "" {
			label = "theme"
		}
		if _, err := tx.Exec(`
			INSERT INTO themes (id, label, summary, weight, repos, parent_theme_id, depth, first_seen, last_touched, computed_at)
			VALUES (?, ?, ?, 0.9, ?, ?, ?, ?, ?, ?)
		`, tw.id, label, tw.summary, reposJSON, parent, tw.depth, tw.first, tw.last, now); err != nil {
			return fmt.Errorf("insert theme %s: %w", tw.id, err)
		}
	}

	// Exactly one primary per segment.
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

	// Leaf themes for secondary pass.
	leafThemes := map[string]*themeRow{}
	for _, tid := range leafOf {
		if tid == "" {
			continue
		}
		if tw, ok := themes[tid]; ok {
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
