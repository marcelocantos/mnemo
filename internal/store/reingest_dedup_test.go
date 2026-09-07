// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// TestReingestAfterPackingDoesNotDuplicate is the regression test for
// 🎯T170.
//
// The existing re-ingest tests all run against unpacked rows, which is
// why they never caught this: packing is what opens the hole. Once a row
// is packed its raw is NULL, so the generated uuid column is NULL too and
// the row drops out of idx_entries_session_uuid — a partial index, WHERE
// uuid IS NOT NULL. From that moment the pair is guarded only by uuid_m,
// and any insert that does not set uuid_m conflicts with neither index.
func TestReingestAfterPackingDoesNotDuplicate(t *testing.T) {
	projectDir := t.TempDir()
	entries := make([]map[string]any, 0, 12)
	for i := 0; i < 12; i++ {
		e := msg("user",
			fmt.Sprintf("reingest line %02d: %s", i, longText("dedup", 2)),
			fmt.Sprintf("2026-04-01T10:%02d:00Z", i))
		// A real transcript line carries a uuid; that is the dedup key,
		// and without one there is nothing for either index to match on.
		e["uuid"] = fmt.Sprintf("11111111-0000-4000-8000-%012d", i)
		entries = append(entries, e)
	}
	path := writeJSONL(t, projectDir, "proj", "sess-reingest", entries)

	s := newTestStore(t, projectDir)
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	before := entryCountFor(t, s, "sess-reingest")
	if before == 0 {
		t.Fatal("nothing ingested; the test would prove nothing")
	}

	// Pack the session. This is the step the other re-ingest tests skip.
	if _, err := s.MaterialiseEntries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res, err := s.CompressBackfill(context.Background(), FamilyEntriesRaw); err != nil || !res.Done {
		t.Fatalf("pack: %+v %v", res, err)
	}
	var packed int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM entries WHERE session_id = 'sess-reingest' AND raw_z IS NOT NULL`).
		Scan(&packed); err != nil {
		t.Fatal(err)
	}
	if packed == 0 {
		t.Fatal("no row was packed; the condition this test guards was not reached")
	}

	// Re-read the file from the top, as a watcher would after the offset
	// cursor is lost (a restart on a truncated/rewritten file, a manual
	// reindex) — and do it in the window before the codec is ready, which
	// is where the live duplication happened. This is the discriminating
	// step: with the statement chosen by codec readiness the re-ingest
	// runs the legacy insert, which binds no uuid_m, so it conflicts with
	// neither index — idx_entries_session_uuid because the packed
	// original's uuid is NULL, idx_entries_session_uuid_m because the
	// incoming uuid_m is NULL — and the row lands a second time. The
	// AFTER INSERT trigger that would have set uuid_m then collides and
	// is swallowed by the statement's own OR IGNORE, leaving the
	// duplicate keyless.
	s.codec.ready.Store(false)
	defer s.codec.ready.Store(true)

	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}

	if got := entryCountFor(t, s, "sess-reingest"); got != before {
		t.Errorf("entries after re-ingesting a packed session = %d, want %d (%d duplicates)",
			got, before, got-before)
	}
}

// TestIngestBeforeCodecReadyStillSetsUuidM guards the half of the 🎯T170
// change that could have gone wrong in the other direction: choosing the
// modern statements by schema shape must not smuggle compression into a
// window that has no codec.
//
// It does NOT discriminate the dedup fix — the entries_materialise
// trigger fills uuid_m on a legacy insert too, whenever there is no
// conflict to swallow, so this passes either way. The test with teeth is
// TestReingestAfterPackingDoesNotDuplicate, which needs a packed original
// for the trigger's update to collide against.
func TestIngestBeforeCodecReadyStillSetsUuidM(t *testing.T) {
	projectDir := t.TempDir()
	early := msg("user", "before the codec is ready", "2026-04-01T10:00:00Z")
	early["uuid"] = "22222222-0000-4000-8000-000000000001"
	late := msg("assistant", "still needs a dedup key", "2026-04-01T10:01:00Z")
	late["uuid"] = "22222222-0000-4000-8000-000000000002"
	path := writeJSONL(t, projectDir, "proj", "sess-early", []map[string]any{early, late})

	s := newTestStore(t, projectDir)
	s.codec.ready.Store(false) // the window between store open and CapCodecReady
	defer s.codec.ready.Store(true)

	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}

	var rows, keyed int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*), COUNT(uuid_m) FROM entries WHERE session_id = 'sess-early'`).
		Scan(&rows, &keyed); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("nothing ingested")
	}
	if keyed != rows {
		t.Errorf("%d of %d rows have a NULL uuid_m; those rows cannot be deduped", rows-keyed, rows)
	}

	// And the rows are plain, because the codec was not ready — the fix
	// must not smuggle compression into a window that has no codec.
	var packed int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM entries WHERE session_id = 'sess-early' AND raw_z IS NOT NULL`).
		Scan(&packed); err != nil {
		t.Fatal(err)
	}
	if packed != 0 {
		t.Errorf("%d rows packed while the codec was not ready", packed)
	}
}

func entryCountFor(t *testing.T, s *Store, session string) int {
	t.Helper()
	var n int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM entries_v WHERE session_id = ?`, session).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCursorReingestAfterPackingDoesNotDuplicate is the same guard as
// TestReingestAfterPackingDoesNotDuplicate, driven through the Cursor
// path (🎯T170).
//
// It is worth a separate test rather than an assertion of symmetry:
// Cursor entries carry a synthetic uuid — cursor-<session>-<content
// hash> — rather than one the provider assigned, and the live duplicates
// were all in a Cursor session. The dedup key has to hold for a
// manufactured uuid exactly as for a native one.
func TestCursorReingestAfterPackingDoesNotDuplicate(t *testing.T) {
	home := t.TempDir()
	path := writeCursorSession(t, home, "/Users/dev/work/github.com/acme/webapp")

	s := newTestStore(t, t.TempDir())
	s.SetCursorRoots([]string{filepath.Join(home, "projects")})
	if err := s.ingestCursorFile(path); err != nil {
		t.Fatal(err)
	}
	before := entryCountFor(t, s, cursorSessUUID)
	if before == 0 {
		t.Fatal("nothing ingested from the Cursor session")
	}

	if _, err := s.MaterialiseEntries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res, err := s.CompressBackfill(context.Background(), FamilyEntriesRaw); err != nil || !res.Done {
		t.Fatalf("pack: %+v %v", res, err)
	}

	// Re-ingest through the pre-fix statement choice, which is what let
	// the duplicates land on the live store.
	s.codec.ready.Store(false)
	defer s.codec.ready.Store(true)
	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	if err := s.ingestCursorFile(path); err != nil {
		t.Fatal(err)
	}

	if got := entryCountFor(t, s, cursorSessUUID); got != before {
		t.Errorf("entries after re-ingesting a packed Cursor session = %d, want %d (%d duplicates)",
			got, before, got-before)
	}
}
