// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/breaker"
)

func resetOCRClock(t *testing.T) {
	t.Helper()
	resetOCRState(t)
	prevNow := ocrNow
	t.Cleanup(func() { ocrNow = prevNow })
	prevEnter := testOCRAndStoreEntered
	t.Cleanup(func() { testOCRAndStoreEntered = prevEnter })
}

func insertOCRTestImage(t *testing.T, s *Store, id int64, hash string) {
	t.Helper()
	if _, err := s.writeDB.Exec(
		`INSERT INTO images (id, content_hash, bytes, mime_type, width, height,
		                     pixel_format, byte_size, created_at)
		 VALUES (?, ?, ?, 'image/png', 10, 10, 'rgba', 10, '2026-07-26T00:00:00Z')`,
		id, hash, []byte("generic-bytes"),
	); err != nil {
		t.Fatal(err)
	}
}

type ocrFlightHold struct {
	calls   *atomic.Int32
	ready   <-chan struct{}
	rel     chan struct{}
	relOnce sync.Once
}

func (h *ocrFlightHold) Release() {
	h.relOnce.Do(func() { close(h.rel) })
}

func waitOCRFlight(key string) {
	ocrImageFlight.Do(key, func() (any, error) { return nil, nil })
}

// holdOCRFlight blocks the single-flight leader inside runOCR until Release.
// ready is closed after n callers have entered ocrAndStoreCtx and the leader
// has reached runOCR (event wait, not a sleep/poll).
func holdOCRFlight(t *testing.T, n int) *ocrFlightHold {
	t.Helper()
	var calls atomic.Int32
	var entered atomic.Int32
	rel := make(chan struct{})
	allEntered := make(chan struct{})
	leaderIn := make(chan struct{})
	ready := make(chan struct{})

	prevEnter := testOCRAndStoreEntered
	testOCRAndStoreEntered = func() {
		if entered.Add(1) == int32(n) {
			close(allEntered)
		}
	}
	prevHook := testRunOCRHook
	testRunOCRHook = func() {
		if calls.Add(1) == 1 {
			close(leaderIn)
		}
		<-rel
	}
	h := &ocrFlightHold{calls: &calls, ready: ready, rel: rel}
	t.Cleanup(func() {
		h.Release()
		testOCRAndStoreEntered = prevEnter
		testRunOCRHook = prevHook
	})
	go func() {
		<-allEntered
		<-leaderIn
		close(ready)
	}()
	return h
}

func TestOCRIndependentSequentialFailuresTripThreshold(t *testing.T) {
	resetOCRClock(t)
	requirePOSIXShell(t)
	withWorker(t, "exit 134")
	s := newTestStore(t, t.TempDir())

	for i := 1; i <= ocrConsecutiveAbortLimit; i++ {
		id := int64(i)
		insertOCRTestImage(t, s, id, fmt.Sprintf("hash-seq-%d", i))
		ocrAndStore(s.writeDB, id, []byte("generic-bytes"), "apple_vision")
		if i < ocrConsecutiveAbortLimit && ocrBreaker.Snapshot().State == breaker.StateOpen {
			t.Fatalf("opened early after %d independent failures", i)
		}
	}
	if ocrBreaker.Snapshot().State != breaker.StateOpen {
		t.Fatalf("want open after %d independent sequential failures, got %+v",
			ocrConsecutiveAbortLimit, ocrBreaker.Snapshot())
	}
}

func TestOCRSingleFlightCanceledWaitersOneFailure(t *testing.T) {
	resetOCRClock(t)
	requirePOSIXShell(t)
	withWorker(t, "exit 134")

	s := newTestStore(t, t.TempDir())
	insertOCRTestImage(t, s, 1, "hash-cancel")

	const n = 8
	hold := holdOCRFlight(t, n)
	cancels := make([]context.CancelFunc, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go func(ctx context.Context) {
			defer wg.Done()
			ocrAndStoreCtx(ctx, s.writeDB, 1, []byte("generic-bytes"), "apple_vision")
		}(ctx)
	}
	<-hold.ready
	for _, cancel := range cancels {
		cancel()
	}
	hold.Release()
	waitOCRFlight("1")
	wg.Wait()

	if got := hold.calls.Load(); got != 1 {
		t.Fatalf("runOCR calls = %d, want 1 (canceled waiters must not start a new flight)", got)
	}
	if ocrBreaker.Snapshot().Failures != 1 {
		t.Fatalf("breaker failures = %d, want 1 (canceled waiters must not multiply counts)", ocrBreaker.Snapshot().Failures)
	}
}

// TestOCRBaseline217de19_DuplicateConcurrentIsolatedRunsEachCountFailure is the
// AFTER half of the 217de19 proof: same observable contract as the adapter
// copied into an unmodified 217de19 worktree (want 1 logical failure).
func TestOCRBaseline217de19_DuplicateConcurrentIsolatedRunsEachCountFailure(t *testing.T) {
	resetOCRClock(t)
	requirePOSIXShell(t)
	withWorker(t, "exit 134")

	s := newTestStore(t, t.TempDir())
	insertOCRTestImage(t, s, 1, "hash-baseline-final")

	const n = 3
	hold := holdOCRFlight(t, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ocrAndStore(s.writeDB, 1, []byte("generic-bytes"), "apple_vision")
		}()
	}
	<-hold.ready
	hold.Release()
	waitOCRFlight("1")
	wg.Wait()

	if got := hold.calls.Load(); got != 1 {
		t.Fatalf("runOCR calls = %d, want 1", got)
	}
	if got := ocrBreaker.Snapshot().Failures; got != 1 {
		t.Fatalf("want 1 failure toward breaker (single-flight), got %d", got)
	}
}

// TestOCRBaseline217de19_RecoversAfterThresholdSuccess is the AFTER half of
// recovery proof: after the trip threshold, a later successful trial closes
// the breaker (217de19's abort latch never recovers).
func TestOCRBaseline217de19_RecoversAfterThresholdSuccess(t *testing.T) {
	resetOCRClock(t)
	requirePOSIXShell(t)
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	SetOCRNowForTest(t0)

	s := newTestStore(t, t.TempDir())
	withWorker(t, "exit 134")
	for i := 1; i <= ocrConsecutiveAbortLimit; i++ {
		id := int64(100 + i)
		insertOCRTestImage(t, s, id, fmt.Sprintf("hash-rec-%d", i))
		ocrAndStore(s.writeDB, id, []byte("generic-bytes"), "apple_vision")
	}
	if ocrBreaker.Snapshot().State != breaker.StateOpen {
		t.Fatalf("want open after threshold, got %+v", ocrBreaker.Snapshot())
	}

	SetOCRNowForTest(t0.Add(ocrBreakerCooldown))
	if !TouchOCRBreakerAllowForTest(ocrNow()) {
		t.Fatal("expected half-open after cooldown")
	}
	prev := ocrWorkerCommand
	ocrWorkerCommand = func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", `printf '%s\n' '{"text":"recovered"}'`), nil
	}
	t.Cleanup(func() { ocrWorkerCommand = prev })
	insertOCRTestImage(t, s, 200, "hash-rec-ok")
	ocrAndStore(s.writeDB, 200, []byte("generic-bytes"), "apple_vision")
	if ocrBreaker.Snapshot().State != breaker.StateClosed {
		t.Fatalf("successful trial must close the breaker, got %+v", ocrBreaker.Snapshot())
	}
	st := OCRProcessStatus()
	if st.Reason != "ok" || st.BreakerState != string(breaker.StateClosed) {
		t.Fatalf("health after successful trial = %+v, want ok/closed", st)
	}
}

func TestOCRCircuitBreakerRecoversWithoutSleep(t *testing.T) {
	resetOCRClock(t)
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	SetOCRNowForTest(t0)

	TripOCRAbortLatchForTest("boom")
	if ocrBreaker.Snapshot().State != breaker.StateOpen {
		t.Fatal("expected open")
	}
	SetOCRNowForTest(t0.Add(ocrBreakerCooldown))
	now := ocrNow()
	if !ocrBreaker.Allow(now) {
		t.Fatal("expected half-open trial after cooldown")
	}
	ocrBreaker.Record(now, true, "")
	if ocrBreaker.Snapshot().State != breaker.StateClosed {
		t.Fatalf("success should close breaker, got %+v", ocrBreaker.Snapshot())
	}
}
