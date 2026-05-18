// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"compress/gzip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// seedDB creates a small SQLite DB with N rows. Returns its path.
func seedDB(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "src.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT);
		CREATE INDEX idx_t_val ON t(val);
	`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Exec("INSERT INTO t(val) VALUES(?)", "row-"+strings.Repeat("x", i%20)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// countRows opens a possibly-gzipped backup and returns the row count of
// table t. For .gz it ungzips into a temp file first.
func countRows(t *testing.T, path string) int {
	t.Helper()
	dbPath := path
	if strings.HasSuffix(path, ".gz") {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		out, err := os.CreateTemp("", "verify-*.db")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(out.Name())
		if _, err := io.Copy(out, gz); err != nil {
			t.Fatal(err)
		}
		out.Close()
		dbPath = out.Name()
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBackupRoundTrip(t *testing.T) {
	const rows = 250
	src := seedDB(t, rows)
	dir := t.TempDir()
	dest := filepath.Join(dir, Filename(TagDaily, time.Now()))

	res, err := Backup(src, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if res.Path != dest {
		t.Errorf("Result.Path = %q, want %q", res.Path, dest)
	}
	if res.RawSize == 0 || res.GzippedSize == 0 {
		t.Errorf("Result.RawSize=%d Result.GzippedSize=%d, both should be >0", res.RawSize, res.GzippedSize)
	}
	if res.Elapsed <= 0 {
		t.Errorf("Result.Elapsed = %v, want >0", res.Elapsed)
	}

	if got := countRows(t, dest); got != rows {
		t.Errorf("restored row count = %d, want %d", got, rows)
	}
}

func TestBackupRejectsNonGzDestination(t *testing.T) {
	src := seedDB(t, 1)
	dir := t.TempDir()
	dest := filepath.Join(dir, "snapshot.db") // missing .gz

	_, err := Backup(src, dest)
	if err == nil {
		t.Fatal("expected error for non-.gz destination, got nil")
	}
	if !strings.Contains(err.Error(), ".gz") {
		t.Errorf("error doesn't mention .gz: %v", err)
	}
}

func TestBackupCleansUpTempOnFailure(t *testing.T) {
	src := seedDB(t, 1)
	dir := t.TempDir()
	// Destination directory must exist; we point at a path inside a
	// non-existent subdir to force gzipFile to fail when creating the
	// .tmp file.
	dest := filepath.Join(dir, "nope", Filename(TagDaily, time.Now()))

	_, err := Backup(src, dest)
	if err == nil {
		t.Fatal("expected error when destination dir doesn't exist")
	}

	// dir should be exactly as we left it (no .backup-*.db, no .tmp files
	// leaking up to the parent).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestBackupSurvivesPathWithQuote(t *testing.T) {
	src := seedDB(t, 5)
	// Path with a single quote in the directory name. SQLite's VACUUM
	// INTO uses single-quoted paths; backup.go escapes embedded quotes.
	dir := filepath.Join(t.TempDir(), "with'quote")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, Filename(TagDaily, time.Now()))

	if _, err := Backup(src, dest); err != nil {
		t.Fatalf("Backup with quoted path: %v", err)
	}
	if got := countRows(t, dest); got != 5 {
		t.Errorf("row count = %d, want 5", got)
	}
}

func TestFilenameTagAndTimeFormat(t *testing.T) {
	ts := time.Date(2026, 5, 18, 3, 17, 42, 0, time.UTC)
	got := Filename(TagPreMigration, ts)
	want := "mnemo-pre-migration-20260518T031742Z.db.gz"
	if got != want {
		t.Errorf("Filename = %q, want %q", got, want)
	}
}

func TestFilenameSortableChronologically(t *testing.T) {
	older := Filename(TagDaily, time.Date(2026, 5, 18, 3, 0, 0, 0, time.UTC))
	newer := Filename(TagDaily, time.Date(2026, 5, 18, 4, 0, 0, 0, time.UTC))
	if older >= newer {
		t.Errorf("filenames don't sort chronologically: %s !< %s", older, newer)
	}
}
