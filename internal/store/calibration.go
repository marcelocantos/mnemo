// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Cross-corpus ranking calibration (🎯T144).
//
// THE PROBLEM. BM25 scores are not comparable across FTS indexes. The
// length-normalisation term is b·|D|/avgdl and avgdl is PER-INDEX, so
// each corpus scores its documents against its own average length. That
// is correct within a corpus and meaningless between two: a 10-word
// commit subject and a 500-word message sit on different scales, and
// merging by raw score lets short-document corpora monopolise the head.
//
// WHY NOT THE OBVIOUS FIXES.
//
//   - Raw comparison: wrong for the avgdl reason above.
//   - One unified FTS index: worse. It would normalise every document
//     against a single global avgdl dominated by ~3M messages, so short
//     documents get an inflated score — introducing a length bias that
//     does not currently exist and hiding it behind one authoritative
//     number. (Also impossible without duplicating text: all 22 indexes
//     are external-content, which binds one index to one table.)
//   - Tuning BM25: SQLite's FTS5 hardcodes k1=1.2 and b=0.75. The only
//     knob is per-column weights. There is no b to turn.
//   - Min-max or z-score over the returned top-N: the tempting one, and
//     the worst. It rescales against the HEAD of each result window
//     rather than the corpus, so a corpus whose best hit is poor still
//     maps to 1.0 — destroying exactly the "how good is the best match"
//     signal that fusion needs.
//
// WHAT THIS DOES. Probe each corpus with terms drawn from its own
// content, record the resulting score distribution, and store its
// quantile boundaries. At query time a live score maps to its quantile
// within its own corpus ("97th percentile for commits"), and quantiles
// compete. The question that answers — "how good is this hit relative to
// typical hits in its own corpus" — is the one cross-corpus
// interleaving actually needs.
//
// Scores are stored as MAGNITUDES. FTS5's rank is bm25(), which is
// negative with more-negative meaning a better match; magnitude = -rank,
// so larger is better and the quantile array ascends.

// calibrationQuantiles is the number of boundary points stored per
// corpus (p0..p100 inclusive at 1% intervals). Fine enough to separate
// a 97th-percentile hit from a 99th, coarse enough to stay small.
const calibrationQuantiles = 101

// calibrationProbes is how many probe terms are drawn per corpus, and
// calibrationTopK how many hits each probe contributes. Their product
// bounds the sample size.
const (
	calibrationProbes = 40
	calibrationTopK   = 25
)

// calibrationMinSamples is the floor below which a distribution is not
// evidence. Deliberately separate from calibrationQuantiles: the
// boundary array is interpolated, so it does not need one raw sample per
// point, and requiring that conflates "how finely we report a quantile"
// with "how much data stands behind it". A homogeneous corpus — one
// where every document uses the same handful of words — yields few
// distinct probe terms, and demanding 101 samples from it would refuse
// to calibrate a corpus that is perfectly calibratable at 30.
const calibrationMinSamples = 30

// calibrationStaleAfter is when a stored distribution stops being
// trusted on age alone. Corpora grow continuously; a distribution is
// not wrong the moment it ages, but past this it is not evidence.
const calibrationStaleAfter = 7 * 24 * time.Hour

// calibrationGrowthTolerance is the fractional change in document count
// that invalidates a distribution regardless of age. A corpus that has
// grown 50% has a materially different score profile.
const calibrationGrowthTolerance = 0.5

// Calibration is one corpus's stored score distribution.
type Calibration struct {
	Corpus     string    `json:"corpus"`
	Quantiles  []float64 `json:"quantiles"`
	SampleSize int       `json:"sample_size"`
	DocCount   int       `json:"doc_count"`
	ComputedAt time.Time `json:"computed_at"`
}

// Quantile maps a score magnitude to its position in this corpus's
// distribution, in [0,1]. A magnitude at or above the top boundary is
// 1.0; at or below the bottom, 0.0.
func (c *Calibration) Quantile(magnitude float64) float64 {
	n := len(c.Quantiles)
	if n < 2 {
		return 0.5 // no usable distribution — treat as median
	}
	// Boundaries ascend, so the index of the first boundary exceeding
	// the magnitude is its rank in the distribution.
	i := sort.SearchFloat64s(c.Quantiles, magnitude)
	if i <= 0 {
		return 0
	}
	if i >= n {
		return 1
	}
	// Linear interpolation between adjacent boundaries so hits inside a
	// bucket are ordered rather than tied.
	lo, hi := c.Quantiles[i-1], c.Quantiles[i]
	frac := 0.0
	if hi > lo {
		frac = (magnitude - lo) / (hi - lo)
	}
	return (float64(i-1) + frac) / float64(n-1)
}

// Stale reports whether this calibration should still be trusted, and
// why not when it should not. Staleness is legible rather than inferred
// (🎯T144): a distribution sampled when a corpus was a fraction of its
// current size mis-maps every score, and nothing about the resulting
// ordering would look wrong.
func (c *Calibration) Stale(now time.Time, currentDocs int) (bool, string) {
	if c == nil || len(c.Quantiles) < 2 {
		return true, "no calibration"
	}
	if age := now.Sub(c.ComputedAt); age > calibrationStaleAfter {
		return true, fmt.Sprintf("calibration is %s old", age.Round(time.Hour))
	}
	if c.DocCount > 0 && currentDocs > 0 {
		growth := math.Abs(float64(currentDocs-c.DocCount)) / float64(c.DocCount)
		if growth > calibrationGrowthTolerance {
			return true, fmt.Sprintf("corpus grew %.0f%% since calibration", growth*100)
		}
	}
	return false, ""
}

// LoadCalibrations returns every stored calibration, keyed by corpus.
func (s *Store) LoadCalibrations() (map[string]*Calibration, error) {
	rows, err := s.readDB.Query(
		`SELECT corpus, quantiles, sample_size, doc_count, computed_at FROM search_calibration`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]*Calibration{}
	for rows.Next() {
		var c Calibration
		var qs, ts string
		if err := rows.Scan(&c.Corpus, &qs, &c.SampleSize, &c.DocCount, &ts); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(qs), &c.Quantiles); err != nil {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, ts); err == nil {
				c.ComputedAt = t
				break
			}
		}
		out[c.Corpus] = &c
	}
	return out, nil
}

// saveCalibration upserts one corpus's distribution.
func (s *Store) saveCalibration(c *Calibration) error {
	buf, err := json.Marshal(c.Quantiles)
	if err != nil {
		return err
	}
	_, err = s.writeDB.Exec(`
		INSERT INTO search_calibration (corpus, quantiles, sample_size, doc_count, computed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(corpus) DO UPDATE SET
			quantiles = excluded.quantiles,
			sample_size = excluded.sample_size,
			doc_count = excluded.doc_count,
			computed_at = excluded.computed_at
	`, c.Corpus, string(buf), c.SampleSize, c.DocCount, c.ComputedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// CalibrateCorpus samples one corpus and stores its distribution.
//
// Probes are drawn from the corpus's own content rather than from a
// fixed vocabulary: a corpus's score profile is a property of its own
// term distribution, and borrowing another corpus's terms would measure
// the wrong thing.
func (s *Store) CalibrateCorpus(ctx context.Context, spec corpusSpec, now time.Time) (*Calibration, error) {
	docs := 0
	//nolint:gosec // table name comes from the internal corpus registry
	_ = s.readDB.QueryRow(`SELECT COUNT(*) FROM ` + spec.source).Scan(&docs)
	if docs == 0 {
		return nil, fmt.Errorf("corpus %s is empty", spec.kind)
	}

	terms, err := s.sampleProbeTerms(spec, calibrationProbes)
	if err != nil {
		return nil, err
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("corpus %s yielded no probe terms", spec.kind)
	}

	var magnitudes []float64
	for _, term := range terms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		//nolint:gosec // table name comes from the internal corpus registry
		rows, err := s.readDB.Query(
			`SELECT rank FROM `+spec.fts+` WHERE `+spec.fts+` MATCH ? ORDER BY rank LIMIT ?`,
			term, calibrationTopK)
		if err != nil {
			continue // a probe term that FTS rejects is not a failure
		}
		for rows.Next() {
			var rank float64
			if err := rows.Scan(&rank); err == nil {
				magnitudes = append(magnitudes, -rank)
			}
		}
		rows.Close()
	}
	if len(magnitudes) < calibrationMinSamples {
		return nil, fmt.Errorf("corpus %s produced only %d scores, need %d",
			spec.kind, len(magnitudes), calibrationMinSamples)
	}

	sort.Float64s(magnitudes)
	cal := &Calibration{
		Corpus:     spec.kind,
		Quantiles:  extractQuantiles(magnitudes, calibrationQuantiles),
		SampleSize: len(magnitudes),
		DocCount:   docs,
		ComputedAt: now,
	}
	if err := s.saveCalibration(cal); err != nil {
		return nil, err
	}
	return cal, nil
}

// extractQuantiles reduces a sorted sample to n evenly-spaced boundary
// points from p0 to p100.
func extractQuantiles(sorted []float64, n int) []float64 {
	out := make([]float64, n)
	last := len(sorted) - 1
	for i := 0; i < n; i++ {
		pos := float64(i) / float64(n-1) * float64(last)
		lo := int(math.Floor(pos))
		hi := int(math.Ceil(pos))
		if hi > last {
			hi = last
		}
		if lo == hi {
			out[i] = sorted[lo]
			continue
		}
		frac := pos - float64(lo)
		out[i] = sorted[lo] + (sorted[hi]-sorted[lo])*frac
	}
	return out
}

// sampleProbeTerms draws distinct query terms from a corpus's own text.
//
// Terms are taken from the middle of the length distribution: very short
// tokens are stopword-like and match everything, very long ones are
// usually identifiers that match one document. Neither shape produces a
// score distribution that says anything about typical matches.
func (s *Store) sampleProbeTerms(spec corpusSpec, want int) ([]string, error) {
	//nolint:gosec // table/column names come from the internal corpus registry
	rows, err := s.readDB.Query(
		`SELECT `+spec.sampleExpr+` FROM `+spec.source+
			` WHERE `+spec.sampleExpr+` IS NOT NULL AND length(`+spec.sampleExpr+`) > 40`+
			` ORDER BY id DESC LIMIT ?`, want*8)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	var terms []string
	for rows.Next() && len(terms) < want {
		var text string
		if err := rows.Scan(&text); err != nil {
			continue
		}
		for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			if len(tok) < 5 || len(tok) > 14 {
				continue
			}
			tok = strings.ToLower(tok)
			if seen[tok] || !isProbeWord(tok) {
				continue
			}
			seen[tok] = true
			terms = append(terms, tok)
			break // one term per sampled document, so probes spread
		}
	}
	return terms, nil
}

// isProbeWord keeps alphabetic tokens, rejecting anything with digits —
// hashes, ids and timestamps match one row and calibrate nothing.
func isProbeWord(tok string) bool {
	for _, r := range tok {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}
