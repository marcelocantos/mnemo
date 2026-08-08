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
	// Evidence is the shrinkage weight behind Score, in [0,1]: how much
	// of this corpus's own distribution was believed. Disclosed because
	// "quantile 0.97" from 30 samples and from 1000 are different claims.
	Evidence float64 `json:"evidence"`

	// SegmentLabel/SegmentSummary enrich a message hit with the topic
	// span that encloses it (🎯T144): segmentation exists to make search
	// better, so its output belongs on the result rather than behind a
	// second tool.
	SegmentLabel   string `json:"segment_label,omitempty"`
	SegmentSummary string `json:"segment_summary,omitempty"`

	// Message carries the full pre-🎯T144 message result — surrounding
	// context, session, role — for hits from the message corpus. Agents
	// that already parse mnemo_search output keep the shape they know;
	// the typed wrapper is additive.
	Message *SearchResult `json:"message,omitempty"`
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
	// Ranking names the scale the hits were ordered on: "calibrated" when
	// every contributing corpus had a usable distribution, "rank_fusion"
	// when at least one did not and the whole merge dropped to RRF. It is
	// a property of the result rather than of a hit because the two scales
	// cannot be mixed within one ordering.
	Ranking string `json:"ranking,omitempty"`
}

// rrfK is the reciprocal-rank-fusion damping constant. 60 is the value
// from Cormack et al., and the reason it is not tuned here is that its
// effect is almost entirely on how sharply the head decays; the property
// this fallback needs — every corpus's rank-N hit scoring equally — holds
// for any k.
const rrfK = 60.0

// fusionEvidenceFloor is the shrinkage weight below which a corpus's
// quantiles are treated as unusable for cross-corpus comparison, sending
// the whole merge to rank fusion. It is deliberately the same 0.5 that
// rankingLabel already calls the boundary between "weak" and
// "calibrated": a hit the product describes to the user as weakly
// evidenced should not be silently ranked as though it were not.
const fusionEvidenceFloor = 0.5

// rrfScore maps a zero-based within-corpus rank to its fusion score.
// Identical across corpora by construction: that is what lets an
// unmeasured corpus compete at all.
func rrfScore(rank int) float64 {
	return 1.0 / (rrfK + float64(rank) + 1.0)
}

// unifiedFetchPerCorpus is how deep each corpus is probed before
// merging. Over-fetching lets a corpus contribute several high-quantile
// hits rather than exactly one.
const unifiedFetchPerCorpus = 50

// rankingLabel describes how much a hit's score rests on its corpus's
// own measured distribution versus the neutral prior. Reported per hit
// so a reader can tell a well-evidenced ranking from a guess.
func rankingLabel(evidence float64) string {
	switch {
	case evidence == 0:
		return "neutral"
	case evidence < 0.5:
		return "weak"
	default:
		return "calibrated"
	}
}

// UnifiedOpts carries the message-corpus filters that predate 🎯T144.
//
// mnemo_search is 55% of all agent calls, and its session-type, repo and
// context-expansion behaviour is the most exercised path in the product.
// Unified search must not quietly drop it: when the message corpus is in
// scope, it goes through the same Search() those calls already use, and
// only its RANKING changes. Everything else about a message hit is what
// it was.
type UnifiedOpts struct {
	Kinds           []string
	Limit           int
	SessionType     string
	Repo            string
	ContextBefore   int
	ContextAfter    int
	SubstantiveOnly bool
}

// UnifiedSearch queries every corpus in scope and merges the results.
//
// Thin wrapper preserving the simple form; see UnifiedSearchOpts.
func (s *Store) UnifiedSearch(query string, kinds []string, limit int, now time.Time) (*UnifiedSearchResult, error) {
	return s.UnifiedSearchOpts(query, UnifiedOpts{
		Kinds: kinds, Limit: limit, SessionType: "all", SubstantiveOnly: true,
	}, now)
}

// UnifiedSearchOpts is the full form.
//
// kinds selects corpora by their registry kind; empty means the default
// set. Ranking is by calibrated quantile where a corpus has a fresh
// distribution, and by reciprocal rank fusion where it does not —
// never by raw BM25 comparison, which is not meaningful across indexes
// with different avgdl.
func (s *Store) UnifiedSearchOpts(query string, opts UnifiedOpts, now time.Time) (*UnifiedSearchResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	specs, err := resolveCorpora(opts.Kinds)
	if err != nil {
		return nil, err
	}
	cals, err := s.LoadCalibrations()
	if err != nil {
		return nil, err
	}

	ftsQuery := relaxQuery(query)
	// Per-term normalisation, matching how the distribution was sampled.
	// See queryTermCount: without it a hit's quantile tracks the query's
	// width rather than the hit's quality.
	terms := float64(queryTermCount(query))
	out := &UnifiedSearchResult{Degraded: map[string]string{}}
	var all []UnifiedHit

	// Scoring happens in a second pass because the choice of scale is a
	// property of the whole query, not of one corpus: see the fuse
	// decision below.
	type corpusHits struct {
		kind     string
		ids      []int64
		msgs     []*SearchResult
		mags     []float64
		cal      *Calibration
		evidence float64
	}
	var collected []corpusHits

	for _, spec := range specs {
		out.Corpora = append(out.Corpora, spec.kind)

		type scored struct {
			id  int64
			mag float64
			msg *SearchResult
		}
		var raw []scored

		if spec.kind == "message" {
			// The message corpus keeps its pre-🎯T144 path: session-type
			// and repo filtering, context expansion, noise handling. Only
			// its ranking is new.
			msgs, merr := s.Search(query, unifiedFetchPerCorpus, opts.SessionType,
				opts.Repo, opts.ContextBefore, opts.ContextAfter, opts.SubstantiveOnly)
			if merr != nil {
				out.Degraded[spec.kind] = "query failed: " + merr.Error()
				continue
			}
			for i := range msgs {
				m := msgs[i]
				raw = append(raw, scored{id: int64(m.MessageID), mag: -m.Rank / terms, msg: &m})
			}
		} else {
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
			for rows.Next() {
				var id int64
				var rank float64
				if err := rows.Scan(&id, &rank); err == nil {
					raw = append(raw, scored{id: id, mag: -rank / terms})
				}
			}
			rows.Close()
		}
		if len(raw) == 0 {
			continue
		}

		// Staleness is checked on AGE only, not on corpus growth.
		//
		// The growth check needs a live document count, and
		// `SELECT COUNT(*) FROM messages` costs ~120ms on 2.97M rows —
		// per search, on the tool carrying 55% of all agent calls. That
		// made a unified search 137ms against 8ms for the old
		// message-only path, and the cost was entirely this count:
		// searching 12 corpora measured the same as searching 1.
		//
		// The reconciler already owns growth detection and ticks every
		// minute, so re-deriving it here bought at most 60 seconds of
		// freshness for a full-table scan on every query. Passing 0
		// skips the growth branch (Calibration.Stale guards on
		// currentDocs > 0) and leaves age as the search-path check.
		cal := cals[spec.kind]
		stale, why := cal.Stale(now, 0)
		if stale {
			out.Degraded[spec.kind] = why
		}

		// A stale distribution is not used as evidence: it is dropped to
		// zero-evidence, so its corpus scores at the neutral prior rather
		// than by numbers that no longer describe it.
		effective := cal
		if stale {
			effective = nil
		}
		ch := corpusHits{
			kind:     spec.kind,
			cal:      effective,
			evidence: effective.Evidence(),
		}
		for _, r := range raw {
			ch.ids = append(ch.ids, r.id)
			ch.msgs = append(ch.msgs, r.msg)
			ch.mags = append(ch.mags, r.mag)
		}
		collected = append(collected, ch)
	}

	// THE SCALE IS A PROPERTY OF THE QUERY, NOT OF A CORPUS.
	//
	// A calibrated quantile answers "how good is this hit relative to
	// typical hits in its own corpus". A corpus with no distribution
	// cannot answer that, and the previous code gave it the neutral prior
	// instead — which reads as a sensible default and is not one. A flat
	// 0.5 is not a weak claim, it is a claim of exact medianness, and it
	// loses deterministically to any corpus holding `limit` hits above its
	// own median. Measured on the oracle fixture: five matching documents
	// all at 0.500 against two hundred messages at 0.82-0.85, so the
	// uncalibrated corpus was not ranked low, it was truncated out of
	// existence. That is the exile the shrinkage was meant to prevent.
	//
	// So when any contributing corpus lacks a usable distribution, the
	// whole merge drops to reciprocal rank fusion. Fusing needs no
	// calibration and puts every corpus on one scale, which is the point:
	// mixing a quantile with an RRF score would be the same category error
	// as comparing raw BM25 across indexes, the thing 🎯T144 exists to
	// avoid. The cost is that a fused ranking is quality-blind — a
	// corpus's #1 counts as a #1 however poor it is — which is exactly why
	// it is the fallback and not the default.
	// The trigger is thin evidence, not absent evidence. A corpus scoring
	// at evidence 0.33 is not meaningfully better placed on the quantile
	// axis than one at 0: two thirds of every score it reports is the
	// prior rather than measurement, and shrinkage caps it well below a
	// rich corpus's range. Measured on the oracle fixture, a ten-document
	// corpus tops out at 0.667 while the three-hundred-document corpus's
	// twenty-fifth hit still scores 0.742 — so it is not ranked low, it is
	// unreachable, which is the same exile in a politer form.
	//
	// The floor reuses rankingLabel's existing "weak" boundary rather than
	// introducing a second, differently-drawn line for the same judgement.
	fuse := false
	for _, ch := range collected {
		if ch.evidence < fusionEvidenceFloor {
			fuse = true
			break
		}
	}
	out.Ranking = "calibrated"
	if fuse {
		out.Ranking = "rank_fusion"
	}

	for _, ch := range collected {
		for i := range ch.ids {
			score := ch.cal.Score(ch.mags[i], i)
			if fuse {
				score = rrfScore(i)
			}
			all = append(all, UnifiedHit{
				Kind:     ch.kind,
				ID:       ch.ids[i],
				Message:  ch.msgs[i],
				Score:    score,
				Evidence: ch.evidence,
				// Ranking stays a statement about THIS corpus's evidence,
				// not about the merge method — a reader still needs to know
				// that a fused hit came from an unmeasured corpus. The
				// merge method is reported once, on the result.
				Ranking: rankingLabel(ch.evidence),
			})
		}
	}

	// Ties are broken by corpus order rather than arbitrarily: under
	// fusion every corpus's rank-N hit scores identically, and a stable
	// sort keeps the registry's ordering, so the merge is deterministic
	// across runs and platforms.
	sort.SliceStable(all, func(i, j int) bool {
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
		rows, err := s.readDB.Query(spec.selectSQL+" WHERE rowid IN ("+ph+")", args...)
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
