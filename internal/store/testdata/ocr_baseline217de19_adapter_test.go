// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Adapter-only tests copied into an unmodified 217de19 worktree by
// scripts/ocr-baseline-217de19-proof.sh. They assert the POST-FIX contract
// so they must FAIL with a runtime assertion on 217de19 (duplicate
// concurrent OCR counts each death; abort latch never recovers).
//
// Not compiled in the mnemo module (testdata/).

package store

import (
	"fmt"
	"sync"
	"testing"
)

func TestOCRBaseline217de19_DuplicateConcurrentIsolatedRunsEachCountFailure(t *testing.T) {
	resetOCRState(t)
	requirePOSIXShell(t)
	withWorker(t, "exit 134")

	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(
		`INSERT INTO images (id, content_hash, bytes, mime_type, width, height,
		                     pixel_format, byte_size, created_at)
		 VALUES (1, 'hash-baseline', ?, 'image/png', 10, 10, 'rgba', 10, '2026-07-26T00:00:00Z')`,
		[]byte("generic-bytes"),
	); err != nil {
		t.Fatal(err)
	}

	const n = 3
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ocrAndStore(s.writeDB, 1, []byte("generic-bytes"), "apple_vision")
		}()
	}
	wg.Wait()

	if got := ocrAborts.Load(); got != 1 {
		t.Fatalf("post-fix contract: want 1 logical abort after per-image single-flight, got %d", got)
	}
}

func TestOCRBaseline217de19_RecoversAfterThresholdSuccess(t *testing.T) {
	resetOCRState(t)
	requirePOSIXShell(t)
	withWorker(t, "exit 134")
	s := newTestStore(t, t.TempDir())

	for i := 1; i <= ocrConsecutiveAbortLimit; i++ {
		id := int64(100 + i)
		if _, err := s.writeDB.Exec(
			`INSERT INTO images (id, content_hash, bytes, mime_type, width, height,
			                     pixel_format, byte_size, created_at)
			 VALUES (?, ?, ?, 'image/png', 10, 10, 'rgba', 10, '2026-07-26T00:00:00Z')`,
			id, fmt.Sprintf("hash-rec-%d", i), []byte("generic-bytes"),
		); err != nil {
			t.Fatal(err)
		}
		ocrAndStore(s.writeDB, id, []byte("generic-bytes"), "apple_vision")
	}
	if !ocrDisabled.Load() {
		t.Fatal("setup: latch should be set after threshold")
	}

	withWorker(t, `printf '%s\n' '{"text":"recovered"}'`)
	if _, err := s.writeDB.Exec(
		`INSERT INTO images (id, content_hash, bytes, mime_type, width, height,
		                     pixel_format, byte_size, created_at)
		 VALUES (200, 'hash-rec-ok', ?, 'image/png', 10, 10, 'rgba', 10, '2026-07-26T00:00:00Z')`,
		[]byte("generic-bytes"),
	); err != nil {
		t.Fatal(err)
	}
	ocrAndStore(s.writeDB, 200, []byte("generic-bytes"), "apple_vision")

	if ocrDisabled.Load() {
		t.Fatal("post-fix contract: after a later successful trial OCR must recover; 217de19 abort latch stays disabled")
	}
}
