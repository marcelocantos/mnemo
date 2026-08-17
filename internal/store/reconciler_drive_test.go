// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeStream struct {
	name     string
	interval time.Duration
	timeout  time.Duration
	block    bool // ignore cancel and sleep long
	honour   bool // block until ctx done
	ran      atomic.Int32
}

func (f *fakeStream) Name() string            { return f.name }
func (f *fakeStream) Interval() time.Duration { return f.interval }
func (f *fakeStream) PassTimeout() time.Duration {
	if f.timeout > 0 {
		return f.timeout
	}
	return 100 * time.Millisecond
}
func (f *fakeStream) Reconcile(ctx context.Context, now time.Time) (int, error) {
	f.ran.Add(1)
	if f.block {
		time.Sleep(2 * time.Second) // ignore ctx — worst case
		return 0, nil
	}
	if f.honour {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 1, nil
}

// TestDriveStreamReconcilersNoStarvation: a blocking stream ahead of a
// fast one must not prevent the fast stream from reconciling (🎯T145).
func TestDriveStreamReconcilersNoStarvation(t *testing.T) {
	slow := &fakeStream{name: "slow", interval: time.Second, timeout: 80 * time.Millisecond, honour: true}
	fast := &fakeStream{name: "fast", interval: time.Second, timeout: 200 * time.Millisecond}
	track := &reconcilerTracker{}
	start := time.Now()
	DriveStreamReconcilers(context.Background(), []StreamReconciler{slow, fast}, time.Now(), track)
	elapsed := time.Since(start)
	if fast.ran.Load() == 0 {
		t.Fatal("fast stream never reconciled while slow was blocking")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("tick took %v; isolation should finish near max pass timeout", elapsed)
	}
	// Fast should complete successfully.
	track.mu.Lock()
	fr := track.records["fast"]
	track.mu.Unlock()
	if fr.lastCompleted.IsZero() {
		t.Fatal("fast stream not marked completed")
	}
	if fr.lastErr != "" {
		t.Errorf("fast stream error: %s", fr.lastErr)
	}
}

func TestClusterReconcileSkipsUnfinishedRun(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	// Insert a fake open cluster run.
	_, err := s.writeDB.Exec(`
		INSERT INTO cluster_runs (started_at, engine, trigger)
		VALUES (?, 'heuristic', 'test')
	`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		// Table might not exist in minimal test schema — skip if so.
		t.Skipf("cluster_runs unavailable: %v", err)
	}
	c := clusterReconcilerStream{s}
	n, err := c.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected skip while unfinished, got n=%d", n)
	}
}

func TestResolveOrphanClusterRuns(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	_, err := s.writeDB.Exec(`
		INSERT INTO cluster_runs (started_at, engine, trigger)
		VALUES (?, 'heuristic', 'test')
	`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Skipf("cluster_runs: %v", err)
	}
	n, err := s.ResolveOrphanClusterRuns(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("expected to close orphans, got %d", n)
	}
	last, err := s.LatestClusterRun()
	if err != nil || last == nil {
		t.Fatalf("latest: %v %v", last, err)
	}
	if last.EndedAt == "" {
		t.Fatal("ended_at still empty after resolve")
	}
	if last.FailureMode != "interrupted_shutdown" {
		t.Errorf("failure_mode=%q", last.FailureMode)
	}
}

func TestStreamHealthOverdue(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	// Pretend daemon started long ago with no completed pass.
	s.reconTracker.mu.Lock()
	s.reconTracker.daemonStart = time.Now().Add(-10 * time.Hour)
	s.reconTracker.records = map[string]streamPassRecord{}
	s.reconTracker.mu.Unlock()

	sr := &fakeStream{name: "search_calibration", interval: time.Hour}
	reports := s.StreamHealthReports([]StreamReconciler{sr}, time.Now())
	if len(reports) != 1 || !reports[0].Overdue {
		t.Fatalf("expected overdue calibration, got %+v", reports)
	}
}
