// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/marcelocantos/sqlift/go/sqlift"
)

// These tests pin down the four-gate model that mnemo's startup migration
// relies on (🎯T49 acceptance criteria 8–11): AllowNone must reject any
// op type that isn't a pure addition, and modifying a trigger body must
// remain non-destructive so future trigger updates can ship as minor
// releases.

// applyDDL runs a DDL string through sqlift.Apply against a fresh DB at
// path. Used to seed a "starting state" before diffing against a desired
// schema.
func applyDDL(t *testing.T, path, ddl string) {
	t.Helper()
	sdb, err := sqlift.Open(path)
	if err != nil {
		t.Fatalf("sqlift.Open: %v", err)
	}
	defer sdb.Close()
	desired, err := sqlift.Parse(ddl)
	if err != nil {
		t.Fatalf("sqlift.Parse seed: %v", err)
	}
	current, err := sqlift.Extract(sdb)
	if err != nil {
		t.Fatalf("sqlift.Extract: %v", err)
	}
	plan, err := sqlift.Diff(current, desired)
	if err != nil {
		t.Fatalf("sqlift.Diff seed: %v", err)
	}
	if err := sqlift.Apply(sdb, plan, sqlift.ApplyOptions{Allow: sqlift.AllowNone}); err != nil {
		t.Fatalf("sqlift.Apply seed: %v", err)
	}
}

// tryUpgrade extracts current schema, parses desired, diffs and attempts
// Apply under AllowNone. Returns any error.
func tryUpgrade(t *testing.T, path, desiredDDL string) error {
	t.Helper()
	sdb, err := sqlift.Open(path)
	if err != nil {
		t.Fatalf("sqlift.Open: %v", err)
	}
	defer sdb.Close()
	current, err := sqlift.Extract(sdb)
	if err != nil {
		t.Fatalf("sqlift.Extract: %v", err)
	}
	desired, err := sqlift.Parse(desiredDDL)
	if err != nil {
		t.Fatalf("sqlift.Parse desired: %v", err)
	}
	plan, err := sqlift.Diff(current, desired)
	if err != nil {
		return err
	}
	return sqlift.Apply(sdb, plan, sqlift.ApplyOptions{Allow: sqlift.AllowNone})
}

// freshDB returns a path to a new empty SQLite DB in t.TempDir().
func freshDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

// rowCount returns the count of rows in tbl, or -1 if the table is missing.
func rowCount(t *testing.T, path, tbl string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + tbl).Scan(&n); err != nil {
		return -1
	}
	return n
}

// TestUpgradeMirrorStatusBackoffColumns pins the 🎯T91 additive
// migration: the failure-backoff columns (fail_count, last_attempt_at)
// are added to an existing mirror_status under AllowNone, preserving rows
// and defaulting the new columns. This is what lets the deployed daemon
// migrate the production DB with no flag and no rebuild.
func TestUpgradeMirrorStatusBackoffColumns(t *testing.T) {
	path := freshDB(t)
	// Pre-🎯T91 shape.
	applyDDL(t, path, `
		CREATE TABLE mirror_status (
			repo TEXT NOT NULL,
			stream TEXT NOT NULL,
			last_reconciled_at TEXT NOT NULL,
			PRIMARY KEY (repo, stream)
		);
	`)
	{
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO mirror_status(repo, stream, last_reconciled_at)
			 VALUES('o/r','ci','2026-01-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	// Upgrade to the post-🎯T91 shape under AllowNone — must succeed.
	if err := tryUpgrade(t, path, `
		CREATE TABLE mirror_status (
			repo TEXT NOT NULL,
			stream TEXT NOT NULL,
			last_reconciled_at TEXT NOT NULL,
			fail_count INTEGER NOT NULL DEFAULT 0,
			last_attempt_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (repo, stream)
		);
	`); err != nil {
		t.Fatalf("AllowNone upgrade of mirror_status must succeed: %v", err)
	}
	// Existing row preserved; new columns take their defaults.
	{
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var fc int
		var la string
		if err := db.QueryRow(
			`SELECT fail_count, last_attempt_at FROM mirror_status
			 WHERE repo='o/r' AND stream='ci'`).Scan(&fc, &la); err != nil {
			t.Fatalf("read migrated row: %v", err)
		}
		if fc != 0 || la != "" {
			t.Errorf("expected defaults (0, \"\"), got (%d, %q)", fc, la)
		}
	}
}

// TestUpgradeAddsDecisionScanState pins the 🎯T92 additive migration: the
// decision_scan_state table is created on an existing DB under AllowNone
// (it is a brand-new table, the simplest additive change), so the deployed
// daemon migrates the production DB with no flag.
func TestUpgradeAddsDecisionScanState(t *testing.T) {
	path := freshDB(t)
	// A pre-🎯T92 schema without the watermark table.
	applyDDL(t, path, `
		CREATE TABLE decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			proposal_text TEXT NOT NULL,
			confirmation_text TEXT NOT NULL,
			timestamp TEXT NOT NULL
		);
	`)
	if rowCount(t, path, "decision_scan_state") != -1 {
		t.Fatalf("expected decision_scan_state absent before upgrade")
	}
	if err := tryUpgrade(t, path, `
		CREATE TABLE decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			proposal_text TEXT NOT NULL,
			confirmation_text TEXT NOT NULL,
			timestamp TEXT NOT NULL
		);
		CREATE TABLE decision_scan_state (
			session_id TEXT PRIMARY KEY,
			scanned_through_id INTEGER NOT NULL DEFAULT 0,
			scanned_at TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		t.Fatalf("AllowNone upgrade adding decision_scan_state must succeed: %v", err)
	}
	if rowCount(t, path, "decision_scan_state") != 0 {
		t.Fatalf("expected empty decision_scan_state after upgrade")
	}
}

// TestUpgradeAddsUsageIndex pins the 🎯T93 additive migration: the usage
// covering index is added to an existing entries table under AllowNone
// (a new index, the simplest additive change).
func TestUpgradeAddsUsageIndex(t *testing.T) {
	path := freshDB(t)
	applyDDL(t, path, `
		CREATE TABLE entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			type TEXT NOT NULL,
			timestamp TEXT,
			raw BLOB,
			model TEXT GENERATED ALWAYS AS (raw->>'$.message.model'),
			input_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.input_tokens')),
			output_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.output_tokens')),
			cache_read_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_read_input_tokens')),
			cache_creation_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_creation_input_tokens'))
		);
	`)
	if err := tryUpgrade(t, path, `
		CREATE TABLE entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			type TEXT NOT NULL,
			timestamp TEXT,
			raw BLOB,
			model TEXT GENERATED ALWAYS AS (raw->>'$.message.model'),
			input_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.input_tokens')),
			output_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.output_tokens')),
			cache_read_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_read_input_tokens')),
			cache_creation_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_creation_input_tokens'))
		);
		CREATE INDEX idx_entries_assistant_usage
			ON entries(timestamp, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, session_id)
			WHERE type = 'assistant';
	`); err != nil {
		t.Fatalf("AllowNone upgrade adding the usage index must succeed: %v", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_entries_assistant_usage'`).Scan(&name); err != nil {
		t.Fatalf("expected idx_entries_assistant_usage after upgrade: %v", err)
	}
}

func TestUpgradePreservesData(t *testing.T) {
	// Criterion 7: an additive upgrade (new column + new table + new
	// index + new trigger) preserves rows in pre-existing tables.
	path := freshDB(t)
	applyDDL(t, path, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		CREATE INDEX idx_users_name ON users(name);
	`)
	// Seed data.
	{
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"alice", "bob", "carol"} {
			if _, err := db.Exec("INSERT INTO users(name) VALUES(?)", n); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()
	}
	if got := rowCount(t, path, "users"); got != 3 {
		t.Fatalf("seed: got %d users, want 3", got)
	}

	// Apply an additive upgrade: new nullable column, a brand new table,
	// a new index, and a new trigger.
	err := tryUpgrade(t, path, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT
		);
		CREATE INDEX idx_users_name ON users(name);
		CREATE INDEX idx_users_email ON users(email) WHERE email IS NOT NULL;
		CREATE TABLE posts (
			id INTEGER PRIMARY KEY,
			user_id INTEGER REFERENCES users(id),
			body TEXT NOT NULL
		);
		CREATE TRIGGER posts_log AFTER INSERT ON posts BEGIN
			SELECT 1;
		END;
	`)
	if err != nil {
		t.Fatalf("AllowNone upgrade failed: %v", err)
	}
	if got := rowCount(t, path, "users"); got != 3 {
		t.Errorf("after upgrade: got %d users, want 3 (data lost)", got)
	}
	if got := rowCount(t, path, "posts"); got != 0 {
		t.Errorf("after upgrade: got %d posts, want 0 (new empty table)", got)
	}
}

func TestAllowNoneRejectsDestructiveDrop(t *testing.T) {
	// Criterion 8: dropping a table is destructive; AllowNone must reject.
	path := freshDB(t)
	applyDDL(t, path, `
		CREATE TABLE keep_me (id INTEGER PRIMARY KEY);
		CREATE TABLE drop_me (id INTEGER PRIMARY KEY);
	`)
	err := tryUpgrade(t, path, `
		CREATE TABLE keep_me (id INTEGER PRIMARY KEY);
	`)
	if err == nil {
		t.Fatal("expected DestructiveError, got nil")
	}
	var de *sqlift.DestructiveError
	if !errors.As(err, &de) {
		t.Errorf("error type = %T, want *sqlift.DestructiveError; err=%v", err, err)
	}
	// drop_me must still exist after the rejection.
	if got := rowCount(t, path, "drop_me"); got < 0 {
		t.Error("drop_me table was dropped despite gate rejection")
	}
}

func TestAllowNoneRejectsRebuild(t *testing.T) {
	// Criterion 9: changing a column type forces a 12-step rebuild;
	// AllowNone must reject with *sqlift.RebuildError.
	path := freshDB(t)
	applyDDL(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT);
	`)
	err := tryUpgrade(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val INTEGER);
	`)
	if err == nil {
		t.Fatal("expected RebuildError, got nil")
	}
	var re *sqlift.RebuildError
	if !errors.As(err, &re) {
		t.Errorf("error type = %T, want *sqlift.RebuildError; err=%v", err, err)
	}
}

func TestAllowNoneRejectsPureLoosening(t *testing.T) {
	// Criterion 10: NOT NULL → nullable on its own is a pure-loosening
	// rebuild; AllowNone must reject (only AllowLoosen permits it).
	path := freshDB(t)
	applyDDL(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT NOT NULL);
	`)
	err := tryUpgrade(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT);
	`)
	if err == nil {
		t.Fatal("expected RebuildError for pure-loosening, got nil")
	}
	// Pure-loosening is gated by AllowLoosen; AllowNone surfaces it as
	// a RebuildError (the rebuild flag is the broader gate that AllowLoosen
	// is a narrower carve-out of).
	var re *sqlift.RebuildError
	if !errors.As(err, &re) {
		t.Errorf("error type = %T, want *sqlift.RebuildError; err=%v", err, err)
	}
}

func TestAllowNoneRejectsDataDependent(t *testing.T) {
	// Criterion 11: nullable → NOT NULL is data-dependent (succeeds on
	// an empty DB, fails on one with NULL rows); AllowNone must reject
	// with *sqlift.BreakingChangeError at apply time (per sqlift v0.14).
	path := freshDB(t)
	applyDDL(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT);
	`)
	err := tryUpgrade(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT NOT NULL);
	`)
	if err == nil {
		t.Fatal("expected BreakingChangeError, got nil")
	}
	// Data-dependent is layered on top of rebuild — both gates fire.
	// sqlift's apply order checks rebuild first, so the reported error
	// is *RebuildError when both gates are off. With only AllowRebuild
	// set, the second-tier gate fires as *BreakingChangeError. We test
	// the AllowNone path here: the first failing gate is acceptable.
	var (
		re  *sqlift.RebuildError
		bce *sqlift.BreakingChangeError
	)
	if !errors.As(err, &re) && !errors.As(err, &bce) {
		t.Errorf("error type = %T, want *sqlift.RebuildError or *sqlift.BreakingChangeError; err=%v", err, err)
	}
}

func TestAllowNonePermitsTriggerBodyModification(t *testing.T) {
	// Criterion 12: replacing a trigger's body while keeping its name
	// is classified as non-destructive (sqlift.cpp Phase 1 logic — when
	// the same-named trigger appears in desired, the implicit DROP is
	// flagged drop_destructive=false). Mnemo relies on this so trigger
	// updates can ship under the strict policy.
	path := freshDB(t)
	applyDDL(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT);
		CREATE TRIGGER t_ai AFTER INSERT ON t BEGIN
			SELECT 1;
		END;
	`)
	// Modify the trigger body — same name, different SELECT.
	err := tryUpgrade(t, path, `
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT);
		CREATE TRIGGER t_ai AFTER INSERT ON t BEGIN
			SELECT 42;
		END;
	`)
	if err != nil {
		t.Fatalf("trigger body modification rejected under AllowNone: %v", err)
	}
}

func TestApplySchemaOnFreshDBSucceeds(t *testing.T) {
	// End-to-end: applying mnemo's embedded schema.sql to a fresh DB
	// under AllowNone produces a working store.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mnemo.db")
	projectDir := t.TempDir()

	s, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Round-trip: subsequent applies are no-ops.
	if err := applySchema(dbPath); err != nil {
		t.Errorf("applySchema on already-migrated DB: %v", err)
	}
}
