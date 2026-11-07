// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/breaker"
)

// resetOCRState clears the process-wide OCR breaker so tests do not
// leak state into each other.
func resetOCRState(t *testing.T) {
	t.Helper()
	unlock := OCRBreakerTestLock()
	t.Cleanup(unlock)
	ResetOCRAbortLatchForTest()
	t.Cleanup(ResetOCRAbortLatchForTest)
}

// TestOCRWorkerRoundTrip pins the child-side contract: bytes in, one
// JSON object out. Whether recognition succeeds depends on the host's
// Vision stack, so this asserts the envelope, not the text.
func TestOCRWorkerRoundTrip(t *testing.T) {
	var out bytes.Buffer
	if err := RunOCRWorker(strings.NewReader("not a real image"), &out); err != nil {
		t.Fatalf("RunOCRWorker: %v", err)
	}
	var res ocrWorkerResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("worker emitted unparseable output %q: %v", out.String(), err)
	}
	// Garbage in must be reported as an error, never a panic or a crash.
	if res.Error == "" && res.Text != "" {
		t.Errorf("expected an error for non-image input, got text %q", res.Text)
	}
}

// TestOCRCircuitBreakerOpensAfterLimit covers the systemic case: repeated
// isolated worker deaths trip the breaker open (bounded cooldown, not
// restart-only permanent latch).
func TestOCRCircuitBreakerOpensAfterLimit(t *testing.T) {
	resetOCRState(t)

	for i := 1; i < ocrConsecutiveAbortLimit; i++ {
		noteOCRAbort("signal: abort trap")
		snap := ocrBreaker.Snapshot()
		if snap.State == breaker.StateOpen {
			t.Fatalf("breaker opened early after %d failures", i)
		}
		if appleVisionAvailable && ocrBackend() == "" {
			t.Fatalf("backend withdrawn early, after %d failures", i)
		}
	}
	noteOCRAbort("signal: abort trap")
	if ocrBreaker.Snapshot().State != breaker.StateOpen {
		t.Errorf("expected breaker open after %d consecutive worker deaths", ocrConsecutiveAbortLimit)
	}

	// While in cooldown, Apple Vision must not be selected as backend.
	if appleVisionAvailable && ocrBackend() == "apple_vision" {
		t.Error("apple_vision still selected immediately after breaker tripped")
	}
}

// TestOCRSuccessResetsBreakerRun: successes clear the consecutive failure
// run so intermittent worker deaths do not trip the breaker.
func TestOCRSuccessResetsBreakerRun(t *testing.T) {
	resetOCRState(t)

	noteOCRAbort("signal: abort trap")
	noteOCRAbort("signal: abort trap")
	if got := ocrBreaker.Snapshot().Failures; got != 2 {
		t.Fatalf("failure count = %d, want 2", got)
	}
	ocrBreaker.Record(time.Now(), true, "")
	noteOCRAbort("signal: abort trap")
	if got := ocrBreaker.Snapshot().Failures; got != 1 {
		t.Errorf("failure count = %d after a recorded success, want 1", got)
	}
	if ocrBreaker.Snapshot().State != breaker.StateClosed {
		t.Error("intermittent deaths must leave the breaker closed")
	}
}

// TestOCREmptyBytesRejectedWithoutSpawn guards the cheap case: no child
// process for input that cannot possibly recognise.
func TestOCREmptyBytesRejectedWithoutSpawn(t *testing.T) {
	resetOCRState(t)
	if _, _, err := runAppleVisionOCRIsolated(nil); err == nil {
		t.Error("expected an error for empty image bytes")
	}
	if ocrBreaker.Snapshot().Failures != 0 {
		t.Error("empty input must not count as a platform worker death")
	}
}

// requirePOSIXShell skips a test whose fixture drives /bin/sh.
//
// The Windows pre-merge gate runs the whole suite on the Parallels VM,
// and these fixtures (a worker that raises SIGABRT, a fake gh script)
// have no cmd.exe equivalent worth maintaining. What they cover —
// containment of an aborting subprocess, error classification — is
// platform-independent, so skipping there loses no coverage of
// Windows-specific behaviour.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture drives a POSIX shell; the behaviour under test is platform-independent")
	}
}

// withWorker swaps in a fake OCR worker for the duration of a test.
func withWorker(t *testing.T, script string) {
	t.Helper()
	requirePOSIXShell(t)
	prev := ocrWorkerCommand
	ocrWorkerCommand = func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", script), nil
	}
	t.Cleanup(func() { ocrWorkerCommand = prev })
}

// TestOCRWorkerAbortIsContained is the reason this whole mechanism
// exists (🎯T118). A worker that dies by SIGABRT — exactly what Vision
// does when the platform's Metal stack fails to load — must surface as
// an ordinary error on that image. Before isolation the same abort
// landed in the daemon and killed ingest, compaction, every mirror
// stream and the MCP endpoint.
//
// That this test returns at all is the assertion: an uncontained abort
// would take the test binary down with it.
func TestOCRWorkerAbortIsContained(t *testing.T) {
	resetOCRState(t)
	withWorker(t, "kill -ABRT $$")

	text, conf, err := runAppleVisionOCRIsolated([]byte("image bytes"))
	if err == nil {
		t.Fatal("a worker abort must be reported as an error")
	}
	if text != "" || conf != nil {
		t.Errorf("aborted worker must yield no result, got text=%q conf=%v", text, conf)
	}
	if !strings.Contains(err.Error(), "worker died") {
		t.Errorf("error should name the worker death, got %q", err)
	}
	if ocrBreaker.Snapshot().Failures != 1 {
		t.Errorf("breaker failures = %d, want 1", ocrBreaker.Snapshot().Failures)
	}
}

// TestOCRAbortRecordsFailureSoImageIsNotRetried: containment is only
// half of it — the image must also be marked so the next backfill pass
// skips it instead of respawning a doomed worker every startup.
func TestOCRAbortRecordsFailureSoImageIsNotRetried(t *testing.T) {
	resetOCRState(t)
	withWorker(t, "kill -ABRT $$")
	s := newTestStore(t, t.TempDir())

	if _, err := s.writeDB.Exec(
		`INSERT INTO images (id, content_hash, bytes, mime_type, width, height,
		                     pixel_format, byte_size, created_at)
		 VALUES (1, 'hash-1', ?, 'image/png', 10, 10, 'rgba', 10, '2026-07-26T00:00:00Z')`,
		[]byte("fake image"),
	); err != nil {
		t.Fatal(err)
	}

	ocrAndStore(s.writeDB, 1, []byte("fake image"), "apple_vision")

	var errMsg string
	if err := s.readDB.QueryRow(
		`SELECT COALESCE(error, '') FROM image_ocr WHERE image_id = 1`,
	).Scan(&errMsg); err != nil {
		t.Fatalf("no image_ocr row written for the aborted image: %v", err)
	}
	if errMsg == "" {
		t.Error("aborted image must record its failure, or it is retried forever")
	}

	// The pending query must now consider this image done.
	var pending int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*) FROM images img
		WHERE NOT EXISTS (SELECT 1 FROM image_ocr o WHERE o.image_id = img.id)
	`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("aborted image still pending (%d); it would be retried next startup", pending)
	}
}

// TestOCRWorkerTimeoutIsContained: a wedged worker must be killed rather
// than holding an OCR slot indefinitely.
func TestOCRWorkerTimeoutIsContained(t *testing.T) {
	resetOCRState(t)
	withWorker(t, "sleep 30")

	prevTimeout, prevDelay := ocrWorkerTimeout, ocrWorkerWaitDelay
	ocrWorkerTimeout = 200 * time.Millisecond
	// A forking `sh` keeps the output pipes open after the kill, so
	// without a short WaitDelay this blocks for the child's full sleep.
	ocrWorkerWaitDelay = 500 * time.Millisecond
	t.Cleanup(func() { ocrWorkerTimeout, ocrWorkerWaitDelay = prevTimeout, prevDelay })

	start := time.Now()
	_, _, err := runAppleVisionOCRIsolated([]byte("x"))
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a timeout error, got %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("worker was not killed at the deadline (took %s)", elapsed)
	}
}

// TestOCRProcessStatusReportsOpenBreaker is the /health proof: after the
// trip threshold, OCRProcessStatus reports apple_vision_breaker + open.
func TestOCRProcessStatusReportsOpenBreaker(t *testing.T) {
	resetOCRState(t)

	before := time.Now().Add(-time.Second)
	TripOCRAbortLatchForTest("timeout")
	after := time.Now().Add(time.Second)

	st := OCRProcessStatus()
	if !st.Disabled || st.Reason != "apple_vision_breaker" || st.BreakerState != "open" {
		t.Fatalf("status = %+v, want disabled apple_vision_breaker open", st)
	}
	if st.OpenedAt.Before(before) || st.OpenedAt.After(after) {
		t.Errorf("OpenedAt = %v, want within [%v, %v]", st.OpenedAt, before, after)
	}
	if !strings.Contains(st.Detail, "timeout") || !strings.Contains(st.Detail, "circuit open") {
		t.Errorf("detail missing why / open phase: %q", st.Detail)
	}
	if st.ConsecutiveFailures < ocrConsecutiveAbortLimit {
		t.Errorf("ConsecutiveFailures = %d, want >= %d", st.ConsecutiveFailures, ocrConsecutiveAbortLimit)
	}
}

// TestOCRCircuitBreakerRecoversAfterCooldown exercises half-open → closed.
func TestOCRCircuitBreakerRecoversAfterCooldown(t *testing.T) {
	resetOCRState(t)

	prevCD := ocrBreakerCooldown
	ocrBreakerCooldown = 50 * time.Millisecond
	t.Cleanup(func() { ocrBreakerCooldown = prevCD })

	TripOCRAbortLatchForTest("boom")
	if ocrBreaker.Snapshot().State != breaker.StateOpen {
		t.Fatal("expected open")
	}

	time.Sleep(ocrBreakerCooldown + 30*time.Millisecond)
	now := time.Now()
	if !ocrBreaker.Allow(now) {
		t.Fatal("expected half-open trial after cooldown")
	}
	ocrBreaker.Record(now, true, "")
	if ocrBreaker.Snapshot().State != breaker.StateClosed {
		t.Fatalf("success should close breaker, got %+v", ocrBreaker.Snapshot())
	}
}

// TestOCRSingleFlightDedupesConcurrentWorkers guards the bug where many
// sidecars for the same image each spawned a cold OCR run and each timeout
// incremented the consecutive counter — only one runOCR body executes.
func TestOCRSingleFlightDedupesConcurrentWorkers(t *testing.T) {
	resetOCRState(t)
	var calls atomic.Int32
	prev := testRunOCRHook
	testRunOCRHook = func() { calls.Add(1) }
	t.Cleanup(func() { testRunOCRHook = prev })

	backend := ""
	switch {
	case appleVisionAvailable:
		requirePOSIXShell(t)
		withWorker(t, `printf '%s\n' '{"text":"ok","confidence":0.99}'`)
		backend = "apple_vision"
	case lookPathOK("tesseract"):
		backend = "tesseract"
	default:
		t.Skip("need apple_vision (darwin) or tesseract for this OCR path")
	}

	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(
		`INSERT INTO images (id, content_hash, bytes, mime_type, width, height,
		                     pixel_format, byte_size, created_at)
		 VALUES (1, 'hash-sf', ?, 'image/png', 10, 10, 'rgba', 10, '2026-07-26T00:00:00Z')`,
		[]byte("fake image"),
	); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ocrAndStore(s.writeDB, 1, []byte("fake image"), backend)
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("runOCR hook calls = %d, want 1 (single-flight per image)", got)
	}
}

// TestOCRSingleFlightOneFailedRunOneBreakerCount: N concurrent waiters for one
// failed image — one actual worker path, one isolated failure toward the breaker.
func TestOCRSingleFlightOneFailedRunOneBreakerCount(t *testing.T) {
	resetOCRState(t)
	requirePOSIXShell(t)
	withWorker(t, "kill -ABRT $$")

	var calls atomic.Int32
	prev := testRunOCRHook
	testRunOCRHook = func() { calls.Add(1) }
	t.Cleanup(func() { testRunOCRHook = prev })

	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(
		`INSERT INTO images (id, content_hash, bytes, mime_type, width, height,
		                     pixel_format, byte_size, created_at)
		 VALUES (1, 'hash-sf-fail', ?, 'image/png', 10, 10, 'rgba', 10, '2026-07-26T00:00:00Z')`,
		[]byte("generic-bytes"),
	); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ocrAndStore(s.writeDB, 1, []byte("generic-bytes"), "apple_vision")
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("runOCR calls = %d, want 1 (single-flight)", got)
	}
	if ocrBreaker.Snapshot().Failures != 1 {
		t.Fatalf("breaker consecutive_failures = %d, want 1", ocrBreaker.Snapshot().Failures)
	}
}

// TestOCRJSONReportedErrorCountsTowardBreaker: worker exits 0 with error JSON — still a trial outcome.
func TestOCRJSONReportedErrorCountsTowardBreaker(t *testing.T) {
	resetOCRState(t)
	requirePOSIXShell(t)
	withWorker(t, `printf '%s\n' '{"error":"generic native failure"}'`)

	_, _, err := runAppleVisionOCRIsolated([]byte("x"))
	if err == nil || !strings.Contains(err.Error(), "generic native failure") {
		t.Fatalf("want reported error, got %v", err)
	}
	if ocrBreaker.Snapshot().Failures != 1 {
		t.Fatalf("breaker failures = %d, want 1", ocrBreaker.Snapshot().Failures)
	}
}

// TestOCRHalfOpenFailedTrialReopensWithCooldown: half-open failure starts a new open cooldown.
func TestOCRHalfOpenFailedTrialReopensWithCooldown(t *testing.T) {
	resetOCRState(t)

	prevCD := ocrBreakerCooldown
	ocrBreakerCooldown = 50 * time.Millisecond
	t.Cleanup(func() { ocrBreakerCooldown = prevCD })

	TripOCRAbortLatchForTest("trip1")
	t1 := time.Now().Add(ocrBreakerCooldown + 10*time.Millisecond)
	if !TouchOCRBreakerAllowForTest(t1) {
		t.Fatal("expected transition to half_open after cooldown")
	}
	noteOCRAbort("half_open_fail")
	if ocrBreaker.Snapshot().State != breaker.StateOpen {
		t.Fatalf("want open after failed half-open trial, got %+v", ocrBreaker.Snapshot())
	}
}

// TestOCRHalfOpenSerializesConcurrentTrials: only one subprocess runs while half_open.
func TestOCRHalfOpenSerializesConcurrentTrials(t *testing.T) {
	resetOCRState(t)
	requirePOSIXShell(t)

	prevCD := ocrBreakerCooldown
	ocrBreakerCooldown = time.Second
	t.Cleanup(func() { ocrBreakerCooldown = prevCD })

	TripOCRAbortLatchForTest("open_for_half_open")

	var inside, peakInside atomic.Int32
	prev := ocrWorkerCommand
	ocrWorkerCommand = func(ctx context.Context) (*exec.Cmd, error) {
		n := inside.Add(1)
		defer inside.Add(-1)
		for {
			cur := peakInside.Load()
			if n <= cur {
				break
			}
			if peakInside.CompareAndSwap(cur, n) {
				break
			}
		}
		return exec.CommandContext(ctx, "sh", "-c", `sleep 0.15; printf '%s\n' '{"text":"ok"}'`), nil
	}
	t.Cleanup(func() { ocrWorkerCommand = prev })

	tEnter := time.Now().Add(ocrBreakerCooldown + 5*time.Millisecond)
	_ = TouchOCRBreakerAllowForTest(tEnter)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = runAppleVisionOCRIsolated([]byte("generic"))
		}()
	}
	wg.Wait()

	if got := int(peakInside.Load()); got != 1 {
		t.Fatalf("peak concurrent worker spawns in half_open = %d, want 1", got)
	}
}

func lookPathOK(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
