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

// TestImagesOCRHealthOkWhenEnabled keeps the happy path green so the
// new check does not warn on a healthy daemon.
func TestImagesOCRHealthOkWhenEnabled(t *testing.T) {
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
