// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestJournalSizeLimitIsSet pins the pragma that makes a checkpoint
// actually shrink the file. Without it SQLite reuses the -wal from
// offset zero but never truncates, so the file is parked at its
// high-water mark for the life of the daemon.
func TestJournalSizeLimitIsSet(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	var limit int64
	if err := s.writeDB.QueryRow("PRAGMA journal_size_limit").Scan(&limit); err != nil {
		t.Fatalf("read journal_size_limit: %v", err)
	}
	if limit != walSizeLimitBytes {
		t.Errorf("journal_size_limit = %d, want %d", limit, walSizeLimitBytes)
	}
}

// Checkpoint's own truncation behaviour is already covered by
// TestCheckpointTruncatesWAL (🎯T97.1) in store_test.go; the tests here
// cover the maintenance policy layered on top of it.

// TestMaybeCheckpointSkipsSmallWAL: below the threshold the file is
// doing its job as a write buffer, and truncating would only contend
// with writers for no gain.
func TestMaybeCheckpointSkipsSmallWAL(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(
		`INSERT INTO messages (session_id, project, role, text, timestamp, type)
		 VALUES ('s', 'p', 'user', 'x', '2026-07-27T00:00:00Z', 'text')`); err != nil {
		t.Fatal(err)
	}

	// A small WAL must be left alone — no error, no truncation attempt.
	before, _ := s.walSize()
	s.maybeCheckpointWAL()
	after, _ := s.walSize()
	if before != after {
		t.Errorf("small WAL should be untouched: %d -> %d", before, after)
	}
}

// TestMaybeCheckpointDefersWhileWritesAreActive: a checkpoint during a
// write burst just contends. The worker waits for a lull.
func TestMaybeCheckpointDefersWhileWritesAreActive(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Pretend the WAL is over threshold by pointing walSize at a big
	// file, and mark writes as happening right now.
	big := s.dbPath + "-wal"
	if err := os.WriteFile(big, make([]byte, walCheckpointThreshold+1), 0o644); err != nil {
		t.Skipf("cannot stage a large -wal: %v", err)
	}
	s.NoteActivity()

	// Should return without attempting anything; the assertion is that
	// it does not block or error, and leaves the staged file in place.
	s.maybeCheckpointWAL()
	if fi, err := os.Stat(big); err == nil && fi.Size() == 0 {
		t.Error("checkpoint ran despite active writes")
	}
}

// TestNoteWALSizeReportsGrowth pins the distinction the diagnostic
// rests on: a large-but-stable WAL is normal (SQLite reuses the file),
// while sustained growth means checkpoints cannot advance.
func TestNoteWALSizeReportsGrowth(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	if s.NoteWALSize(100 << 20) {
		t.Error("first observation has nothing to compare against; must not report growth")
	}
	if !s.NoteWALSize(200 << 20) {
		t.Error("a larger WAL than last time is growth")
	}
	if s.NoteWALSize(200 << 20) {
		t.Error("an unchanged WAL is stable, not growing")
	}
	if s.NoteWALSize(150 << 20) {
		t.Error("a shrinking WAL is not growth")
	}
}

// TestWALSizeMissingFile: no -wal means nothing to reclaim, not an error.
//
// walSize reads only s.dbPath, so the absent-file case is built by
// pointing a bare Store at a path that was never opened. Deleting the
// -wal out from under a live store does not work: POSIX unlinks an open
// file happily, but Windows refuses, so the removal failed silently
// there and the assertion saw a real WAL (8272 bytes on hms-vm, once
// boot-time codec writes guaranteed one existed).
func TestWALSizeMissingFile(t *testing.T) {
	s := &Store{dbPath: filepath.Join(t.TempDir(), "never-opened.db")}
	if _, err := os.Stat(s.dbPath + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("precondition: -wal should not exist, stat err = %v", err)
	}
	n, err := s.walSize()
	if err != nil {
		t.Errorf("missing -wal should not error: %v", err)
	}
	if n != 0 {
		t.Errorf("missing -wal size = %d, want 0", n)
	}
}
