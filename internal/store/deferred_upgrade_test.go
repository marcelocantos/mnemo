// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
	_ "github.com/mattn/go-sqlite3"
)

// TestNewReturnsWhilePreMigrationBackupBlocked proves 🎯T114.1: store.New
// opens long-lived handles and returns without waiting for the pre-migration
// VACUUM+gzip that used to serialize the entire daemon open path.
func TestNewReturnsWhilePreMigrationBackupBlocked(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mnemo.db")
	projectDir := t.TempDir()

	// Full schema first, then drop one additive-only table so the next
	// New sees a non-empty AllowNone plan (CREATE TABLE decision_scan_state).
	s0, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatalf("seed New: %v", err)
	}
	if err := s0.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE decision_scan_state`); err != nil {
		t.Fatal(err)
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
		// Real tiny backup so apply still has a clean path; dest need not
		// exist for apply to proceed (backup failure is non-fatal), but a
		// successful Result keeps the log path realistic.
		return backup.Result{Path: dest, RawSize: 1, GzippedSize: 1, Elapsed: time.Millisecond}, nil
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

	// Store must answer queries before backup completes.
	if _, err := s.Query("SELECT 1 AS ok"); err != nil {
		close(block)
		_ = s.Close()
		t.Fatalf("query during backup: %v", err)
	}

	close(block)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close (waits for upgradeDone), the dropped table is restored.
	db2, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var n int
	if err := db2.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='decision_scan_state'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("decision_scan_state missing after deferred upgrade (count=%d)", n)
	}
}

// TestApplySchemaStillUpgradesSynchronously keeps the sync path used by
// tests/tools: prepare + backup + apply before return.
func TestApplySchemaStillUpgradesSynchronously(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mnemo.db")
	projectDir := t.TempDir()

	s, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE decision_scan_state`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := applySchema(dbPath); err != nil {
		t.Fatalf("applySchema: %v", err)
	}

	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='decision_scan_state'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("decision_scan_state missing after applySchema (count=%d)", n)
	}
}
