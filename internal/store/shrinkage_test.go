// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Shrinkage tests (🎯T144, revised).
//
// The first design used a hard trust threshold: below 30 samples a
// corpus was not calibrated at all, and uncalibrated corpora sorted
// beneath every calibrated one. Both ends were wrong. These tests pin
// the continuous replacement at both extremes.

// TestEvidenceScalesWithSampleSize is the weighting itself.
func TestEvidenceScalesWithSampleSize(t *testing.T) {
	q := make([]float64, calibrationQuantiles)
	for i := range q {
		q[i] = float64(i)
	}
	for _, tc := range []struct {
		samples  int
		wantEvid float64
	}{
		{0, 0.00},
		{30, 0.23},
		{100, 0.50},
		{250, 0.71},
		{1000, 0.91},
	} {
		c := &Calibration{Quantiles: q, SampleSize: tc.samples}
		got := c.Evidence()
		if diff := got - tc.wantEvid; diff > 0.02 || diff < -0.02 {
			t.Errorf("Evidence(n=%d) = %.3f, want ~%.2f", tc.samples, got, tc.wantEvid)
		}
	}
	// A nil calibration has no evidence at all.
	var missing *Calibration
	if got := missing.Evidence(); got != 0 {
		t.Errorf("nil calibration has evidence %v, want 0", got)
	}
}

// TestThinEvidenceCannotClaimTheExtremes is the thin-end fix.
//
// A corpus calibrated from a handful of samples must not be able to
// hand a hit a near-1.0 score just for being the best of a tiny set.
// On the live index this was real: `skill` calibrated from 30 samples
// over TEN documents and could claim quantile 1.00.
func TestThinEvidenceCannotClaimTheExtremes(t *testing.T) {
	q := make([]float64, calibrationQuantiles)
	for i := range q {
		q[i] = float64(i)
	}
	thin := &Calibration{Quantiles: q, SampleSize: 30}
	rich := &Calibration{Quantiles: q, SampleSize: 1000}

	// The same top-of-distribution magnitude in both corpora.
	top := 100.0
	thinScore := thin.Score(top, 0)
	richScore := rich.Score(top, 0)

	if thinScore >= richScore {
		t.Errorf("a 30-sample corpus scored %.3f against a 1000-sample corpus's %.3f "+
			"for the same relative match quality; thin evidence must not win",
			thinScore, richScore)
	}
	// And it must stay near the prior rather than reaching the extreme.
	if thinScore > 0.65 {
		t.Errorf("30 samples produced score %.3f — thin evidence should move only "+
			"slightly from the %.1f prior, not claim the top", thinScore, neutralQuantile)
	}
	// Symmetrically at the bottom: thin evidence must not exile a hit.
	if bottom := thin.Score(0, 0); bottom < 0.35 {
		t.Errorf("30 samples produced score %.3f at the bottom of its range; "+
			"thin evidence should not push a hit to the floor either", bottom)
	}
}

// TestNoEvidenceSitsAtThePrior is the zero end: a corpus with no
// distribution scores neutrally and interleaves, rather than being
// ranked beneath every measured corpus.
func TestNoEvidenceSitsAtThePrior(t *testing.T) {
	var missing *Calibration
	got := missing.Score(12345, 0)
	if diff := got - neutralQuantile; diff > 0.001 || diff < -0.001 {
		t.Errorf("no-evidence score = %.4f, want the %.1f prior", got, neutralQuantile)
	}
	// Within-corpus order is still preserved.
	if missing.Score(999, 0) <= missing.Score(999, 1) {
		t.Error("hits from an unmeasured corpus must still order by their own rank")
	}
}

// TestUncalibratedCorpusInterleaves is the end-to-end zero-end oracle:
// a corpus with no calibration must appear among the results alongside
// a well-calibrated one, not be swept beneath it.
func TestUncalibratedCorpusInterleaves(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)
	// A handful of docs, all matching, deliberately left uncalibrated.
	for i := 0; i < 5; i++ {
		mustExec(t, s, `INSERT INTO docs (repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at)
			VALUES ('o/r', ?, 'md', 'watcher design', 'the watcher and its descriptor handling', 'h', 10, '2026-01-01', '2026-01-01')`,
			fmt.Sprintf("d%d.md", i))
	}
	now := time.Now()
	spec, _ := corpusByKind("message")
	if _, err := s.CalibrateCorpus(context.Background(), spec, now); err != nil {
		t.Fatalf("CalibrateCorpus(message): %v", err)
	}

	res, err := s.UnifiedSearch("watcher", []string{"message", "doc"}, 12, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) < 6 {
		t.Fatalf("expected a populated result set, got %d", len(res.Hits))
	}
	var docPos = -1
	for i, h := range res.Hits {
		if h.Kind == "doc" {
			docPos = i
			break
		}
	}
	if docPos < 0 {
		t.Fatal("the uncalibrated corpus returned no hits at all")
	}
	if docPos == len(res.Hits)-1 {
		t.Errorf("the uncalibrated corpus's first hit landed last (position %d of %d); "+
			"no evidence should mean the neutral prior, not exile", docPos, len(res.Hits))
	}
	// And its lack of evidence must be disclosed rather than implied.
	for _, h := range res.Hits {
		if h.Kind == "doc" {
			if h.Ranking != "neutral" {
				t.Errorf("uncalibrated hit labelled %q, want \"neutral\"", h.Ranking)
			}
			if h.Evidence != 0 {
				t.Errorf("uncalibrated hit reports evidence %v, want 0", h.Evidence)
			}
			break
		}
	}
}

// TestTinyCorpusDoesNotTakeTheHead is the thin end end-to-end: a
// ten-document corpus, calibrated from what little it has, must not
// sweep the top of a merged result set against a large well-sampled
// corpus.
func TestTinyCorpusDoesNotTakeTheHead(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 300)
	// The large corpus must contain genuinely STRONG matches, not just
	// mediocre ones. Without them this test asserts the wrong property:
	// a corpus with no evidence sits at the neutral prior, so it
	// correctly outranks hits a measured corpus has itself scored as
	// below-median. "Unknown beats known-poor" is the intended
	// behaviour of an uninformative prior. What must NOT happen is
	// unknown beating known-EXCELLENT.
	for i := 0; i < 10; i++ {
		mustExec(t, s, `INSERT INTO messages
			(session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', 'watcher watcher watcher watcher descriptor exhaustion of the watcher', '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			fmt.Sprintf("strong%d", i))
	}
	// Ten skill docs, every one a strong match for the query term.
	for i := 0; i < 10; i++ {
		mustExec(t, s, `INSERT INTO skills (file_path, name, description, content, updated_at)
			VALUES (?, ?, 'd', 'watcher watcher watcher descriptor handling and more watcher prose', '2026-01-01')`,
			fmt.Sprintf("s%d.md", i), fmt.Sprintf("skill %d", i))
	}
	now := time.Now()
	for _, kind := range []string{"message", "skill"} {
		spec, _ := corpusByKind(kind)
		// Best effort: the tiny corpus may or may not clear the floor —
		// either way it must not dominate.
		_, _ = s.CalibrateCorpus(context.Background(), spec, now)
	}

	// A wide enough window that all three populations can appear: the
	// large corpus's excellent hits, the small corpus's unmeasured ones,
	// and the large corpus's mediocre ones.
	res, err := s.UnifiedSearch("watcher", []string{"message", "skill"}, 25, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) < 20 {
		t.Fatalf("expected a wide result set, got %d", len(res.Hits))
	}
	skills, firstSkill := 0, -1
	for i, h := range res.Hits {
		if h.Kind == "skill" {
			skills++
			if firstSkill < 0 {
				firstSkill = i
			}
		}
	}
	// The precise property: unmeasured lands MID-PACK. Not at the top
	// (that would mean thin evidence beating known-excellent) and not
	// absent (that would be the exile the shrinkage replaced).
	if skills == 0 {
		t.Error("the small corpus vanished entirely; it should interleave, not be exiled")
	}
	if firstSkill == 0 {
		t.Error("a 10-document corpus led the results; thin evidence must not " +
			"outrank a 300-document corpus's strongest hits")
	}
	if skills > 0 && firstSkill > 0 {
		t.Logf("small corpus interleaved from position %d (%d of %d hits)",
			firstSkill, skills, len(res.Hits))
	}
	// Whatever it scores, it must disclose how little is behind it.
	for _, h := range res.Hits {
		if h.Kind == "skill" && h.Ranking == "calibrated" {
			t.Errorf("a 10-document corpus reported %q ranking with evidence %.2f; "+
				"that overstates what its distribution supports", h.Ranking, h.Evidence)
		}
	}
}

// TestRankingLabelsMatchEvidence keeps the reported label honest.
func TestRankingLabelsMatchEvidence(t *testing.T) {
	for _, tc := range []struct {
		evidence float64
		want     string
	}{
		{0, "neutral"},
		{0.1, "weak"},
		{0.49, "weak"},
		{0.5, "calibrated"},
		{0.95, "calibrated"},
	} {
		if got := rankingLabel(tc.evidence); got != tc.want {
			t.Errorf("rankingLabel(%.2f) = %q, want %q", tc.evidence, got, tc.want)
		}
	}
}

// TestStaleCalibrationDropsToNeutral: a distribution that no longer
// describes its corpus must not be used as evidence at all. It falls to
// the prior rather than ranking by numbers that have stopped applying.
func TestStaleCalibrationDropsToNeutral(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)
	past := time.Now().Add(-90 * 24 * time.Hour)
	spec, _ := corpusByKind("message")
	if _, err := s.CalibrateCorpus(context.Background(), spec, past); err != nil {
		t.Fatalf("CalibrateCorpus: %v", err)
	}

	res, err := s.UnifiedSearch("watcher", []string{"message"}, 5, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range res.Hits {
		if h.Ranking != "neutral" {
			t.Errorf("a 90-day-old calibration produced %q ranking; stale evidence "+
				"must drop to the prior, not keep being believed", h.Ranking)
		}
	}
	if len(res.Degraded) == 0 || !strings.Contains(fmt.Sprint(res.Degraded), "old") {
		t.Errorf("staleness must be reported with its reason, got %v", res.Degraded)
	}
}

// TestCalibrationCoversRealQueryShapes is the regression guard for the
// defect that made this feature INERT in production while forty tests
// passed.
//
// Probes were single terms; real agent queries are several words that
// relaxQuery ORs together, and BM25 sums per-term contributions. On the
// live index single-term probes spanned magnitudes 10.9–11.4 while a
// five-word query spanned 19.6–43.4 — so every real hit landed above
// the entire sampled distribution, clamped to quantile 1.0, and every
// result scored an identical 0.95. Ranking silently fell back to the
// within-corpus tiebreak.
//
// The tests missed it because they queried single terms too. This one
// calibrates, then asserts a MULTI-TERM query produces a spread of
// quantiles rather than a wall of 1.0.
func TestCalibrationCoversRealQueryShapes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 400)

	now := time.Now()
	spec, _ := corpusByKind("message")
	cal, err := s.CalibrateCorpus(context.Background(), spec, now)
	if err != nil {
		t.Fatalf("CalibrateCorpus: %v", err)
	}

	// A realistic multi-word query, the shape agents actually send.
	res, err := s.UnifiedSearch("watcher descriptor compaction segment throttle",
		[]string{"message"}, 20, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) < 5 {
		t.Fatalf("expected several hits, got %d", len(res.Hits))
	}

	saturated := 0
	distinct := map[float64]bool{}
	for _, h := range res.Hits {
		// Round away the rank tiebreak so only real quantile differences count.
		distinct[float64(int(h.Score*100))/100] = true
		if h.Score >= 0.949 {
			saturated++
		}
	}
	if saturated == len(res.Hits) {
		t.Errorf("every one of %d hits saturated at the top of the distribution "+
			"(top boundary %.2f) — the probe distribution does not cover real "+
			"query shapes, so calibration orders nothing",
			len(res.Hits), cal.Quantiles[len(cal.Quantiles)-1])
	}
	if len(distinct) < 2 {
		t.Errorf("all hits scored identically (%v); calibration is inert for this "+
			"query shape", distinct)
	}
}
