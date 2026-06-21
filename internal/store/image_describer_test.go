// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestHasPendingImageWork covers the 🎯T91 gate predicate that suppresses
// the describer/OCR worker fan-out when there is nothing to do: it reports
// pending work only while an image lacks a row in the derived table, and
// defensively reports pending for an unknown table so work is never
// silently dropped.
func TestHasPendingImageWork(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Empty images table → nothing pending.
	if hasPendingImageWork(s.readDB, "image_descriptions") {
		t.Fatalf("empty images table should report no pending work")
	}

	id, err := storeImage(s.writeDB, []byte("fake-image-bytes"), "image/png", "")
	if err != nil {
		t.Fatalf("storeImage: %v", err)
	}

	// Image with no description row → pending.
	if !hasPendingImageWork(s.readDB, "image_descriptions") {
		t.Fatalf("undescribed image should report pending work")
	}

	// Once a description row exists, the queue is drained → not pending.
	if _, err := s.writeDB.Exec(
		`INSERT INTO image_descriptions (image_id, name, description, model, error)
		 VALUES (?, '', 'desc', 'test', '')`, id); err != nil {
		t.Fatalf("insert description: %v", err)
	}
	if hasPendingImageWork(s.readDB, "image_descriptions") {
		t.Fatalf("fully-described images should report no pending work")
	}

	// The same image is still pending OCR (independent derived table).
	if !hasPendingImageWork(s.readDB, "image_ocr") {
		t.Fatalf("image without an OCR row should report pending OCR work")
	}

	// Unknown table → defensively pending (never suppress work).
	if !hasPendingImageWork(s.readDB, "bogus_table") {
		t.Fatalf("unknown table should defensively report pending work")
	}
}
