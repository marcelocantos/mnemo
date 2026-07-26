// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// resetOCRState clears the process-wide abort latch so tests do not
// leak state into each other.
func resetOCRState(t *testing.T) {
	t.Helper()
	ocrAborts.Store(0)
	ocrDisabled.Store(false)
	t.Cleanup(func() {
		ocrAborts.Store(0)
		ocrDisabled.Store(false)
	})
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

// TestOCRAbortLatchDisablesAfterLimit covers the systemic case: a
// platform whose Vision stack is broken aborts on every image. Isolation
// alone would then spawn a doomed process per image forever, so repeated
// aborts must latch OCR off (🎯T118).
func TestOCRAbortLatchDisablesAfterLimit(t *testing.T) {
	resetOCRState(t)

	for i := 1; i < ocrConsecutiveAbortLimit; i++ {
		noteOCRAbort("signal: abort trap")
		if ocrDisabled.Load() {
			t.Fatalf("disabled early, after %d aborts (limit %d)", i, ocrConsecutiveAbortLimit)
		}
		if ocrBackend() == "" && appleVisionAvailable {
			t.Fatalf("backend withdrawn early, after %d aborts", i)
		}
	}
	noteOCRAbort("signal: abort trap")
	if !ocrDisabled.Load() {
		t.Errorf("expected OCR disabled after %d consecutive aborts", ocrConsecutiveAbortLimit)
	}

	// Once latched, the isolated path refuses without spawning anything.
	if _, _, err := runAppleVisionOCRIsolated([]byte("x")); err == nil ||
		!strings.Contains(err.Error(), "disabled") {
		t.Errorf("expected a disabled error once latched, got %v", err)
	}
	// And the backend selection reflects it, so callers stop trying.
	if appleVisionAvailable && ocrBackend() == "apple_vision" {
		t.Error("apple_vision still selected after the abort latch tripped")
	}
}

// TestOCRSuccessResetsAbortCounter: the latch must track *consecutive*
// aborts, so an occasional bad image never accumulates toward disabling
// an otherwise healthy backend.
func TestOCRSuccessResetsAbortCounter(t *testing.T) {
	resetOCRState(t)

	noteOCRAbort("signal: abort trap")
	noteOCRAbort("signal: abort trap")
	if got := ocrAborts.Load(); got != 2 {
		t.Fatalf("abort count = %d, want 2", got)
	}
	ocrAborts.Store(0) // what a successful worker run does
	noteOCRAbort("signal: abort trap")
	if got := ocrAborts.Load(); got != 1 {
		t.Errorf("abort count = %d after a success, want 1", got)
	}
	if ocrDisabled.Load() {
		t.Error("intermittent aborts must not disable OCR")
	}
}

// TestOCREmptyBytesRejectedWithoutSpawn guards the cheap case: no child
// process for input that cannot possibly recognise.
func TestOCREmptyBytesRejectedWithoutSpawn(t *testing.T) {
	resetOCRState(t)
	if _, _, err := runAppleVisionOCRIsolated(nil); err == nil {
		t.Error("expected an error for empty image bytes")
	}
	if ocrAborts.Load() != 0 {
		t.Error("empty input must not count as a platform abort")
	}
}

// withWorker swaps in a fake OCR worker for the duration of a test.
func withWorker(t *testing.T, script string) {
	t.Helper()
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
	if ocrAborts.Load() != 1 {
		t.Errorf("abort counter = %d, want 1", ocrAborts.Load())
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

	prevTimeout := ocrWorkerTimeout
	ocrWorkerTimeout = 200 * time.Millisecond
	t.Cleanup(func() { ocrWorkerTimeout = prevTimeout })

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
