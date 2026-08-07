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

// TestQuantileMapsWithinCorpus covers the primitive everything else
// rests on: a score maps to its position in its own corpus's
// distribution, not to an absolute scale.
func TestQuantileMapsWithinCorpus(t *testing.T) {
	// A corpus whose scores run 0..100.
	q := make([]float64, calibrationQuantiles)
	for i := range q {
		q[i] = float64(i)
	}
	cal := &Calibration{Corpus: "test", Quantiles: q}

	for _, tc := range []struct {
		mag  float64
		want float64
	}{
		{-5, 0},   // below the distribution
		{0, 0},    // p0
		{50, 0.5}, // median
		{100, 1},  // p100
		{500, 1},  // above the distribution
	} {
		got := cal.Quantile(tc.mag)
		if diff := got - tc.want; diff > 0.02 || diff < -0.02 {
			t.Errorf("Quantile(%v) = %.3f, want ~%.3f", tc.mag, got, tc.want)
		}
	}
}

// TestQuantileIsScaleInvariant is the property that makes cross-corpus
// comparison meaningful: two corpora with wildly different score
// magnitudes must map equivalent-quality hits to equivalent quantiles.
//
// This is precisely what raw BM25 comparison fails to do, and why
// mnemo does not merge on raw score.
func TestQuantileIsScaleInvariant(t *testing.T) {
	small := make([]float64, calibrationQuantiles)
	large := make([]float64, calibrationQuantiles)
	for i := range small {
		small[i] = float64(i) * 0.01 // scores 0..1
		large[i] = float64(i) * 100  // scores 0..10000
	}
	a := &Calibration{Corpus: "small", Quantiles: small}
	b := &Calibration{Corpus: "large", Quantiles: large}

	// The 90th-percentile score in each corpus.
	qa := a.Quantile(0.90)
	qb := b.Quantile(9000)
	if diff := qa - qb; diff > 0.02 || diff < -0.02 {
		t.Errorf("equivalent-quality hits mapped to %.3f and %.3f; "+
			"calibration must be scale-invariant or cross-corpus merging is meaningless", qa, qb)
	}
}

// TestStalenessIsDetected pins the criterion that a distribution which
// no longer describes its corpus is reported rather than silently used.
// This is the failure mode with no visible symptom: every score
// mis-maps, and the resulting ordering still looks reasonable.
func TestStalenessIsDetected(t *testing.T) {
	now := time.Now()
	fresh := &Calibration{
		Corpus:     "c",
		Quantiles:  []float64{0, 1, 2},
		DocCount:   1000,
		ComputedAt: now.Add(-time.Hour),
	}

	if stale, why := fresh.Stale(now, 1050); stale {
		t.Errorf("a fresh calibration on a barely-grown corpus is not stale: %s", why)
	}
	if stale, _ := fresh.Stale(now, 10000); !stale {
		t.Error("a corpus that grew 10x must invalidate its calibration")
	}
	old := &Calibration{
		Corpus:     "c",
		Quantiles:  []float64{0, 1, 2},
		DocCount:   1000,
		ComputedAt: now.Add(-30 * 24 * time.Hour),
	}
	if stale, _ := old.Stale(now, 1000); !stale {
		t.Error("a month-old calibration must be stale on age alone")
	}
	var missing *Calibration
	stale, why := missing.Stale(now, 100)
	if !stale || why == "" {
		t.Error("a missing calibration must be stale, with a reason")
	}
}

// TestUnifiedSearchSpansCorpora is the headline behaviour: one query,
// hits from several corpora, each labelled with where it came from.
func TestUnifiedSearchSpansCorpora(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCorpora(t, s)

	res, err := s.UnifiedSearch("watcher", nil, 20, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	kinds := map[string]int{}
	for _, h := range res.Hits {
		kinds[h.Kind]++
		if h.Title == "" && h.Body == "" {
			t.Errorf("hit %s/%d has no displayable content — hydration failed", h.Kind, h.ID)
		}
	}
	if len(kinds) < 2 {
		t.Fatalf("expected hits from several corpora, got %v — this is the whole point", kinds)
	}
	if len(res.Corpora) == 0 {
		t.Error("result must report which corpora were searched")
	}
}

// TestUnifiedSearchDegradesToNeutral is the safety criterion: with no
// calibration stored, hits score at the neutral prior and SAY so —
// never by raw BM25 comparison, which is the method calibration exists
// to replace. A design that degrades into its own rejected failure mode
// is worse than one that never had the good path.
//
// Was "DegradesToFusion" before shrinkage replaced the two-tier
// calibrated/fused ordering with one continuous scale.
func TestUnifiedSearchDegradesToNeutral(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCorpora(t, s)

	res, err := s.UnifiedSearch("watcher", nil, 20, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range res.Hits {
		if h.Ranking != "neutral" {
			t.Errorf("hit %s/%d ranked %q with no calibration stored; want neutral",
				h.Kind, h.ID, h.Ranking)
		}
	}
	if len(res.Degraded) == 0 {
		t.Error("degraded ranking must be reported, not silent")
	}
}

// TestShortDocumentsDoNotMonopolise is the bias oracle 🎯T144 requires.
//
// A corpus of short documents (commit subjects) and one of long
// documents (messages) both contain the query term. Under raw BM25
// comparison the short corpus would sweep the head, because BM25
// length-normalises against a per-index avgdl and short documents score
// higher for the same term frequency. The merged result must not be
// monopolised by either.
func TestShortDocumentsDoNotMonopolise(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Both corpora get a SPREAD of match quality, not one repeated
	// document. A degenerate corpus (every score identical) has no
	// distribution to calibrate against, and a test built on one would
	// be asserting against an artefact of the fixture rather than the
	// ranking.
	//
	// Long documents (messages, ~2.4kB) against short ones (commit
	// subjects, ~60B) — a 40x length ratio, which is what makes BM25's
	// per-index avgdl normalisation visible. Each corpus contains a few
	// strong matches for the query term and many weak ones.
	vocab := []string{"watcher", "descriptor", "compaction", "segment", "throttle",
		"reconciler", "transcript", "ingest", "calibration", "boundary"}
	filler := strings.Repeat("context and surrounding prose that pads this document out. ", 40)
	for i := 0; i < 60; i++ {
		body := fmt.Sprintf("%s and %s under load. ", vocab[i%len(vocab)], vocab[(i+3)%len(vocab)])
		if i%12 == 0 {
			body = "vnode vnode vnode exhaustion of the vnode table. " + body // strong match
		} else if i%4 == 0 {
			body = "vnode exhaustion noted. " + body // weak match
		}
		mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			fmt.Sprintf("sess%d", i), body+filler)
	}
	for i := 0; i < 60; i++ {
		subj := fmt.Sprintf("adjust %s handling in the %s subsystem path %d",
			vocab[i%len(vocab)], vocab[(i+5)%len(vocab)], i)
		if i%12 == 0 {
			subj = fmt.Sprintf("fix vnode vnode exhaustion in the %s path %d", vocab[i%len(vocab)], i)
		} else if i%4 == 0 {
			subj = fmt.Sprintf("note vnode exhaustion in the %s path %d", vocab[i%len(vocab)], i)
		}
		mustExec(t, s, `INSERT INTO git_commits (repo, commit_hash, author_name, author_email, commit_date, subject, body)
			VALUES ('o/r', ?, 'a', 'a@b', '2026-01-01T00:00:00Z', ?, '')`,
			fmt.Sprintf("hash%d", i), subj)
	}

	// Calibrate BOTH corpora, so this exercises the path that actually
	// ships rather than the degraded one. Without this the test would
	// pass on fusion and say nothing about calibrated ranking — the
	// distinction matters, because fusion interleaves by construction
	// and calibration has to earn it.
	now := time.Now()
	for _, kind := range []string{"message", "commit"} {
		spec, _ := corpusByKind(kind)
		if _, err := s.CalibrateCorpus(context.Background(), spec, now); err != nil {
			t.Fatalf("CalibrateCorpus(%s): %v", kind, err)
		}
	}

	res, err := s.UnifiedSearch("vnode", []string{"message", "commit"}, 20, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) < 10 {
		t.Fatalf("expected a full result set, got %d", len(res.Hits))
	}
	for _, h := range res.Hits {
		if h.Ranking != "calibrated" {
			t.Fatalf("hit ranked %q — this oracle must exercise the calibrated path, "+
				"not fall through to fusion (degraded: %v)", h.Ranking, res.Degraded)
		}
	}
	counts := map[string]int{}
	for _, h := range res.Hits[:10] {
		counts[h.Kind]++
	}
	// Neither corpus may take the entire head. This is deliberately a
	// weak bound: the claim is "no monopoly", not a precise ratio,
	// because the right ratio is a product judgement and pinning one
	// would make the test assert an opinion rather than a property.
	for kind, n := range counts {
		if n == 10 {
			t.Errorf("%s took all 10 head positions (%v) — one corpus is monopolising "+
				"the merged result, which is the failure cross-corpus ranking exists to prevent",
				kind, counts)
		}
	}
	if len(counts) < 2 {
		t.Errorf("head contains only %v; both corpora matched and both should appear", counts)
	}
}

// TestUnknownKindIsRejected: an unknown corpus must name the valid ones
// rather than silently returning nothing.
func TestUnknownKindIsRejected(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	_, err := s.UnifiedSearch("x", []string{"nonsense"}, 10, time.Now())
	if err == nil {
		t.Fatal("want an error for an unknown kind")
	}
	if !strings.Contains(err.Error(), "message") {
		t.Errorf("error %q does not name the valid kinds", err)
	}
}

// TestCalibrateCorpusProducesUsableDistribution runs the real sampling
// path end to end against a seeded corpus.
func TestCalibrateCorpusProducesUsableDistribution(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	// Enough varied text that probe sampling finds distinct terms and
	// each probe returns several hits.
	words := []string{"watcher", "vnode", "descriptor", "compaction", "segment",
		"calibration", "throttle", "reconciler", "transcript", "ingest"}
	for i := 0; i < 200; i++ {
		text := fmt.Sprintf("%s %s %s discussion of %s behaviour under load with surrounding prose",
			words[i%len(words)], words[(i+3)%len(words)], words[(i+7)%len(words)], words[(i+1)%len(words)])
		mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			fmt.Sprintf("s%d", i), text)
	}
	spec, _ := corpusByKind("message")
	cal, err := s.CalibrateCorpus(context.Background(), spec, time.Now())
	if err != nil {
		t.Fatalf("CalibrateCorpus: %v", err)
	}
	if len(cal.Quantiles) != calibrationQuantiles {
		t.Fatalf("got %d quantile points, want %d", len(cal.Quantiles), calibrationQuantiles)
	}
	// Boundaries must ascend, or quantile lookup is meaningless.
	for i := 1; i < len(cal.Quantiles); i++ {
		if cal.Quantiles[i] < cal.Quantiles[i-1] {
			t.Fatalf("quantile boundaries not ascending at %d: %v > %v",
				i, cal.Quantiles[i-1], cal.Quantiles[i])
		}
	}
	// And it must round-trip.
	loaded, err := s.LoadCalibrations()
	if err != nil {
		t.Fatalf("LoadCalibrations: %v", err)
	}
	if loaded["message"] == nil {
		t.Fatal("calibration did not persist")
	}
	if loaded["message"].SampleSize != cal.SampleSize {
		t.Errorf("sample size did not round-trip: %d vs %d",
			loaded["message"].SampleSize, cal.SampleSize)
	}
}

// TestCalibratedRankingBeatsFusionLabel proves the primary path
// actually engages once a corpus is calibrated — otherwise every result
// would quietly be fusion forever and the calibration would be dead
// code wearing a green test.
func TestCalibratedRankingBeatsFusionLabel(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	words := []string{"watcher", "vnode", "descriptor", "compaction", "segment"}
	for i := 0; i < 200; i++ {
		mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			fmt.Sprintf("s%d", i),
			fmt.Sprintf("%s and %s appear together in this document about system behaviour",
				words[i%len(words)], words[(i+2)%len(words)]))
	}
	now := time.Now()
	spec, _ := corpusByKind("message")
	if _, err := s.CalibrateCorpus(context.Background(), spec, now); err != nil {
		t.Fatalf("CalibrateCorpus: %v", err)
	}
	res, err := s.UnifiedSearch("watcher", []string{"message"}, 10, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range res.Hits {
		if h.Ranking != "calibrated" {
			t.Fatalf("hit ranked %q after calibration; the primary path is not engaging", h.Ranking)
		}
		if h.Score < 0 || h.Score > 1 {
			t.Errorf("calibrated score %v is outside [0,1]", h.Score)
		}
	}
}

// --- helpers ---

func mustExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.writeDB.Exec(q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// seedCorpora inserts a matching row into several corpora so a single
// query spans them.
func seedCorpora(t *testing.T, s *Store) {
	t.Helper()
	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('s1', 'p', 'user', 'the watcher exhausted its file descriptors', '2026-01-01T00:00:00Z', 'user', 0, 'text')`)
	mustExec(t, s, `INSERT INTO docs (repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at)
		VALUES ('o/r', 'docs/w.md', 'md', 'Watcher design', 'the watcher uses FSEvents', 'h', 10, '2026-01-01', '2026-01-01')`)
	mustExec(t, s, `INSERT INTO git_commits (repo, commit_hash, author_name, author_email, commit_date, subject, body)
		VALUES ('o/r', 'abc123', 'a', 'a@b', '2026-01-01T00:00:00Z', 'fix watcher leak', 'the watcher leaked')`)
	mustExec(t, s, `INSERT INTO targets (repo, file_path, target_id, name, status, weight, description, raw_text)
		VALUES ('o/r', 'docs/targets.md', 'T1', 'watcher is bounded', 'identified', 1, 'the watcher must be bounded', 'raw')`)
}
