// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && system_test

package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestForwardPathOCR verifies the forward-path trigger: when an image
// arrives at ingest time, OCR fires immediately (no ticker wait). This
// exercises triggerImageSidecars end-to-end through the semaphore pool.
//
// The describer and embedder triggers are also fired, but we only assert
// on OCR here because it is fast (~100ms), deterministic, and local.
// Description and embedding are covered by their own golden tests.
//
// Run with:
//
//	go test -tags "sqlite_fts5 system_test" -run TestForwardPathOCR -v -timeout 1m ./internal/store/
func TestForwardPathOCR(t *testing.T) {
	tmp := t.TempDir()
	s, err := New(filepath.Join(tmp, "mnemo.sqlite"), filepath.Join(tmp, "projects"))
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	defer s.Close()

	// Simulate a fresh image arriving via ingestImageFromPath.
	png, err := filepath.Abs("testdata/images/error_log.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(png); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	ingestImageFromPath(s, png, 0, 0, "test-session", ts)

	// Poll for OCR completion (up to 10s). The trigger spawned a
	// goroutine; no ticker, so we expect near-immediate processing.
	deadline := time.Now().Add(10 * time.Second)
	var text sql.NullString
	var errCol sql.NullString
	for time.Now().Before(deadline) {
		err := s.db.QueryRow(`
			SELECT text, error FROM image_ocr
			WHERE image_id = (SELECT id FROM images ORDER BY id DESC LIMIT 1)
		`).Scan(&text, &errCol)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !text.Valid && !errCol.Valid {
		t.Fatal("OCR row never appeared — forward-path trigger did not fire")
	}
	if errCol.Valid && errCol.String != "" {
		t.Fatalf("OCR error: %s", errCol.String)
	}
	if text.String == "" {
		t.Fatal("OCR produced empty text for fixture")
	}
	// Sanity: expected distinctive token from this fixture.
	if !contains(text.String, "ECONNREFUSED") {
		t.Errorf("expected ECONNREFUSED in OCR output, got: %s", text.String)
	} else {
		t.Logf("forward-path OCR completed successfully: %d chars", len(text.String))
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, substr string) int {
	n, m := len(s), len(substr)
	if m == 0 {
		return 0
	}
	for i := 0; i <= n-m; i++ {
		if s[i:i+m] == substr {
			return i
		}
	}
	return -1
}
