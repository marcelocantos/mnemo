// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"
)

// TestDedupeEntriesRemovesTheSurplusCopy covers the GC half of 🎯T170:
// the rows already written before the insert path was fixed.
func TestDedupeEntriesRemovesTheSurplusCopy(t *testing.T) {
	projectDir := t.TempDir()
	entries := make([]map[string]any, 0, 6)
	for i := 0; i < 6; i++ {
		e := msg("user", fmt.Sprintf("dedupe line %02d: %s", i, longText("gc", 2)),
			fmt.Sprintf("2026-04-01T10:%02d:00Z", i))
		e["uuid"] = fmt.Sprintf("33333333-0000-4000-8000-%012d", i)
		entries = append(entries, e)
	}
	path := writeJSONL(t, projectDir, "proj", "sess-gc", entries)

	s := newTestStore(t, projectDir)
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	before := entryCountFor(t, s, "sess-gc")
	msgsBefore := messageCountFor(t, s, "sess-gc")

	// Manufacture the duplicates exactly as the bug did: pack, then
	// re-ingest through the pre-fix statement choice.
	if _, err := s.MaterialiseEntries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res, err := s.CompressBackfill(context.Background(), FamilyEntriesRaw); err != nil || !res.Done {
		t.Fatalf("pack: %+v %v", res, err)
	}
	s.codec.ready.Store(false)
	s.forceLegacyInsertShapeForTest()
	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	s.codec.ready.Store(true)
	if got := entryCountFor(t, s, "sess-gc"); got != 2*before {
		t.Fatalf("expected the pre-fix path to duplicate all %d entries, got %d", before, got)
	}

	// A dry run reports and changes nothing.
	dry, err := s.DedupeEntries(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.DuplicateGroups != int64(before) || dry.EntriesRemoved != int64(before) {
		t.Errorf("dry run: groups=%d entries=%d, want %d each", dry.DuplicateGroups, dry.EntriesRemoved, before)
	}
	if dry.MessagesRemoved == 0 {
		t.Error("dry run reported no messages; the fan-out is what makes this expensive")
	}
	if got := entryCountFor(t, s, "sess-gc"); got != 2*before {
		t.Fatalf("dry run deleted rows: %d", got)
	}

	got, err := s.DedupeEntries(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.EntriesRemoved != int64(before) {
		t.Errorf("removed %d entries, want %d", got.EntriesRemoved, before)
	}
	if n := entryCountFor(t, s, "sess-gc"); n != before {
		t.Errorf("entries after dedupe = %d, want %d", n, before)
	}
	if n := messageCountFor(t, s, "sess-gc"); n != msgsBefore {
		t.Errorf("messages after dedupe = %d, want %d", n, msgsBefore)
	}

	// The survivor is the keyed copy: every remaining row must carry the
	// uuid_m the unique index depends on.
	var keyless int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM entries WHERE session_id = 'sess-gc' AND uuid_m IS NULL`).
		Scan(&keyless); err != nil {
		t.Fatal(err)
	}
	if keyless != 0 {
		t.Errorf("%d surviving rows have no uuid_m; dedupe kept the wrong copy", keyless)
	}

	// FTS must not be left pointing at deleted rows. An external-content
	// index with dangling rowids fails integrity-check outright.
	if _, err := s.readDB.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('integrity-check')`); err != nil {
		t.Errorf("messages_fts integrity-check failed after dedupe: %v", err)
	}

	// Idempotent.
	again, err := s.DedupeEntries(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if again.EntriesRemoved != 0 || again.DuplicateGroups != 0 {
		t.Errorf("second pass removed %d entries in %d groups, want 0",
			again.EntriesRemoved, again.DuplicateGroups)
	}
}

func messageCountFor(t *testing.T, s *Store, session string) int {
	t.Helper()
	var n int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, session).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
