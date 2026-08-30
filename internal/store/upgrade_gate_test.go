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

// stageDeferredUpgrade builds a database whose schema is one additive
// table behind, so the next New sees a non-empty AllowNone plan and
// defers it (🎯T114.1). Returns the db path and a channel that releases
// the stubbed pre-migration backup, plus a channel closed once that
// backup has started.
func stageDeferredUpgrade(t *testing.T) (dbPath string, started, release chan struct{}) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "mnemo.db")
	projectDir := t.TempDir()

	s0, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatalf("seed New: %v", err)
	}
	if err := s0.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
	db, err := sql.Open(writerDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE decision_scan_state`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	started, release = make(chan struct{}), make(chan struct{})
	prev := preMigrationBackup
	preMigrationBackup = func(src, dest string, args *backup.BackupArgs) (backup.Result, error) {
		close(started)
		<-release
		return backup.Result{Path: dest, RawSize: 1, CompressedSize: 1, Elapsed: time.Millisecond}, nil
	}
	t.Cleanup(func() { preMigrationBackup = prev })
	return dbPath, started, release
}

// TestAwaitSchemaUpgradeGatesOnTheMigration is the regression guard for
// a bug that landed twice in one session (🎯T123).
//
// 🎯T114.1 defers the schema upgrade so the daemon can serve during a
// pre-migration backup that takes minutes on a large index. While it
// runs, the store answers on the OLD schema. Any background worker that
// touches newly-added schema must therefore wait on AwaitSchemaUpgrade
// first — the segment backfill did not, and read
// topic_segments.compaction_id before it existed; the image embedder
// did not, and wrote image_embedding_attempts before it existed.
//
// This never reproduces on a fresh test database, because a fresh
// database is already at the latest schema. It only appears on an
// UPGRADE boot, which is exactly why CI could not see it.
func TestAwaitSchemaUpgradeGatesOnTheMigration(t *testing.T) {
	dbPath, started, release := stageDeferredUpgrade(t)
	projectDir := t.TempDir()

	s, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close() }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("pre-migration backup never started; upgrade was not deferred")
	}

	// Precondition: the upgrade really is outstanding, so this test is
	// exercising the gate rather than a schema that was never behind.
	if tableExists(t, s, "decision_scan_state") {
		close(release)
		t.Fatal("schema already upgraded; the deferred window was not staged")
	}

	// A worker that waits on the gate must not observe the old schema.
	gated := make(chan bool, 1)
	go func() {
		s.AwaitSchemaUpgrade()
		gated <- tableExists(t, s, "decision_scan_state")
	}()

	// While the migration is blocked the gate must hold the worker back.
	select {
	case <-gated:
		close(release)
		t.Fatal("AwaitSchemaUpgrade returned while the migration was still blocked")
	case <-time.After(250 * time.Millisecond):
	}

	close(release)
	select {
	case sawTable := <-gated:
		if !sawTable {
			t.Error("worker released by AwaitSchemaUpgrade still saw the pre-migration schema")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitSchemaUpgrade never returned after the migration completed")
	}
}

// tableExists reports whether a table is present in the live schema.
func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, name,
	).Scan(&n); err != nil {
		return false
	}
	return n == 1
}
