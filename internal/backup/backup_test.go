// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"compress/gzip"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/marcelocantos/mnemo/internal/zstdc"
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

// countRows opens a backup and returns the row count of table t. A
// compressed backup is expanded through the production Decompress helper,
// so these tests exercise the same reader a restore would.
func countRows(t *testing.T, path string) int {
	t.Helper()
	dbPath := path
	if strings.HasSuffix(path, ExtZstd) || strings.HasSuffix(path, ExtGzip) {
		dbPath = filepath.Join(t.TempDir(), "verify.db")
		if _, err := Decompress(path, dbPath); err != nil {
			t.Fatalf("Decompress(%s): %v", filepath.Base(path), err)
		}
	}
	db, err := sql.Open("sqlite3", dbPath)
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
	if res.RawSize == 0 || res.CompressedSize == 0 {
		t.Errorf("Result.RawSize=%d Result.CompressedSize=%d, both should be >0", res.RawSize, res.CompressedSize)
	}
	if res.Elapsed <= 0 {
		t.Errorf("Result.Elapsed = %v, want >0", res.Elapsed)
	}

	if got := countRows(t, dest); got != rows {
		t.Errorf("restored row count = %d, want %d", got, rows)
	}
}

func TestBackupWithOnStep(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	dest := filepath.Join(dir, Filename(TagPreMigration, time.Now()))
	var steps []string
	res, err := BackupWith(src, dest, &BackupArgs{
		OnStep: func(step string) { steps = append(steps, step) },
	})
	if err != nil {
		t.Fatalf("BackupWith: %v", err)
	}
	if res.CompressedSize == 0 {
		t.Fatal("empty compression result")
	}
	if len(steps) < 3 {
		t.Fatalf("want at least vacuum/integrity/compress steps, got %v", steps)
	}
	joined := strings.Join(steps, " | ")
	if !strings.Contains(joined, "VACUUM") {
		t.Errorf("missing VACUUM step: %v", steps)
	}
	if !strings.Contains(joined, "compressing") {
		t.Errorf("missing compression step: %v", steps)
	}
	// Verification is a step in its own right, and must be reported: it is
	// what makes deleting the previous snapshot safe at keep=1.
	if !strings.Contains(joined, "verifying") {
		t.Errorf("missing verification step: %v", steps)
	}
}

func TestBackupRejectsWrongDestinationExtension(t *testing.T) {
	src := seedDB(t, 1)
	dir := t.TempDir()
	for _, dest := range []string{"snapshot.db", "snapshot.db.gz"} {
		_, err := Backup(src, filepath.Join(dir, dest))
		if err == nil {
			t.Fatalf("%s: expected an error, got nil", dest)
		}
		if !strings.Contains(err.Error(), ExtZstd) {
			t.Errorf("%s: error doesn't mention %s: %v", dest, ExtZstd, err)
		}
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
	want := "mnemo-pre-migration-20260518T031742Z.db.zst"
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

// TestVerificationCatchesCorruption is the reason verification exists.
// Retention keeps ONE snapshot (🎯T158), so the caller deletes the
// previous backup once this one is declared good. A backup that cannot be
// read back is therefore not a degraded backup, it is no backup — and the
// only moment to find out is before the old one goes.
func TestVerificationCatchesCorruption(t *testing.T) {
	dir := t.TempDir()
	src := seedDB(t, 200)
	good := filepath.Join(dir, "good"+ExtZstd)
	if _, err := Backup(src, good); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	b, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}

	// Flip bytes in the compressed payload, past the frame header. The
	// XXH64 checksum written into the frame turns this into a hard read
	// error rather than silently wrong bytes.
	corrupt := append([]byte(nil), b...)
	for i := len(corrupt) / 2; i < len(corrupt)/2+64 && i < len(corrupt); i++ {
		corrupt[i] ^= 0xff
	}
	bad := filepath.Join(dir, "bad"+ExtZstd)
	if err := os.WriteFile(bad, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyCompressed(bad, int64(len(b))); err == nil {
		t.Error("verification accepted a corrupted artefact")
	}

	// Truncation is the other realistic failure — a full disk, an
	// interrupted write. The frame never terminates, so the read fails.
	short := filepath.Join(dir, "short"+ExtZstd)
	if err := os.WriteFile(short, b[:len(b)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyCompressed(short, int64(len(b))); err == nil {
		t.Error("verification accepted a truncated artefact")
	}
}

// TestVerificationChecksSize guards the case a checksum cannot: an
// artefact that decompresses cleanly but to the wrong content length.
func TestVerificationChecksSize(t *testing.T) {
	dir := t.TempDir()
	src := seedDB(t, 50)
	dest := filepath.Join(dir, "s"+ExtZstd)
	res, err := Backup(src, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := verifyCompressed(dest, res.RawSize+1); err == nil {
		t.Error("verification accepted a size mismatch")
	}
	if err := verifyCompressed(dest, res.RawSize); err != nil {
		t.Errorf("verification rejected a sound artefact: %v", err)
	}
}

// TestDecompressReadsLegacyGzip keeps snapshots taken before 🎯T159
// restorable. The format switched; the backups already on disk did not,
// and a restore path that cannot read them makes them worthless.
func TestDecompressReadsLegacyGzip(t *testing.T) {
	dir := t.TempDir()
	srcPath := seedDB(t, 30)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	gzPath := filepath.Join(dir, "legacy"+ExtGzip)
	f, err := os.Create(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, gzPath); got != 30 {
		t.Errorf("legacy gzip backup restored %d rows, want 30", got)
	}
}

func TestDecompressRejectsUnknownExtension(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Decompress(src, filepath.Join(dir, "out")); err == nil {
		t.Error("Decompress accepted an unrecognised extension")
	}
}

// TestCompressionUsesAllWorkers guards the silent-single-thread failure
// mode: libzstd built without ZSTD_MULTITHREAD accepts a worker count and
// then ignores it, so the backup still succeeds and just takes the ~3.8
// minutes the switch was meant to remove. Only the granted count reveals
// it.
func TestCompressionUsesAllWorkers(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("single-CPU machine")
	}
	dir := t.TempDir()
	src := seedDB(t, 5000)
	st, err := zstdc.CompressFile(src, filepath.Join(dir, "o"+ExtZstd), zstdc.LevelFast, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.WorkersUsed < 2 {
		t.Errorf("libzstd granted %d workers on a %d-CPU machine — the "+
			"vendored amalgamation looks single-threaded, so backups are "+
			"paying gzip-era wall-clock", st.WorkersUsed, runtime.NumCPU())
	}
}

// TestInterruptedBackupLeavesNothingBehind is acceptance criterion 1 of
// 🎯T158. A backup that fails between VACUUM INTO and a finished
// artefact must leave the directory exactly as it found it — no
// multi-gigabyte VACUUM temp, no half-written .tmp, no partial output
// under the final name. The 104 GB of orphans that motivated the target
// came from process death rather than a returned error, but an error
// path that leaks is the same bug with a smaller blast radius.
func TestInterruptedBackupLeavesNothingBehind(t *testing.T) {
	orig := compressFileFn
	defer func() { compressFileFn = orig }()

	cases := []struct {
		name string
		fn   func(src, dst string) (int64, error)
	}{
		{"compressor fails before writing anything", func(string, string) (int64, error) {
			return 0, errors.New("injected: compressor failed")
		}},
		{"compressor fails after writing a partial temp", func(_, dst string) (int64, error) {
			if err := os.WriteFile(dst+".tmp", []byte("partial output"), 0o644); err != nil {
				return 0, err
			}
			return 0, errors.New("injected: compressor died mid-write")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := seedDB(t, 40)
			dir := t.TempDir()
			before := dirSnapshot(t, dir)

			compressFileFn = tc.fn
			_, err := Backup(src, filepath.Join(dir, Filename(TagDaily, time.Now())))
			if err == nil {
				t.Fatal("expected the injected failure to surface")
			}

			after := dirSnapshot(t, dir)
			if len(after) != len(before) {
				t.Errorf("directory changed after a failed backup:\nbefore %v\nafter  %v",
					before, after)
			}
			for _, n := range after {
				t.Errorf("leftover file after failed backup: %s", n)
			}
		})
	}
}

// dirSnapshot lists every entry in dir, including dot-prefixed ones —
// which is the point: the stranded VACUUM temps were invisible to a
// plain ls precisely because their names start with a dot.
func dirSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}
