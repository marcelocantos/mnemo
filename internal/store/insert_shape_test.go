// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
)

// TestModernInsertShapeProbeRejects096Schema is the discriminating
// check for the deferred-upgrade ingest hole: 0.96.0 already has
// uuid_m and text_z, so a T170-only probe would select the modern
// INSERT (which binds message_id_m / messages.plain_len) and fail
// with "no such column" for the whole pre-migration backup window.
func TestModernInsertShapeProbeRejects096Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE entries (id INTEGER PRIMARY KEY, uuid_m TEXT);
		CREATE TABLE messages (id INTEGER PRIMARY KEY, text_z BLOB);
	`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(modernInsertShapeSQL).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("0.96-shaped schema scored %d; modern INSERT would fail during 🎯T114.1", n)
	}
}

func TestModernInsertShapeAcceptsCurrentSchema(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if !s.modernInsertShape() {
		t.Fatal("current schema must select the modern INSERT")
	}
}

// TestIngestDuring096UpgradeUsesLegacyInsert is the live form: strip
// the columns this PR added, open the store while the deferred backup
// is blocked (serving on the 0.96-shaped schema), and ingest. The
// writer must use the legacy statements; the modern ones name columns
// that are not there yet.
//
// DROP COLUMN is only a test fixture. The production 0.96.0 plan is
// append-only ADD COLUMN (TestSchemaUpgradeIsAdditive); sqlift will
// refuse to re-add a dropped column under AllowNone, which is why
// the background upgrade in this test fails. Ingest is the assertion.
func TestIngestDuring096UpgradeUsesLegacyInsert(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mnemo.db")
	projectDir := t.TempDir()

	s0, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatalf("seed New: %v", err)
	}
	if err := s0.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open(writerDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP VIEW IF EXISTS entries_v`,
		`DROP TRIGGER IF EXISTS entries_materialise`,
		`ALTER TABLE entries DROP COLUMN message_id_m`,
		`ALTER TABLE messages DROP COLUMN plain_len`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	block := make(chan struct{})
	prev := preMigrationBackup
	preMigrationBackup = func(src, dest string, args *backup.BackupArgs) (backup.Result, error) {
		if args != nil && args.OnStep != nil {
			args.OnStep("test: blocked pre-migration backup")
		}
		close(started)
		<-block
		return backup.Result{Path: dest, RawSize: 1, CompressedSize: 1, Elapsed: time.Millisecond}, nil
	}
	t.Cleanup(func() { preMigrationBackup = prev })

	type result struct {
		s   *Store
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := New(dbPath, projectDir)
		done <- result{s: s, err: err}
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("pre-migration backup never started")
	}

	var res result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		close(block)
		t.Fatal("New still blocked while pre-migration backup is in progress")
	}
	if res.err != nil {
		close(block)
		t.Fatalf("New: %v", res.err)
	}
	s := res.s
	defer func() {
		close(block)
		_ = s.Close()
	}()

	if s.modernInsertShape() {
		t.Fatal("stripped 0.96-shaped schema must not select the modern INSERT")
	}

	line := assistantWithUsage(now(), "claude-sonnet-4-6", 10, 4, 0, 0)
	line["uuid"] = "33333333-0000-4000-8000-000000000001"
	path := writeJSONL(t, projectDir, "p", "sess-096-upgrade", []map[string]any{line})
	if err := s.ingestFile(path); err != nil {
		t.Fatalf("ingest during deferred 0.96 upgrade: %v", err)
	}
	var n int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM entries WHERE session_id = 'sess-096-upgrade'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("ingest wrote nothing during the deferred-upgrade window")
	}
}
