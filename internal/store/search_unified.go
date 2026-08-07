// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Unified cross-corpus search (🎯T144).
//
// One search spans the registered corpora and returns typed hits, so the
// per-corpus search tools are subsumed rather than deleted. Ranking is
// by calibrated quantile (see calibration.go), with rank fusion as the
// degraded path when a corpus has no trustworthy distribution.

// UnifiedHit is one result, labelled with the corpus it came from.
type UnifiedHit struct {
	Kind  string `json:"kind"`
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Meta  string `json:"meta,omitempty"`
	TS    string `json:"timestamp,omitempty"`

	// Score is the value hits were ordered by: a calibrated quantile in
	// [0,1] under the primary path, or a fusion score under the
	// degraded one. Ranking is the field's meaning; Ranking says which.
	Score   float64 `json:"score"`
	Ranking string  `json:"ranking"`

	// SegmentLabel/SegmentSummary enrich a message hit with the topic
	// span that encloses it (🎯T144): segmentation exists to make search
	// better, so its output belongs on the result rather than behind a
	// second tool.
	SegmentLabel   string `json:"segment_label,omitempty"`
	SegmentSummary string `json:"segment_summary,omitempty"`
}

// UnifiedSearchResult carries the hits plus the honesty fields: which
// corpora were searched, and where ranking had to degrade.
type UnifiedSearchResult struct {
	Hits []UnifiedHit `json:"hits"`
	// Corpora searched, in registry order.
	Corpora []string `json:"corpora"`
	// Degraded names corpora whose calibration was missing or stale,
	// with the reason. Their hits were fused by rank instead of
	// calibrated, which is coarser — reporting it beats a merged list
	// that silently mixes two ranking methods.
	Degraded map[string]string `json:"degraded,omitempty"`
}

// unifiedFetchPerCorpus is how deep each corpus is probed before
// merging. Over-fetching lets a corpus contribute several high-quantile
// hits rather than exactly one.
const unifiedFetchPerCorpus = 50

// rrfK damps the contribution of a corpus's head under the degraded
// path. The conventional 60: large enough that rank 1 and rank 5 are
// not wildly apart, which is the point — fusion is quality-blind, so it
// should not assert that a corpus's #1 is excellent.
const rrfK = 60.0

// UnifiedSearch queries every corpus in scope and merges the results.
//
// kinds selects corpora by their registry kind; empty means the default
// set. Ranking is by calibrated quantile where a corpus has a fresh
// distribution, and by reciprocal rank fusion where it does not —
// never by raw BM25 comparison, which is not meaningful across indexes
// with different avgdl.
func (s *Store) UnifiedSearch(query string, kinds []string, limit int, now time.Time) (*UnifiedSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	specs, err := resolveCorpora(kinds)
	if err != nil {
		return nil, err
	}
	cals, err := s.LoadCalibrations()
	if err != nil {
		return nil, err
	}

	ftsQuery := relaxQuery(query)
	out := &UnifiedSearchResult{Degraded: map[string]string{}}
	var all []UnifiedHit

	for _, spec := range specs {
		out.Corpora = append(out.Corpora, spec.kind)

		type scored struct {
			id  int64
			mag float64
		}
		//nolint:gosec // table name comes from the internal corpus registry
		rows, qerr := s.readDB.Query(
			`SELECT rowid, rank FROM `+spec.fts+` WHERE `+spec.fts+` MATCH ? ORDER BY rank LIMIT ?`,
			ftsQuery, unifiedFetchPerCorpus)
		if qerr != nil {
			// A corpus that cannot be queried must not fail the whole
			// search — the other corpora still have answers.
			out.Degraded[spec.kind] = "query failed: " + qerr.Error()
			continue
		}
		var raw []scored
		for rows.Next() {
			var id int64
			var rank float64
			if err := rows.Scan(&id, &rank); err == nil {
				raw = append(raw, scored{id: id, mag: -rank})
			}
		}
		rows.Close()
		if len(raw) == 0 {
			continue
		}

		docs := 0
		//nolint:gosec // table name comes from the internal corpus registry
		_ = s.readDB.QueryRow(`SELECT COUNT(*) FROM ` + spec.source).Scan(&docs)

		cal := cals[spec.kind]
		stale, why := cal.Stale(now, docs)
		if stale {
			out.Degraded[spec.kind] = why
		}

		for i, r := range raw {
			h := UnifiedHit{Kind: spec.kind, ID: r.id}
			if stale {
				// Degraded: rank fusion. Quality-blind by construction,
				// so it must not be presented as a quantile.
				h.Score = 1.0 / (rrfK + float64(i+1))
				h.Ranking = "fusion"
			} else {
				h.Score = cal.Quantile(r.mag)
				h.Ranking = "calibrated"
			}
			all = append(all, h)
		}
	}

	// Fusion scores and quantiles are not on one scale, so sorting them
	// together would reintroduce exactly the incomparability this exists
	// to remove. Calibrated hits rank above fused ones, each group
	// internally ordered; the Degraded map says which corpora landed in
	// the second group and why.
	sort.SliceStable(all, func(i, j int) bool {
		ci := all[i].Ranking == "calibrated"
		cj := all[j].Ranking == "calibrated"
		if ci != cj {
			return ci
		}
		return all[i].Score > all[j].Score
	})
	if len(all) > limit {
		all = all[:limit]
	}
	if err := s.hydrateHits(all); err != nil {
		return nil, err
	}
	s.enrichWithSegments(all)
	out.Hits = all
	if len(out.Degraded) == 0 {
		out.Degraded = nil
	}
	return out, nil
}

// resolveCorpora maps requested kinds to specs, defaulting to the
// registry's default set.
func resolveCorpora(kinds []string) ([]corpusSpec, error) {
	if len(kinds) == 0 {
		var out []corpusSpec
		for _, c := range searchCorpora() {
			if c.inDefault {
				out = append(out, c)
			}
		}
		return out, nil
	}
	var out []corpusSpec
	for _, k := range kinds {
		spec, ok := corpusByKind(strings.TrimSpace(k))
		if !ok {
			var valid []string
			for _, c := range searchCorpora() {
				valid = append(valid, c.kind)
			}
			return nil, fmt.Errorf("unknown kind %q. Valid kinds: %s",
				k, strings.Join(valid, ", "))
		}
		out = append(out, spec)
	}
	return out, nil
}

// hydrateHits fills in display fields, one query per corpus rather than
// one per hit.
func (s *Store) hydrateHits(hits []UnifiedHit) error {
	byKind := map[string][]int64{}
	for _, h := range hits {
		byKind[h.Kind] = append(byKind[h.Kind], h.ID)
	}
	type fields struct{ title, body, meta, ts string }
	resolved := map[string]map[int64]fields{}

	for kind, ids := range byKind {
		spec, ok := corpusByKind(kind)
		if !ok {
			continue
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		//nolint:gosec // SQL comes from the internal registry; ids are bound
		rows, err := s.readDB.Query(spec.selectSQL+" WHERE id IN ("+ph+")", args...)
		if err != nil {
			continue
		}
		m := map[int64]fields{}
		for rows.Next() {
			var id int64
			var f fields
			if err := rows.Scan(&id, &f.title, &f.body, &f.meta, &f.ts); err == nil {
				m[id] = f
			}
		}
		rows.Close()
		resolved[kind] = m
	}

	for i := range hits {
		f, ok := resolved[hits[i].Kind][hits[i].ID]
		if !ok {
			continue
		}
		hits[i].Title = truncateField(f.title, 200)
		hits[i].Body = truncateField(f.body, 600)
		hits[i].Meta = f.meta
		hits[i].TS = f.ts
	}
	return nil
}

// enrichWithSegments attaches the enclosing topic span to message hits.
//
// This is the 🎯T144 framing made concrete: segmentation is a richer
// search signal, not a separate domain, so a message hit carries the
// span that contains it rather than requiring a second tool call.
func (s *Store) enrichWithSegments(hits []UnifiedHit) {
	for i := range hits {
		if hits[i].Kind != "message" {
			continue
		}
		var label, summary string
		err := s.readDB.QueryRow(`
			SELECT COALESCE(label,''), COALESCE(summary,'')
			FROM topic_segments
			WHERE from_msg_id <= ? AND to_msg_id >= ?
			ORDER BY (to_msg_id - from_msg_id) ASC
			LIMIT 1`, hits[i].ID, hits[i].ID).Scan(&label, &summary)
		if err != nil {
			continue
		}
		hits[i].SegmentLabel = label
		hits[i].SegmentSummary = truncateField(summary, 300)
	}
}

// truncateField bounds a display field without splitting a rune.
func truncateField(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}
