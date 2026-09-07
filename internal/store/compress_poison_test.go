// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestBackfillSkipsACollidingRowInsteadOfStranding reproduces the live
// failure behind 🎯T169 and pins the fix.
//
// entries.raw's backfill writes the materialised twins alongside the
// compressed blob. When a duplicate row was ingested while uuid_m was
// still NULL — SQLite's unique index treats NULLs as distinct, so nothing
// stopped the second insert — materialising the twin makes the pair
// collide. On the live store that aborted the whole family on every
// two-minute cycle for hours: rows=0, cursor frozen at 14827588, the
// remaining millions of rows never packed, and VACUUM gated behind a
// backfill that could never report done.
//
// One unwritable row must cost exactly one unwritable row.
func TestBackfillSkipsACollidingRowInsteadOfStranding(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	const good = 20
	seedLegacyEntries(t, s, "sess-poison", good)

	if _, err := s.MaterialiseEntries(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Pack the seeded rows. This is the step that opens the hole: raw goes
	// NULL, so the generated uuid column goes NULL with it, and the row
	// drops out of idx_entries_session_uuid — a partial index, WHERE uuid
	// IS NOT NULL. From here only uuid_m guards the pair.
	if first, err := s.CompressBackfill(context.Background(), FamilyEntriesRaw); err != nil || !first.Done {
		t.Fatalf("initial pack: %+v %v", first, err)
	}

	// Now a re-ingest of an entry already held. INSERT OR IGNORE finds no
	// conflict, because the packed original's uuid is NULL, so the
	// duplicate lands; the AFTER INSERT trigger that would have set its
	// uuid_m then hits the unique index and is swallowed by that same OR
	// IGNORE. What remains is a plain duplicate with a NULL twin — exactly
	// the live shape.
	seedDuplicateEntry(t, s, "sess-poison", "u-0")
	var rows int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM entries WHERE session_id = 'sess-poison'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != good+1 {
		t.Skipf("the duplicate did not land (%d rows): the dedup hole behind this test is closed, "+
			"so the collider it guards against can no longer be created", rows)
	}

	// The next pass meets the collider. Before 🎯T169 this returned UNIQUE
	// constraint failed, rows=0, and the family never advanced again.
	res, err := s.CompressBackfill(context.Background(), FamilyEntriesRaw)
	if err != nil {
		t.Fatalf("one colliding row aborted the whole family: %v", err)
	}
	if !res.Done {
		t.Error("the family must still reach done; VACUUM is gated on it")
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want exactly 1 (the collider)", res.Skipped)
	}

	// Everything but the collider is packed, and the collider is still
	// readable — skipping must not lose the row.
	// raw_z, not raw: the compressed-blob column is the only reliable
	// signal of whether a row is packed, and the ratchet in
	// compress_readers_test.go rightly refuses a bare read of raw.
	var plain, total int
	if err := s.readDB.QueryRow(`
		SELECT SUM(raw_z IS NULL), COUNT(*) FROM entries WHERE session_id = 'sess-poison'`).
		Scan(&plain, &total); err != nil {
		t.Fatal(err)
	}
	if total != good+1 {
		t.Fatalf("seeded %d rows, found %d", good+1, total)
	}
	if plain != 1 {
		t.Errorf("%d rows left plain, want 1 (only the collider)", plain)
	}
	var readable int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*) FROM entries_v
		WHERE session_id = 'sess-poison' AND json_extract(raw, '$.uuid') IS NOT NULL`).
		Scan(&readable); err != nil {
		t.Fatal(err)
	}
	if readable != good+1 {
		t.Errorf("%d of %d rows readable after the pass", readable, good+1)
	}

	// The reason has to survive the log (🎯T169): a family that finished
	// with a residue must say why in op=compress_status.
	var lastErr string
	if err := s.readDB.QueryRow(
		`SELECT last_error FROM compression_gc WHERE family = ?`, FamilyEntriesRaw).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastErr, "skipped") {
		t.Errorf("last_error = %q, want a durable note naming the skip", lastErr)
	}
}

func seedLegacyEntries(t *testing.T, s *Store, session string, n int) {
	t.Helper()
	tx, err := s.writeDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(entryInsertLegacySQL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		e := entryFixture(fmt.Sprintf("u-%d", i), "claude-opus-5", longText("poison", 3), "2026-04-01T10:00:00Z", i, 1)
		line, _ := json.Marshal(e)
		if _, err := stmt.Exec(session, "proj", "assistant", "2026-04-01T10:00:00Z", string(line)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// seedDuplicateEntry inserts a second copy of an existing uuid in the
// same session — what a re-ingest produced while uuid_m was NULL.
func seedDuplicateEntry(t *testing.T, s *Store, session, uuid string) {
	t.Helper()
	e := entryFixture(uuid, "claude-opus-5", longText("poison", 3), "2026-04-01T10:00:00Z", 0, 1)
	line, _ := json.Marshal(e)
	if _, err := s.writeDB.Exec(entryInsertLegacySQL,
		session, "proj", "assistant", "2026-04-01T10:00:00Z", string(line)); err != nil {
		t.Fatalf("the duplicate must be insertable while uuid_m is NULL — that is the bug: %v", err)
	}
}
