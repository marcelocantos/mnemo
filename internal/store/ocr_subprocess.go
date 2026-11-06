// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/marcelocantos/mnemo/internal/breaker"
)

// Apple Vision runs in a subprocess, never in the daemon (🎯T118).
//
// Vision does not always fail by returning an error. When the platform's
// Metal/MPS shader stack cannot be loaded it calls abort() inside the
// framework, which raises SIGABRT in whatever process made the call. A
// cgo abort cannot be caught — recover() does not see it — so an
// in-process call makes every undecodable image a potential daemon
// killer. It killed ingest, the compaction watcher, every mirror stream
// and the MCP endpoint on 2026-07-26 for one image, having emitted only
// benign "OCR failed" warnings for hours beforehand.
//
// Isolation is therefore the only containment: the recogniser runs in a
// short-lived child, and an abort becomes an ordinary error on this
// image. The child is this same binary re-executed with a hidden
// subcommand, so there is no second artefact to build, ship or keep in
// version lockstep.

// OCRWorkerSubcommand is the argv[1] that turns this binary into a
// one-shot recogniser: image bytes on stdin, ocrWorkerResult JSON on
// stdout. Hidden because it is an implementation detail of the daemon,
// not a user-facing command.
const OCRWorkerSubcommand = "ocr-worker"

// ocrWorkerTimeout bounds one recognition. A wedged Vision call would
// otherwise hold an OCR slot indefinitely; the child is killed and the
// image recorded as failed. A variable so tests can shrink it.
var ocrWorkerTimeout = 60 * time.Second

// ocrWorkerWaitDelay bounds how long Wait lingers after the worker has
// been killed, waiting for descendants to release the output pipes.
// Without it a worker that forks leaves this call blocked indefinitely
// even though the deadline fired — the timeout would be advisory rather
// than enforced.
var ocrWorkerWaitDelay = 5 * time.Second

// ocrConsecutiveAbortLimit is how many consecutive isolated worker deaths
// (timeout, SIGABRT, etc.) trip the Apple Vision circuit breaker open.
const ocrConsecutiveAbortLimit = 5

// ocrBreakerCooldown is how long Apple Vision stays off after a trip before
// a single half-open recovery trial is permitted.
var ocrBreakerCooldown = 10 * time.Minute

// ocrBreaker gates Apple Vision subprocess work: repeated worker deaths open
// it for a bounded cooldown, then allow one trial (half-open). Success closes
// it; another failure reopens with a fresh cooldown — no restart required.
// Re-built when ocrBreakerCooldown changes (tests) via ResetOCRAbortLatchForTest.
var ocrBreaker = newOCRBreaker()

func newOCRBreaker() *breaker.Breaker {
	return breaker.New(ocrConsecutiveAbortLimit, ocrBreakerCooldown)
}

// ocrWorkerCommand builds the child invocation. A variable so tests can
// inject a worker that aborts, which is the one behaviour that matters
// here and cannot be provoked reliably through the real framework.
var ocrWorkerCommand = func(ctx context.Context) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("apple_vision: locate self: %w", err)
	}
	return exec.CommandContext(ctx, self, OCRWorkerSubcommand), nil
}

// ocrWorkerResult is the child's stdout contract.
type ocrWorkerResult struct {
	Text       string   `json:"text"`
	Confidence *float64 `json:"confidence,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// RunOCRWorker is the child-side entry point. It reads image bytes from
// stdin, runs the native recogniser, and writes one JSON object to
// stdout. It always exits 0 on a *reported* failure — a non-zero exit or
// a signal is reserved for the framework taking the process down, which
// is exactly the signal the parent needs to distinguish.
func RunOCRWorker(stdin io.Reader, stdout io.Writer) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("ocr-worker: read stdin: %w", err)
	}
	var res ocrWorkerResult
	text, confidence, err := runAppleVisionOCRNative(data)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Text, res.Confidence = text, confidence
	}
	return json.NewEncoder(stdout).Encode(res)
}

// runAppleVisionOCRIsolated runs Vision in a child process and maps its
// fate onto an ordinary (text, confidence, error) result.
func runAppleVisionOCRIsolated(imageBytes []byte) (string, *float64, error) {
	if len(imageBytes) == 0 {
		return "", nil, fmt.Errorf("apple_vision: empty image bytes")
	}
	now := time.Now()
	if !ocrBreaker.Allow(now) {
		return "", nil, fmt.Errorf("apple_vision: circuit open — retry after cooldown (%s)", ocrBreakerCooldown)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ocrWorkerTimeout)
	defer cancel()

	cmd, err := ocrWorkerCommand(ctx)
	if err != nil {
		return "", nil, err
	}
	cmd.Stdin = bytes.NewReader(imageBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Cancelling the context kills the worker, but Wait still blocks
	// until every inherited write end of the output pipes is closed —
	// so anything the worker itself spawned can hold this call open long
	// past the deadline, which is precisely the wedge the timeout exists
	// to prevent. WaitDelay caps that: after the kill, give descendants
	// this long to let go, then return regardless.
	cmd.WaitDelay = ocrWorkerWaitDelay

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		noteOCRWorkerDeath("timeout")
		return "", nil, fmt.Errorf("apple_vision: worker timed out after %s", ocrWorkerTimeout)
	}
	if runErr != nil {
		// The child died rather than reporting. This is the case the
		// isolation exists for: attribute it to this image, keep the
		// daemon alive, and let the breaker notice a pattern.
		var ee *exec.ExitError
		detail := runErr.Error()
		if errors.As(runErr, &ee) {
			detail = ee.ProcessState.String() // e.g. "signal: abort trap"
		}
		if msg := firstLine(stderr.String()); msg != "" {
			detail += ": " + msg
		}
		noteOCRWorkerDeath(detail)
		return "", nil, fmt.Errorf("apple_vision: worker died (%s)", detail)
	}

	var res ocrWorkerResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		// A clean exit with unreadable output is a protocol fault, not a
		// platform abort, so it does not count toward the breaker.
		return "", nil, fmt.Errorf("apple_vision: unreadable worker output: %w", err)
	}
	ocrBreaker.Record(time.Now(), true, "")
	if res.Error != "" {
		return "", nil, fmt.Errorf("%s", res.Error)
	}
	return res.Text, res.Confidence, nil
}

// ocrDisabledByConfig reports the user's explicit off switch. Read per
// call (config is cheap and hot-reloadable) so flipping disable_ocr
// takes effect without a daemon restart; an unreadable config leaves
// OCR enabled, matching every other optional feature's default.
func ocrDisabledByConfig() bool {
	cfg, err := LoadConfig()
	if err != nil {
		return false
	}
	return cfg.DisableOCR
}

// noteOCRWorkerDeath records one isolated worker death on the circuit breaker.
// Duplicate concurrent attempts for the same image are prevented by singleflight
// in ocrAndStore, so each death here is one logical failure toward cooldown.
func noteOCRWorkerDeath(detail string) {
	now := time.Now()
	ocrBreaker.Record(now, false, detail)
	snap := ocrBreaker.Snapshot()
	switch snap.State {
	case breaker.StateOpen:
		slog.Error("apple vision OCR circuit breaker OPEN: repeated worker deaths — cooling down before one recovery trial",
			"cooldown", ocrBreakerCooldown, "last_error", snap.LastError, "trips", snap.TripCount)
	case breaker.StateHalfOpen:
		slog.Warn("apple vision worker died during half-open recovery trial",
			"detail", detail, "will_reopen", true)
	default:
		slog.Warn("apple vision worker aborted; image skipped, daemon unaffected",
			"consecutive_failures", snap.Failures, "threshold", ocrConsecutiveAbortLimit, "detail", detail)
	}
}

// noteOCRAbort is the legacy name kept for tests; it matches noteOCRWorkerDeath.
func noteOCRAbort(detail string) {
	noteOCRWorkerDeath(detail)
}

// OCRProcessSnapshot backs images.ocr /health: atomics + one breaker snapshot only.
type OCRProcessSnapshot struct {
	// Disabled is true when Apple Vision subprocess OCR is not offered
	// (config off, breaker open in cooldown, or non-darwin without tess).
	Disabled bool `json:"disabled"`
	// Reason is machine-readable: ok, config, apple_vision_breaker, unavailable, ...
	Reason string `json:"reason"`
	// Detail is the human-facing expansion of Reason.
	Detail string `json:"detail"`
	// BreakerState is set when Apple Vision uses the subprocess breaker.
	BreakerState string `json:"breaker_state,omitempty"`
	// ConsecutiveFailures is the breaker's consecutive failure count while closed.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// OpenedAt is when the breaker last tripped open (cooldown anchor).
	OpenedAt time.Time `json:"opened_at,omitempty"`
	// CooldownRemaining is positive while the breaker is open before half-open.
	CooldownRemaining time.Duration `json:"cooldown_remaining,omitempty"`
}

// OCRProcessStatus reports Apple Vision + subprocess breaker state for /health.
func OCRProcessStatus() OCRProcessSnapshot {
	if ocrDisabledByConfig() {
		return OCRProcessSnapshot{
			Disabled: true,
			Reason:   "config",
			Detail:   "image OCR disabled by config (disable_ocr)",
		}
	}

	now := time.Now()
	snap := ocrBreaker.Snapshot()
	switch snap.State {
	case breaker.StateOpen:
		rem := ocrBreakerCooldown - now.Sub(snap.OpenedAt)
		if rem < 0 {
			rem = 0
		}
		return OCRProcessSnapshot{
			Disabled:            true,
			Reason:              "apple_vision_breaker",
			BreakerState:        string(snap.State),
			ConsecutiveFailures: snap.Failures,
			OpenedAt:            snap.OpenedAt,
			CooldownRemaining:   rem,
			Detail: fmt.Sprintf(
				"Apple Vision OCR paused after repeated worker failures — cooldown %s remaining (opened at %s); last error: %s",
				rem.Round(time.Second), snap.OpenedAt.UTC().Format(time.RFC3339), snap.LastError,
			),
		}
	case breaker.StateHalfOpen:
		return OCRProcessSnapshot{
			Reason:       "apple_vision_breaker",
			BreakerState: string(snap.State),
			Detail:       "Apple Vision OCR attempting one recovery trial after cooldown",
		}
	}

	if !appleVisionAvailable {
		detail := "no OCR backend available (Apple Vision is macOS-only, tesseract not installed)"
		if _, err := exec.LookPath("tesseract"); err == nil {
			detail = "Apple Vision unavailable on this platform; using tesseract"
		}
		return OCRProcessSnapshot{
			Reason: "unavailable",
			Detail: detail,
		}
	}

	return OCRProcessSnapshot{
		Reason:              "ok",
		Detail:              "apple vision OCR enabled",
		BreakerState:        string(breaker.StateClosed),
		ConsecutiveFailures: snap.Failures,
	}
}

// ResetOCRAbortLatchForTest resets breaker state (tests only).
func ResetOCRAbortLatchForTest() {
	ocrBreaker = newOCRBreaker()
}

// TripOCRAbortLatchForTest trips the breaker as after N consecutive worker deaths (tests only).
func TripOCRAbortLatchForTest(detail string) {
	ResetOCRAbortLatchForTest()
	t0 := time.Now()
	for i := 0; i < ocrConsecutiveAbortLimit; i++ {
		ocrBreaker.Record(t0, false, detail)
		t0 = t0.Add(time.Millisecond)
	}
}
