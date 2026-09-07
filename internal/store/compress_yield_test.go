// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestPauseAfterBatchScalesWithBatchCost pins the duty-cycle arithmetic
// (🎯T168). The pause the packer takes has to be a function of what the
// batch just cost, not a constant: the fixed 10ms it used to take left
// the writer pinned 99.5% of the time on a batch of any real size.
func TestPauseAfterBatchScalesWithBatchCost(t *testing.T) {
	s := &Store{}

	// A zero yield is the explicit, user-triggered compress_gc: someone
	// is waiting on it, so it does not pace itself.
	s.backfill.setYield(0)
	start := time.Now()
	if err := s.pauseAfterBatch(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("a zero yield must not pause, slept %s", elapsed)
	}

	// At a 0.5 duty cycle the pause matches the busy time, so the packer
	// gets half the wall clock and foreground work gets the other half.
	s.backfill.setYield(10 * time.Millisecond)
	start = time.Now()
	if err := s.pauseAfterBatch(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	want := time.Duration(float64(100*time.Millisecond) * (1/backfillDutyCycle - 1))
	if elapsed < want || elapsed > want+80*time.Millisecond {
		t.Errorf("pause after a 100ms batch = %s, want ~%s", elapsed, want)
	}

	// A pathologically slow batch must not park the packer for minutes.
	start = time.Now()
	if err := s.pauseAfterBatch(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > backfillMaxPause+time.Second {
		t.Errorf("pause after an hour-long batch = %s, want ≤ %s", elapsed, backfillMaxPause)
	}
}

// TestPauseAfterBatchHonoursCancellation makes sure a paused packer
// still stops promptly on shutdown rather than sleeping out its pause.
func TestPauseAfterBatchHonoursCancellation(t *testing.T) {
	s := &Store{}
	s.backfill.setYield(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.pauseAfterBatch(ctx, time.Minute); err == nil {
		t.Error("a cancelled context must abort the pause, got nil error")
	}
}

// TestBackfillLeavesTheWriterAvailable is the behavioural half of
// 🎯T168: while the packer runs, an ordinary foreground write must still
// get through in reasonable time.
//
// On 0.94.0 the batch's zstd encoding and round-trip verification
// happened between BeginTx and Commit, so the single SQLite writer was
// held for the whole of it and foreground writes queued behind the
// packer. This drives a real backfill over several batches and measures
// the worst latency a concurrent writer sees.
//
// The bound is deliberately loose. The assertion worth making on shared
// CI is "a foreground write is not blocked for the length of a batch",
// not a tight latency figure that would flake on a loaded runner.
func TestBackfillLeavesTheWriterAvailable(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 3*backfillBatchRows)

	// Pace the packer the way the auto worker does; an explicit
	// compress_gc deliberately does not yield.
	s.backfill.setYield(compressAutoYield)
	defer s.backfill.setYield(0)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := s.CompressBackfill(ctx, FamilyMessagesText)
		done <- err
	}()

	var worst time.Duration
	var writes int
	var finished bool
	deadline := time.Now().Add(30 * time.Second)
	for !finished && time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("backfill: %v", err)
			}
			finished = true
		default:
		}
		start := time.Now()
		_, err := s.writeDB.ExecContext(ctx,
			`INSERT INTO compression_gc (family, next_id, done, saved_bytes, updated_at, last_error)
			 VALUES ('probe', 1, 0, 0, ?, '')
			 ON CONFLICT(family) DO UPDATE SET updated_at = excluded.updated_at`,
			time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatalf("foreground write failed while the backfill ran: %v", err)
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
		writes++
		time.Sleep(5 * time.Millisecond)
	}

	if writes < 10 {
		t.Fatalf("only %d foreground writes sampled; the test measured nothing", writes)
	}
	if worst > 3*time.Second {
		t.Errorf("worst foreground write latency during the backfill was %s over %d writes; "+
			"the packer is holding the SQLite writer across its encode phase again", worst, writes)
	}
	t.Logf("worst foreground write latency %s over %d writes", worst, writes)

	cancel()
	if !finished {
		<-done
	}
}
