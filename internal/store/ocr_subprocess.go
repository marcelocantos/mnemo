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
	"sync"
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

// ocrNow is the OCR breaker/health clock. Tests freeze it so cooldown
// transitions do not require sleeping.
var ocrNow = time.Now

// ocrBreaker gates Apple Vision subprocess work: repeated worker deaths open
// it for a bounded cooldown, then allow one trial (half-open). Success closes
// it; another failure reopens with a fresh cooldown — no restart required.
// Re-built when ocrBreakerCooldown changes (tests) via ResetOCRAbortLatchForTest.
var ocrBreaker = newOCRBreaker()

// ocrHalfOpenTrial serializes Apple Vision work while the breaker is half-open
// so at most one recovery/probing subprocess runs at a time (ShouldAttempt stays
// true for half-open, so without this many waiters could start parallel trials).
var ocrHalfOpenTrial sync.Mutex

// ocrBreakerTestMu serializes tests that mutate the process-wide OCR breaker
// (shared across packages in the same test binary).
var ocrBreakerTestMu sync.Mutex

// OCRBreakerTestLock acquires ocrBreakerTestMu; returned unlock must run in t.Cleanup (tests only).
func OCRBreakerTestLock() (unlock func()) {
	ocrBreakerTestMu.Lock()
	return ocrBreakerTestMu.Unlock
}

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
	now := ocrNow()
	if !ocrBreaker.Allow(now) {
		return "", nil, fmt.Errorf("apple_vision: circuit open — retry after cooldown (%s)", ocrBreakerCooldown)
	}
	if ocrBreaker.Snapshot().State == breaker.StateHalfOpen {
		if !ocrHalfOpenTrial.TryLock() {
			return "", nil, fmt.Errorf("apple_vision: recovery trial already in progress")
		}
		defer ocrHalfOpenTrial.Unlock()
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
	if res.Error != "" {
		// Worker exited cleanly but reported failure — still a completed trial;
		// fold into the breaker (half-open failure re-opens; closed increments
		// toward threshold). Do not treat as success.
		ocrBreaker.Record(ocrNow(), false, res.Error)
		return "", nil, fmt.Errorf("%s", res.Error)
	}
	ocrBreaker.Record(ocrNow(), true, "")
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
	now := ocrNow()
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

// OCRProcessSnapshot backs images.ocr /health: in-memory breaker snapshot only (fast tier).
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
	// OpenedAt is when the breaker last tripped open (cooldown anchor; "trip time").
	OpenedAt time.Time `json:"opened_at,omitempty"`
	// CooldownRemaining is positive while the breaker is open before half-open.
	CooldownRemaining time.Duration `json:"cooldown_remaining,omitempty"`
	// NextTrialAt is when the first recovery attempt is permitted (OpenedAt+cooldown)
	// while open; while half-open it is the start of the probing window (RFC3339 in Detail).
	NextTrialAt time.Time `json:"next_trial_at,omitempty"`
	// LastError is the breaker's last failure detail (worker death or reported error).
	LastError string `json:"last_error,omitempty"`
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

	now := ocrNow()
	snap := ocrBreaker.Snapshot()
	switch snap.State {
	case breaker.StateOpen:
		rem := ocrBreakerCooldown - now.Sub(snap.OpenedAt)
		if rem < 0 {
			rem = 0
		}
		nextAt := snap.OpenedAt.Add(ocrBreakerCooldown)
		if rem <= 0 {
			// Cooldown window has expired. The underlying breaker still reports "open"
			// until Allow() runs — for diagnostics we surface the effective half_open
			// probing phase so /health matches next-trial semantics.
			return OCRProcessSnapshot{
				Reason:              "apple_vision_breaker",
				BreakerState:        string(breaker.StateHalfOpen),
				ConsecutiveFailures: snap.Failures,
				OpenedAt:            snap.OpenedAt,
				CooldownRemaining:   0,
				NextTrialAt:         nextAt,
				LastError:           snap.LastError,
				Detail: fmt.Sprintf(
					"half_open (probing): next trial state is an in-process Apple Vision attempt after cooldown ended at %s; "+
						"trip at %s; last error: %s; successful trial closes the breaker; "+
						"expiry of cooldown alone is not recovery; a failed trial reopens with a new bounded cooldown (%s)",
					nextAt.UTC().Format(time.RFC3339),
					snap.OpenedAt.UTC().Format(time.RFC3339),
					snap.LastError,
					ocrBreakerCooldown,
				),
			}
		}
		return OCRProcessSnapshot{
			Disabled:            true,
			Reason:              "apple_vision_breaker",
			BreakerState:        string(snap.State),
			ConsecutiveFailures: snap.Failures,
			OpenedAt:            snap.OpenedAt,
			CooldownRemaining:   rem,
			NextTrialAt:         nextAt,
			LastError:           snap.LastError,
			Detail: fmt.Sprintf(
				"circuit open (cooldown active): Apple Vision paused after repeated worker failures; trip at %s; last error: %s; "+
					"cooldown %s remaining; next trial state half_open at %s (expiry alone is not recovery — a successful trial closes the breaker)",
				snap.OpenedAt.UTC().Format(time.RFC3339), snap.LastError,
				rem.Round(time.Second), nextAt.UTC().Format(time.RFC3339),
			),
		}
	case breaker.StateHalfOpen:
		cooldownEnded := snap.OpenedAt.Add(ocrBreakerCooldown)
		return OCRProcessSnapshot{
			Reason:              "apple_vision_breaker",
			BreakerState:        string(snap.State),
			ConsecutiveFailures: snap.Failures,
			OpenedAt:            snap.OpenedAt,
			// Probing window starts when cooldown elapses; same instant a trial may run.
			NextTrialAt: cooldownEnded,
			LastError:   snap.LastError,
			Detail: fmt.Sprintf(
				"half_open (probing): next trial state is an in-process Apple Vision attempt after cooldown ended at %s; "+
					"trip at %s; last error: %s; successful trial closes the breaker; "+
					"expiry of cooldown alone is not recovery; a failed trial reopens with a new bounded cooldown (%s)",
				cooldownEnded.UTC().Format(time.RFC3339),
				snap.OpenedAt.UTC().Format(time.RFC3339),
				snap.LastError,
				ocrBreakerCooldown,
			),
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
		Detail:              "apple vision OCR enabled (breaker closed)",
		BreakerState:        string(breaker.StateClosed),
		ConsecutiveFailures: snap.Failures,
	}
}

// ResetOCRAbortLatchForTest resets breaker state (tests only).
func ResetOCRAbortLatchForTest() {
	ocrBreaker = newOCRBreaker()
}

// TouchOCRBreakerAllowForTest runs Allow on the Apple Vision breaker (tests / health probes only).
func TouchOCRBreakerAllowForTest(now time.Time) bool {
	return ocrBreaker.Allow(now)
}

// TripOCRAbortLatchForTest trips the breaker as after N consecutive worker deaths (tests only).
func TripOCRAbortLatchForTest(detail string) {
	ResetOCRAbortLatchForTest()
	t0 := ocrNow()
	for i := 0; i < ocrConsecutiveAbortLimit; i++ {
		ocrBreaker.Record(t0, false, detail)
	}
}

// SetOCRNowForTest freezes the OCR clock (tests only). Pass zero to restore time.Now.
func SetOCRNowForTest(now time.Time) {
	if now.IsZero() {
		ocrNow = time.Now
		return
	}
	frozen := now
	ocrNow = func() time.Time { return frozen }
}

// OCRBreakerCooldownForTest returns the Apple Vision breaker cooldown (tests only).
func OCRBreakerCooldownForTest() time.Duration {
	return ocrBreakerCooldown
}

// SetOCRBreakerCooldownForTest sets the Apple Vision breaker cooldown and rebuilds the breaker (tests only).
func SetOCRBreakerCooldownForTest(d time.Duration) {
	ocrBreakerCooldown = d
	ocrBreaker = newOCRBreaker()
}

// RecordOCRBreakerTrialSuccessForTest records a successful half-open trial (tests only).
func RecordOCRBreakerTrialSuccessForTest(now time.Time) {
	ocrBreaker.Record(now, true, "")
}

// RecordOCRBreakerTrialFailureForTest records a failed trial (tests only).
func RecordOCRBreakerTrialFailureForTest(now time.Time, detail string) {
	ocrBreaker.Record(now, false, detail)
}
