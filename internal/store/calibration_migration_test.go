// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Migration tests for the 🎯T144 search_calibration table.
//
// mnemo's schema is an append-only contract applied under
// sqlift.AllowNone — no rebuilds, no drops, no data-dependent changes,
// ever. A new table is the one shape that is unambiguously allowed, but
// "unambiguously allowed" is a claim about the diff, and the diff is
// generated from schema.sql rather than hand-written. These tests check
// the claim rather than assume it.

// schemaWithoutCalibration returns the shipped schema with the
// search_calibration table removed — i.e. the schema as it stood in
// v0.84.0, the last release before this feature.
func schemaWithoutCalibration(t *testing.T) string {
	t.Helper()
	full := schemaSQL
	start := strings.Index(full, "CREATE TABLE search_calibration")
	if start < 0 {
		t.Fatal("search_calibration not found in schema.sql — this test is stale")
	}
	end := strings.Index(full[start:], ");")
	if end < 0 {
		t.Fatal("could not find the end of the search_calibration definition")
	}
	return full[:start] + full[start+end+2:]
}

// TestCalibrationTableIsPurelyAdditive is the migration gate: upgrading
// a database built from the previous schema must succeed under
// AllowNone. If this fails, the change is not additive and cannot ship
// under mnemo's schema policy — the fix is to redesign the change, not
// to relax the gate.
func TestCalibrationTableIsPurelyAdditive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prev.db")

	// Seed a database at the pre-🎯T144 schema.
	applyDDL(t, path, schemaWithoutCalibration(t))

	// Confirm the seed really lacks the table, so a false pass is not
	// possible by seeding the new schema twice.
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='search_calibration'`).Scan(&n); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n != 0 {
		t.Fatal("seed database already has search_calibration; the test proves nothing")
	}
	db.Close()

	// Upgrade to the shipped schema under the production gate.
	if err := tryUpgrade(t, path, schemaSQL); err != nil {
		t.Fatalf("upgrading to the 🎯T144 schema failed under AllowNone: %v\n"+
			"A new table must be a pure addition. If this is failing, something "+
			"in the change is destructive or data-dependent.", err)
	}

	// And the table must actually exist afterwards.
	db, err = sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='search_calibration'`).Scan(&n); err != nil {
		t.Fatalf("post-upgrade probe: %v", err)
	}
	if n != 1 {
		t.Fatal("upgrade reported success but search_calibration does not exist")
	}
}

// TestMigrationPreservesExistingData is the criterion that matters more
// than the DDL succeeding: an upgrade must not lose a row. mnemo's
// schema policy exists because some users' source transcripts have been
// pruned upstream, so a wipe-and-reindex is permanent data loss.
func TestMigrationPreservesExistingData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	applyDDL(t, path, schemaWithoutCalibration(t))

	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := db.Exec(`INSERT INTO messages
			(session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			"s", "pre-existing message about watchers and descriptors"); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	db.Close()

	if err := tryUpgrade(t, path, schemaSQL); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	db, err = sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 50 {
		t.Fatalf("message count is %d after upgrade, want 50 — the migration lost data", count)
	}
	// FTS must still resolve against the preserved rows: an
	// external-content index whose content table was rebuilt would
	// return rowids that no longer resolve.
	var hits int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'watchers'`).Scan(&hits); err != nil {
		t.Fatalf("fts probe: %v", err)
	}
	if hits != 50 {
		t.Fatalf("FTS returned %d hits after upgrade, want 50 — the index and its "+
			"content table are out of step", hits)
	}
}

// TestUpgradeIsIdempotent: applying the shipped schema to a database
// already at that schema must be a no-op, not an error. The daemon runs
// the migration on every start.
func TestUpgradeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idem.db")
	applyDDL(t, path, schemaSQL)
	if err := tryUpgrade(t, path, schemaSQL); err != nil {
		t.Fatalf("re-applying the current schema must be a no-op, got: %v", err)
	}
	if err := tryUpgrade(t, path, schemaSQL); err != nil {
		t.Fatalf("second re-application failed: %v", err)
	}
}

// TestFreshInstallHasCalibrationTable covers the other install path: a
// brand-new database created from schema.sql directly, rather than
// upgraded into.
func TestFreshInstallHasCalibrationTable(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(
		`INSERT INTO search_calibration (corpus, quantiles, sample_size, doc_count, computed_at)
		 VALUES ('probe', '[1,2,3]', 3, 10, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("fresh install cannot write search_calibration: %v", err)
	}
	cals, err := s.LoadCalibrations()
	if err != nil {
		t.Fatalf("LoadCalibrations: %v", err)
	}
	if cals["probe"] == nil {
		t.Fatal("row written but not loaded back")
	}
}

// TestSearchWorksBeforeAnyCalibration is the migration-day behaviour: a
// user who upgrades has an empty search_calibration table until the
// reconciler first runs. Search must work in that window — scoring at
// the neutral prior, not broken, and not silently comparing raw BM25.
func TestSearchWorksBeforeAnyCalibration(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCorpora(t, s)

	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM search_calibration`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected an empty calibration table, got %d rows", n)
	}

	res, err := s.UnifiedSearch("watcher", nil, 10, time.Now())
	if err != nil {
		t.Fatalf("search must work on a freshly-migrated database: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits on a freshly-migrated database")
	}
	for _, h := range res.Hits {
		if h.Ranking != "neutral" {
			t.Errorf("hit ranked %q before any calibration exists; want neutral", h.Ranking)
		}
	}
}

// TestSchemaFileHasCalibration guards against the embedded schema and
// the migration tests drifting apart.
func TestSchemaFileHasCalibration(t *testing.T) {
	if !strings.Contains(schemaSQL, "CREATE TABLE search_calibration") {
		t.Fatal("schema.sql does not declare search_calibration")
	}
	if _, err := os.Stat("schema.sql"); err != nil {
		t.Skipf("schema.sql not readable from the test working dir: %v", err)
	}
	onDisk, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	if !strings.Contains(string(onDisk), "CREATE TABLE search_calibration") {
		t.Fatal("the on-disk schema.sql and the embedded copy disagree")
	}
}
