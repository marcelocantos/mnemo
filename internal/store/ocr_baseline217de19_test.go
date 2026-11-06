// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build ocr_baseline217de19

// Replays @217de19-style duplicate concurrent worker deaths (no per-image singleflight).
//
// Run (meaningful FAIL on baseline semantics — adapter expects post-fix behaviour):
//
//	GOWORK=off go test -tags 'sqlite_fts5,ocr_baseline217de19' ./internal/store -run TestOCRBaseline217de19 -count=1
//
// Default build omits this file.

package store

import (
	"context"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestOCRBaseline217de19_DuplicateConcurrentIsolatedRunsEachCountFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	resetOCRState(t)

	prevCD := ocrBreakerCooldown
	ocrBreakerCooldown = time.Hour
	t.Cleanup(func() { ocrBreakerCooldown = prevCD })

	prevCmd := ocrWorkerCommand
	ocrWorkerCommand = func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", "exit 134"), nil
	}
	t.Cleanup(func() { ocrWorkerCommand = prevCmd })

	const n = 3
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = runAppleVisionOCRIsolated([]byte("generic-image-bytes"))
		}()
	}
	wg.Wait()

	if got := ocrBreaker.Snapshot().Failures; got != 1 {
		t.Fatalf("want 1 logical isolated-death toward breaker (post-fix), got %d (217de19-style duplicates count each)", got)
	}
}
