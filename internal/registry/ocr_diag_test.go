// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestImagesOCRHealthWarnsWhenBreakerOpen trips the Apple Vision breaker
// open and asserts images.ocr warns with why; remediation documents cooldown
// (not restart-only).
func TestImagesOCRHealthWarnsWhenBreakerOpen(t *testing.T) {
	t.Cleanup(store.OCRBreakerTestLock())
	store.ResetOCRAbortLatchForTest()
	t.Cleanup(store.ResetOCRAbortLatchForTest)

	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "t.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	store.TripOCRAbortLatchForTest("timeout")

	r := NewRegistry(context.Background(), store.Config{}, dir)
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	rep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), false, time.Now())
	var saw bool
	for _, res := range rep.Results {
		if res.Name != "images.ocr" {
			continue
		}
		saw = true
		if res.Severity != "warn" {
			t.Fatalf("images.ocr severity=%s detail=%s, want warn", res.Severity, res.Detail)
		}
		if res.Tier != "fast" {
			t.Errorf("images.ocr tier=%s, want fast", res.Tier)
		}
		if !strings.Contains(res.Detail, "timeout") && !strings.Contains(res.Detail, "last error") {
			t.Errorf("detail missing failure context: %q", res.Detail)
		}
		if !strings.Contains(res.Remediation, "cooldown") {
			t.Errorf("remediation should document cooldown recovery, got %q", res.Remediation)
		}
	}
	if !saw {
		t.Fatal("images.ocr missing from /health report")
	}
}

// TestImagesOCRHealthWarnsWhenBreakerHalfOpen path: after cooldown elapses, the breaker is
// half_open — images.ocr still warns (probing / next trial state in detail).
func TestImagesOCRHealthWarnsWhenBreakerHalfOpen(t *testing.T) {
	t.Cleanup(store.OCRBreakerTestLock())
	store.ResetOCRAbortLatchForTest()
	t.Cleanup(store.ResetOCRAbortLatchForTest)

	prevCD := store.OCRBreakerCooldownForTest()
	store.SetOCRBreakerCooldownForTest(50 * time.Millisecond)
	t.Cleanup(func() { store.SetOCRBreakerCooldownForTest(prevCD) })

	store.TripOCRAbortLatchForTest("timeout")
	time.Sleep(ocrBreakerHalfOpenSleep(store.OCRBreakerCooldownForTest())) // → half_open

	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "t.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	r := NewRegistry(context.Background(), store.Config{}, dir)
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	rep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), false, time.Now())
	var saw bool
	for _, res := range rep.Results {
		if res.Name != "images.ocr" {
			continue
		}
		saw = true
		if res.Severity != "warn" {
			t.Fatalf("images.ocr severity=%s detail=%s, want warn in half_open", res.Severity, res.Detail)
		}
		if !strings.Contains(res.Detail, "half_open") {
			t.Errorf("detail should mention half_open probing: %q", res.Detail)
		}
		if !strings.Contains(res.Remediation, "successful trial") {
			t.Errorf("remediation should say expiry alone is not recovery: %q", res.Remediation)
		}
	}
	if !saw {
		t.Fatal("images.ocr missing from /health report")
	}
}

func ocrBreakerHalfOpenSleep(cd time.Duration) time.Duration {
	return cd + 30*time.Millisecond
}

// TestImagesOCRHealthSequenceOpenToClosed exercises open WARN → half-open WARN → closed OK
// after a successful recovery Record (without running native Vision).
func TestImagesOCRHealthSequenceOpenToClosed(t *testing.T) {
	t.Cleanup(store.OCRBreakerTestLock())
	store.ResetOCRAbortLatchForTest()
	t.Cleanup(store.ResetOCRAbortLatchForTest)

	prevCD := store.OCRBreakerCooldownForTest()
	// Cooldown must exceed temp-store startup so the first images.ocr sample is still
	// in the true "open (cooldown active)" phase (not yet effective half_open).
	store.SetOCRBreakerCooldownForTest(500 * time.Millisecond)
	t.Cleanup(func() { store.SetOCRBreakerCooldownForTest(prevCD) })

	store.TripOCRAbortLatchForTest("boom")

	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "t.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	r := NewRegistry(context.Background(), store.Config{}, dir)
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	openRep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), false, time.Now())
	var openWarn bool
	for _, res := range openRep.Results {
		if res.Name == "images.ocr" && res.Severity == "warn" && strings.Contains(res.Detail, "circuit open (cooldown active)") {
			openWarn = true
		}
	}
	if !openWarn {
		t.Fatal("want images.ocr warn while breaker open")
	}

	time.Sleep(ocrBreakerHalfOpenSleep(store.OCRBreakerCooldownForTest()))
	tHalf := time.Now()
	if !store.TouchOCRBreakerAllowForTest(tHalf) {
		t.Fatal("expected transition to half_open after cooldown")
	}
	halfRep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), false, time.Now())
	var halfWarn bool
	for _, res := range halfRep.Results {
		if res.Name == "images.ocr" && res.Severity == "warn" && strings.Contains(res.Detail, "half_open") {
			halfWarn = true
		}
	}
	if !halfWarn {
		t.Fatal("want images.ocr warn in half_open (probing)")
	}

	store.RecordOCRBreakerTrialSuccessForTest(tHalf)

	finalRep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), false, time.Now())
	for _, res := range finalRep.Results {
		if res.Name == "images.ocr" && res.Severity != "ok" {
			t.Fatalf("after successful trial want ok, got %s %s", res.Severity, res.Detail)
		}
	}
}

// TestImagesOCRHealthOkWhenEnabled keeps the happy path green so the
// new check does not warn on a healthy daemon.
func TestImagesOCRHealthOkWhenEnabled(t *testing.T) {
	t.Cleanup(store.OCRBreakerTestLock())
	store.ResetOCRAbortLatchForTest()
	t.Cleanup(store.ResetOCRAbortLatchForTest)

	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "t.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	r := NewRegistry(context.Background(), store.Config{}, dir)
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	rep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), false, time.Now())
	for _, res := range rep.Results {
		if res.Name == "images.ocr" && res.Severity != "ok" {
			t.Fatalf("images.ocr want ok when enabled, got %s %s", res.Severity, res.Detail)
		}
	}
}
