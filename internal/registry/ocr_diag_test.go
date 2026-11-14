// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

func freezeOCRClock(t *testing.T, now time.Time) {
	t.Helper()
	t.Cleanup(store.OCRBreakerTestLock())
	store.ResetOCRAbortLatchForTest()
	t.Cleanup(store.ResetOCRAbortLatchForTest)
	store.SetOCRNowForTest(now)
	t.Cleanup(func() { store.SetOCRNowForTest(time.Time{}) })
}

func imagesOCRResult(t *testing.T, r *Registry) (severity, detail, remediation, tier string) {
	t.Helper()
	rep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), false, time.Now())
	for _, res := range rep.Results {
		if res.Name == "images.ocr" {
			return res.Severity, res.Detail, res.Remediation, res.Tier
		}
	}
	t.Fatal("images.ocr missing from /health report")
	return "", "", "", ""
}

func registryWithStore(t *testing.T) *Registry {
	t.Helper()
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
	return r
}

// TestImagesOCRHealthWarnsWhenBreakerOpen trips the Apple Vision breaker
// open and asserts images.ocr warns with why; remediation documents cooldown
// (not restart-only).
func TestImagesOCRHealthWarnsWhenBreakerOpen(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	freezeOCRClock(t, t0)
	store.TripOCRAbortLatchForTest("timeout")

	r := registryWithStore(t)
	sev, detail, rem, tier := imagesOCRResult(t, r)
	if sev != "warn" {
		t.Fatalf("images.ocr severity=%s detail=%s, want warn", sev, detail)
	}
	if tier != "fast" {
		t.Errorf("images.ocr tier=%s, want fast", tier)
	}
	if !strings.Contains(detail, "timeout") && !strings.Contains(detail, "last error") {
		t.Errorf("detail missing failure context: %q", detail)
	}
	if !strings.Contains(detail, "circuit open") {
		t.Errorf("detail missing open phase: %q", detail)
	}
	if !strings.Contains(rem, "cooldown") {
		t.Errorf("remediation should document cooldown recovery, got %q", rem)
	}
}

// TestImagesOCRHealthWarnsWhenBreakerHalfOpen: after cooldown elapses,
// images.ocr still warns (probing / next trial). Timer expiry is not recovery.
func TestImagesOCRHealthWarnsWhenBreakerHalfOpen(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	freezeOCRClock(t, t0)
	store.TripOCRAbortLatchForTest("timeout")
	store.SetOCRNowForTest(t0.Add(store.OCRBreakerCooldownForTest()))

	r := registryWithStore(t)
	sev, detail, rem, _ := imagesOCRResult(t, r)
	if sev != "warn" {
		t.Fatalf("images.ocr severity=%s detail=%s, want warn in half_open", sev, detail)
	}
	if !strings.Contains(detail, "half_open") {
		t.Errorf("detail should mention half_open probing: %q", detail)
	}
	if !strings.Contains(rem, "successful trial") {
		t.Errorf("remediation should say expiry alone is not recovery: %q", rem)
	}
}

// TestImagesOCRHealthSequenceOpenToClosed exercises open WARN → half-open WARN → closed OK
// after a successful recovery Record (without running native Vision).
func TestImagesOCRHealthSequenceOpenToClosed(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	freezeOCRClock(t, t0)
	store.TripOCRAbortLatchForTest("boom")

	r := registryWithStore(t)
	sev, detail, _, _ := imagesOCRResult(t, r)
	if sev != "warn" || !strings.Contains(detail, "circuit open (cooldown active)") {
		t.Fatalf("want images.ocr warn while breaker open, got %s %s", sev, detail)
	}

	tHalf := t0.Add(store.OCRBreakerCooldownForTest())
	store.SetOCRNowForTest(tHalf)
	if !store.TouchOCRBreakerAllowForTest(tHalf) {
		t.Fatal("expected transition to half_open after cooldown")
	}
	sev, detail, _, _ = imagesOCRResult(t, r)
	if sev != "warn" || !strings.Contains(detail, "half_open") {
		t.Fatalf("want images.ocr warn in half_open (probing), got %s %s", sev, detail)
	}

	store.RecordOCRBreakerTrialSuccessForTest(tHalf)
	sev, detail, _, _ = imagesOCRResult(t, r)
	if sev != "ok" {
		t.Fatalf("after successful trial want ok, got %s %s", sev, detail)
	}
}

// TestImagesOCRHealthSequenceFailedTrialReopens: half-open failure → open WARN again.
func TestImagesOCRHealthSequenceFailedTrialReopens(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	freezeOCRClock(t, t0)
	store.TripOCRAbortLatchForTest("boom")
	tHalf := t0.Add(store.OCRBreakerCooldownForTest())
	store.SetOCRNowForTest(tHalf)
	if !store.TouchOCRBreakerAllowForTest(tHalf) {
		t.Fatal("expected half_open")
	}

	r := registryWithStore(t)
	store.RecordOCRBreakerTrialFailureForTest(tHalf, "half_open_fail")

	sev, detail, _, _ := imagesOCRResult(t, r)
	if sev != "warn" || !strings.Contains(detail, "circuit open") {
		t.Fatalf("failed trial must reopen with open WARN, got %s %s", sev, detail)
	}
}

// TestImagesOCRHealthOkWhenEnabled keeps the happy path green so the
// new check does not warn on a healthy daemon.
func TestImagesOCRHealthOkWhenEnabled(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	freezeOCRClock(t, t0)

	r := registryWithStore(t)
	sev, detail, _, _ := imagesOCRResult(t, r)
	if sev != "ok" {
		t.Fatalf("images.ocr want ok when enabled, got %s %s", sev, detail)
	}
}

// TestImagesOCRHealthOkWhenConfigDisabled: deliberate disable_ocr stays healthy.
func TestImagesOCRHealthOkWhenConfigDisabled(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	freezeOCRClock(t, t0)

	home := t.TempDir()
	t.Setenv(store.MnemoHomeEnv, home)
	if err := os.MkdirAll(filepath.Join(home, ".mnemo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mnemo", "config.json"),
		[]byte(`{"disable_ocr": true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	r := registryWithStore(t)
	sev, detail, _, _ := imagesOCRResult(t, r)
	if sev != "ok" {
		t.Fatalf("config-disabled OCR must stay healthy, got %s %s", sev, detail)
	}
	if !strings.Contains(detail, "disable_ocr") && !strings.Contains(detail, "config") {
		t.Errorf("detail should name config disable, got %q", detail)
	}
}
