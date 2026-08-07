// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Refresh-path tests for 🎯T144 calibration.
//
// The reconciler is the half of this feature that decays silently. A
// calibration that is never refreshed still produces an ordering, and
// the ordering still looks reasonable — there is no error, no empty
// result, nothing a user would report. So the refresh path needs
// oracles for what it does AND for what it declines to do: recalibrating
// every corpus on every tick would be as wrong as never recalibrating,
// just expensive rather than stale.

// seedCalibratableCorpus fills messages with enough varied text that
// probe sampling finds distinct terms.
func seedCalibratableCorpus(t *testing.T, s *Store, n int) {
	t.Helper()
	vocab := []string{"watcher", "descriptor", "compaction", "segment", "throttle",
		"reconciler", "transcript", "ingest", "calibration", "boundary",
		"quantile", "corpus", "divergence", "snapshot", "checkpoint"}
	for i := 0; i < n; i++ {
		text := fmt.Sprintf(
			"%s interacts with %s during %s handling, with surrounding prose to give the document length",
			vocab[i%len(vocab)], vocab[(i+4)%len(vocab)], vocab[(i+9)%len(vocab)])
		mustExec(t, s, `INSERT INTO messages
			(session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			fmt.Sprintf("s%d", i), text)
	}
}

// TestReconcilerCalibratesWhenMissing: the cold-start path. A freshly
// migrated database has no calibrations, and the first reconcile pass
// must produce them.
func TestReconcilerCalibratesWhenMissing(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)

	rec := calibrationReconcilerStream{s}
	changed, err := rec.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if changed == 0 {
		t.Fatal("first pass calibrated nothing; cold start is broken")
	}
	cals, err := s.LoadCalibrations()
	if err != nil {
		t.Fatalf("LoadCalibrations: %v", err)
	}
	if cals["message"] == nil {
		t.Fatal("the messages corpus was not calibrated")
	}
	if cals["message"].SampleSize < calibrationMinSamples {
		t.Errorf("sample size %d is below the floor %d",
			cals["message"].SampleSize, calibrationMinSamples)
	}
}

// TestReconcilerIsQuiescent is the counterpart, and the one that keeps
// this cheap: a second pass immediately after the first must recalibrate
// nothing. A reconciler that redoes its work every tick is a background
// job that never idles.
func TestReconcilerIsQuiescent(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)
	rec := calibrationReconcilerStream{s}
	now := time.Now()

	if _, err := rec.Reconcile(context.Background(), now); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	changed, err := rec.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if changed != 0 {
		t.Errorf("second pass recalibrated %d corpora with nothing changed; "+
			"the reconciler is not quiescent", changed)
	}
}

// TestReconcilerRefreshesOnGrowth: the condition that actually matters.
// Corpora here grow continuously, and a distribution sampled when a
// corpus was a fraction of its current size mis-maps every score while
// producing an ordering that looks entirely reasonable.
func TestReconcilerRefreshesOnGrowth(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)
	rec := calibrationReconcilerStream{s}
	now := time.Now()

	if _, err := rec.Reconcile(context.Background(), now); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	before, _ := s.LoadCalibrations()
	if before["message"] == nil {
		t.Fatal("no initial calibration")
	}

	// Grow the corpus past the tolerance.
	seedCalibratableCorpus(t, s, 400)

	changed, err := rec.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatalf("post-growth pass: %v", err)
	}
	if changed == 0 {
		t.Fatal("corpus tripled and nothing recalibrated; growth detection is broken")
	}
	after, _ := s.LoadCalibrations()
	if after["message"].DocCount <= before["message"].DocCount {
		t.Errorf("doc count did not advance: %d → %d",
			before["message"].DocCount, after["message"].DocCount)
	}
}

// TestReconcilerRefreshesOnAge: the other staleness axis. A corpus that
// has not grown still needs periodic recalibration, because its content
// mix shifts even when its size does not.
func TestReconcilerRefreshesOnAge(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)
	rec := calibrationReconcilerStream{s}

	past := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := rec.Reconcile(context.Background(), past); err != nil {
		t.Fatalf("seed pass: %v", err)
	}
	// Same corpus, much later.
	changed, err := rec.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("aged pass: %v", err)
	}
	if changed == 0 {
		t.Error("a month-old calibration was not refreshed; age-based staleness is not firing")
	}
}

// TestReconcilerSurvivesUncalibratableCorpus: a corpus too small or too
// homogeneous to yield a distribution is a normal state, not a fault.
// The pass must continue and calibrate the corpora it can — a search
// feature that fails to start because one corpus is empty would be
// broken on every fresh install.
func TestReconcilerSurvivesUncalibratableCorpus(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)
	// docs has exactly one row: enough to be non-empty, far too few to
	// calibrate.
	mustExec(t, s, `INSERT INTO docs (repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at)
		VALUES ('o/r', 'a.md', 'md', 'T', 'a very short document', 'h', 5, '2026-01-01', '2026-01-01')`)

	rec := calibrationReconcilerStream{s}
	changed, err := rec.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("an uncalibratable corpus must not fail the pass: %v", err)
	}
	if changed == 0 {
		t.Fatal("the calibratable corpus was not calibrated")
	}
	cals, _ := s.LoadCalibrations()
	if cals["message"] == nil {
		t.Error("messages should have calibrated despite docs being too small")
	}
	if cals["doc"] != nil {
		t.Error("docs produced a calibration from one row; the sample floor is not enforced")
	}
}

// TestReconcilerHonoursCancellation: the pass runs in a daemon worker
// and must unwind on shutdown rather than finishing every corpus.
func TestReconcilerHonoursCancellation(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := calibrationReconcilerStream{s}
	start := time.Now()
	_, err := rec.Reconcile(ctx, time.Now())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled reconcile took %s", elapsed)
	}
	if err == nil {
		t.Error("a cancelled reconcile must report cancellation, not silent success — " +
			"a silent success would look like a completed pass")
	}
}

// TestRefreshClearsStaleness closes the loop: after the reconciler runs,
// search must actually use the calibrated path. Without this, every
// other refresh test could pass while search silently stayed on fusion
// forever.
func TestRefreshClearsStaleness(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 200)
	now := time.Now()

	res, err := s.UnifiedSearch("watcher", []string{"message"}, 5, now)
	if err != nil {
		t.Fatalf("pre-refresh search: %v", err)
	}
	if len(res.Hits) == 0 || res.Hits[0].Ranking != "fusion" {
		t.Fatal("expected fusion before the reconciler has run")
	}
	if len(res.Degraded) == 0 {
		t.Error("pre-refresh search must report degradation")
	}

	rec := calibrationReconcilerStream{s}
	if _, err := rec.Reconcile(context.Background(), now); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	res, err = s.UnifiedSearch("watcher", []string{"message"}, 5, now)
	if err != nil {
		t.Fatalf("post-refresh search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits after refresh")
	}
	for _, h := range res.Hits {
		if h.Ranking != "calibrated" {
			t.Fatalf("still ranking by %q after a successful refresh (degraded: %v)",
				h.Ranking, res.Degraded)
		}
	}
	if res.Degraded != nil {
		t.Errorf("degradation still reported after refresh: %v", res.Degraded)
	}
}

// TestReconcilerRegisteredInDataPlane: the reconciler must be wired into
// the worker that actually drives it. An unwired oracle decays; so does
// an unwired reconciler, silently and completely.
func TestReconcilerRegisteredInDataPlane(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	found := false
	for _, r := range s.StreamReconcilers() {
		if r.Name() == "search_calibration" {
			found = true
			if r.Interval() <= 0 {
				t.Error("calibration reconciler has a non-positive interval")
			}
		}
	}
	if !found {
		t.Fatal("the calibration reconciler is not registered in StreamReconcilers; " +
			"it would never run in the daemon")
	}
}
