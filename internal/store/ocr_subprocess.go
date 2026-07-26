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
	"sync/atomic"
	"time"
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

// ocrConsecutiveAbortLimit is how many consecutive worker aborts are
// tolerated before Apple Vision is disabled for the lifetime of the
// process. Repeated aborts mean the platform's Vision stack is broken,
// not that these particular images are odd, and each one costs a
// process spawn. Disabling converts a systemic fault into a single
// loud message instead of one warning per image forever.
const ocrConsecutiveAbortLimit = 5

// ocrAborts counts consecutive worker aborts; any success resets it.
var ocrAborts atomic.Int64

// ocrDisabled latches once the abort limit is hit.
var ocrDisabled atomic.Bool

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
	if ocrDisabled.Load() {
		return "", nil, fmt.Errorf("apple_vision: disabled after %d consecutive worker aborts",
			ocrConsecutiveAbortLimit)
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

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		noteOCRAbort("timeout")
		return "", nil, fmt.Errorf("apple_vision: worker timed out after %s", ocrWorkerTimeout)
	}
	if runErr != nil {
		// The child died rather than reporting. This is the case the
		// isolation exists for: attribute it to this image, keep the
		// daemon alive, and let the abort counter notice a pattern.
		var ee *exec.ExitError
		detail := runErr.Error()
		if errors.As(runErr, &ee) {
			detail = ee.ProcessState.String() // e.g. "signal: abort trap"
		}
		if msg := firstLine(stderr.String()); msg != "" {
			detail += ": " + msg
		}
		noteOCRAbort(detail)
		return "", nil, fmt.Errorf("apple_vision: worker died (%s)", detail)
	}

	var res ocrWorkerResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		// A clean exit with unreadable output is a protocol fault, not a
		// platform abort, so it does not count toward the abort limit.
		return "", nil, fmt.Errorf("apple_vision: unreadable worker output: %w", err)
	}
	ocrAborts.Store(0)
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

// noteOCRAbort records a worker death and disables Apple Vision once the
// consecutive limit is reached.
func noteOCRAbort(detail string) {
	n := ocrAborts.Add(1)
	if n < ocrConsecutiveAbortLimit {
		slog.Warn("apple vision worker aborted; image skipped, daemon unaffected",
			"consecutive", n, "limit", ocrConsecutiveAbortLimit, "detail", detail)
		return
	}
	if ocrDisabled.CompareAndSwap(false, true) {
		slog.Error("apple vision disabled for this process: worker aborted repeatedly, "+
			"which indicates a broken platform Vision/Metal stack rather than bad images. "+
			"Image OCR will be skipped; set disable_ocr in ~/.mnemo/config.json to silence this.",
			"consecutive", n, "detail", detail)
	}
}
