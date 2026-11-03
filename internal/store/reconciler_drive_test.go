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

// stuckStream ignores ctx entirely and stays in Reconcile until the test
// releases it — the shape of a pass parked in an uncancellable syscall.
type stuckStream struct {
	name    string
	timeout time.Duration
	release chan struct{}
	calls   atomic.Int32
}

func (f *stuckStream) Name() string               { return f.name }
func (f *stuckStream) Interval() time.Duration    { return time.Minute }
func (f *stuckStream) PassTimeout() time.Duration { return f.timeout }
func (f *stuckStream) Reconcile(ctx context.Context, now time.Time) (int, error) {
	f.calls.Add(1)
	<-f.release
	return 0, nil
}

// TestStuckStreamDoesNotDelayOtherStreams: a stream that never returns
// must be reported as timed out at its own pass timeout, must not be
// relaunched while it is still stuck, and must not hold up any other
// stream's minute-by-minute passes.
//
// The daemon once lost every stream to a single mirror pass parked in
// open(2): the tick waited out a shared 30-minute budget, so a
// one-minute stream ran twice an hour, and every 30 minutes another
// copy of the stuck pass was launched on top of the last.
func TestStuckStreamDoesNotDelayOtherStreams(t *testing.T) {
	stuck := &stuckStream{name: "stuck", timeout: 300 * time.Millisecond, release: make(chan struct{})}
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(stuck.release)
		}
	}
	t.Cleanup(release)
	fast := &fakeStream{name: "fast", interval: time.Minute, timeout: time.Second}
	track := &reconcilerTracker{}

	// tick runs one "minute" of the scheduler and fails the test, rather
	// than hanging it, if the tick does not come back in time.
	tick := func(i int, within time.Duration) time.Duration {
		t.Helper()
		start := time.Now()
		done := make(chan struct{})
		go func() {
			DriveStreamReconcilers(context.Background(),
				[]StreamReconciler{stuck, fast}, time.Now(), track)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(within):
			t.Fatalf("tick %d still waiting after %v: a stuck stream is holding up the scheduler", i, within)
		}
		return time.Since(start)
	}

	// First minute: the stuck pass is cut off at its own timeout.
	if d := tick(1, 2*time.Second); d < 250*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("tick 1 took %v; want about the stuck stream's 300ms pass timeout", d)
	}
	track.mu.Lock()
	sr := track.records["stuck"]
	track.mu.Unlock()
	if want := "stream stuck exceeded pass timeout 300ms"; sr.lastErr != want {
		t.Errorf("stuck stream lastErr = %q, want %q", sr.lastErr, want)
	}
	if sr.inFlightSince.IsZero() {
		t.Error("stuck stream no longer reported in flight while its pass is still running")
	}
	if !sr.lastCompleted.IsZero() {
		t.Error("a pass abandoned at its deadline was recorded as completed")
	}

	// Following minutes: the stuck stream is not relaunched, and nothing
	// waits on it.
	for i := 2; i <= 5; i++ {
		if d := tick(i, time.Second); d > 500*time.Millisecond {
			t.Errorf("tick %d took %v while the stuck stream was skipped; want prompt", i, d)
		}
	}
	if got := fast.ran.Load(); got != 5 {
		t.Errorf("fast stream ran %d times in 5 ticks, want 5", got)
	}
	if got := stuck.calls.Load(); got != 1 {
		t.Errorf("stuck stream launched %d times while still stuck, want 1", got)
	}

	// Once the stuck call finally returns, the stream is scheduled again.
	release()
	deadline := time.Now().Add(2 * time.Second)
	for {
		track.mu.Lock()
		inFlight := !track.records["stuck"].inFlightSince.IsZero()
		track.mu.Unlock()
		if !inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stuck stream still in flight after its call returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tick(6, 2*time.Second)
	if got := stuck.calls.Load(); got != 2 {
		t.Errorf("stuck stream calls after release = %d, want 2 (relaunched)", got)
	}
}

// TestLaunchStreamReconcilersDoesNotWait: the daemon's tick launches
// passes and moves on; a long, legitimately busy pass of one stream
// never delays the next minute's pass of another.
func TestLaunchStreamReconcilersDoesNotWait(t *testing.T) {
	busy := &fakeStream{name: "busy", interval: time.Minute, timeout: 10 * time.Second, honour: true}
	fast := &fakeStream{name: "fast", interval: time.Minute, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	track := &reconcilerTracker{}
	for i := 0; i < 3; i++ {
		start := time.Now()
		LaunchStreamReconcilers(ctx, []StreamReconciler{busy, fast}, time.Now(), track)
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("launch %d blocked for %v", i+1, d)
		}
		want := int32(i + 1)
		deadline := time.Now().Add(time.Second)
		for fast.ran.Load() < want || inFlight(track, "fast") {
			if time.Now().After(deadline) {
				t.Fatalf("fast stream did not complete pass %d while busy was mid-pass", want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got := busy.ran.Load(); got != 1 {
		t.Errorf("busy stream launched %d times while its first pass was running, want 1", got)
	}
}

func inFlight(track *reconcilerTracker, name string) bool {
	track.mu.Lock()
	defer track.mu.Unlock()
	return !track.records[name].inFlightSince.IsZero()
}
