// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package backup produces compressed, atomic snapshots of mnemo's SQLite
// database via SQLite's `VACUUM INTO`. Snapshots are safe against a
// concurrent daemon (the source is opened read-only; SQLite serializes
// the snapshot via a shared lock) and the output is a fully-consistent
// standalone DB file — no WAL replay needed to restore.
//
// On-disk layout under the backup directory:
//
//	mnemo-{tag}-YYYYMMDDTHHMMSSZ.db.gz
//
// where {tag} is "daily" for the periodic snapshot or "pre-migration"
// for the one taken before sqlift.Apply. Filenames are sortable by
// recency (lexicographic == chronological).
//
// The package deliberately does not own the schedule, retention policy,
// or storage backend — those live in internal/registry as a worker
// goroutine. This package is the I/O primitive only.
package backup

import (
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Tag classifies a backup by what triggered it.
type Tag string

const (
	TagDaily        Tag = "daily"
	TagPreMigration Tag = "pre-migration"
	TagManual       Tag = "manual"
)

// Result reports timing and sizes from a Backup call.
type Result struct {
	Path        string        // final on-disk path of the compressed backup
	RawSize     int64         // size of the VACUUM INTO output before compression
	GzippedSize int64         // size of the on-disk .gz file
	Elapsed     time.Duration // total wall-clock for VACUUM + integrity + gzip
}

// Filename returns the canonical backup filename for the given tag and
// timestamp. The format is sortable lexicographically by chronological
// order, so worker rotation logic can just sort by name.
func Filename(tag Tag, t time.Time) string {
	return fmt.Sprintf("mnemo-%s-%s.db.gz", tag, t.UTC().Format("20060102T150405Z"))
}

// Backup snapshots the SQLite database at srcPath into destPath. destPath
// must end in .db.gz (caller's responsibility — typically constructed via
// filepath.Join(dir, Filename(tag, time.Now()))). destPath's parent
// directory must exist and be writable.
//
// Mechanism:
//  1. VACUUM INTO produces a fully-consistent standalone DB at a sibling
//     temp file (same filesystem, so the later rename is atomic).
//  2. PRAGMA integrity_check on the snapshot — bail if corrupted.
//  3. Gzip-compress (level 1 — favours speed over size; the consecutive-
//     snapshot redundancy gzip can't catch is left for a future zstd-dict
//     or chunked-dedup pass) to destPath.tmp.
//  4. fsync + atomic rename to destPath.
//
// On any failure the function leaves no partial output (temp files are
// cleaned up). The compressed backup at destPath either exists fully or
// does not exist at all.
func Backup(srcPath, destPath string) (Result, error) {
	start := time.Now()
	if filepath.Ext(destPath) != ".gz" {
		return Result{}, fmt.Errorf("destPath must end in .gz: %s", destPath)
	}
	destDir := filepath.Dir(destPath)

	// VACUUM INTO target. Same directory as destPath so the eventual gzip
	// rename is on the same filesystem.
	tmpDBFile, err := os.CreateTemp(destDir, ".backup-*.db")
	if err != nil {
		return Result{}, fmt.Errorf("alloc tmpdb: %w", err)
	}
	tmpDBPath := tmpDBFile.Name()
	tmpDBFile.Close()
	// VACUUM INTO refuses to write to an existing file. Remove the placeholder
	// CreateTemp left behind, then ensure cleanup on any exit.
	if err := os.Remove(tmpDBPath); err != nil {
		return Result{}, fmt.Errorf("remove tmpdb placeholder: %w", err)
	}
	defer os.Remove(tmpDBPath)

	if err := vacuumInto(srcPath, tmpDBPath); err != nil {
		return Result{}, err
	}

	rawInfo, err := os.Stat(tmpDBPath)
	if err != nil {
		return Result{}, fmt.Errorf("stat tmpdb: %w", err)
	}

	if err := integrityCheck(tmpDBPath); err != nil {
		return Result{}, err
	}

	gzSize, err := gzipFile(tmpDBPath, destPath)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Path:        destPath,
		RawSize:     rawInfo.Size(),
		GzippedSize: gzSize,
		Elapsed:     time.Since(start),
	}, nil
}

// vacuumInto runs `VACUUM INTO destPath` against a read-only handle of
// srcPath. The destination must not exist; SQLite refuses to overwrite.
func vacuumInto(srcPath, destPath string) error {
	// mode=ro takes a shared lock; safe alongside a concurrent writer.
	// _txlock=deferred avoids holding an exclusive lock unnecessarily.
	dsn := fmt.Sprintf("file:%s?mode=ro&_txlock=deferred", srcPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer db.Close()

	// SQLite quotes the path in single-quotes; escape any embedded single
	// quote by doubling. Path is caller-controlled so this is defensive
	// rather than necessary in practice, but cheap.
	quoted := "'" + replaceAll(destPath, "'", "''") + "'"
	if _, err := db.Exec("VACUUM INTO " + quoted); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// integrityCheck opens dbPath read-only and runs PRAGMA integrity_check.
// Returns nil only when SQLite reports "ok" — any other result is a
// corruption signal and we bail out of the backup rather than ship a
// broken snapshot.
func integrityCheck(dbPath string) error {
	dsn := fmt.Sprintf("file:%s?mode=ro", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("reopen for integrity_check: %w", err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity_check exec: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check reported corruption: %s", result)
	}
	return nil
}

// gzipFile compresses srcPath into destPath (atomic rename via .tmp). Uses
// gzip BestSpeed; the consecutive-snapshot redundancy that gzip can't catch
// is left for a future dedup pass.
func gzipFile(srcPath, destPath string) (int64, error) {
	tmpPath := destPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return 0, fmt.Errorf("create gz tmp: %w", err)
	}
	cleanup := func() { out.Close(); os.Remove(tmpPath) }

	gw, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("gzip writer: %w", err)
	}

	in, err := os.Open(srcPath)
	if err != nil {
		gw.Close()
		cleanup()
		return 0, fmt.Errorf("open src for gzip: %w", err)
	}
	if _, err := io.Copy(gw, in); err != nil {
		in.Close()
		gw.Close()
		cleanup()
		return 0, fmt.Errorf("gzip copy: %w", err)
	}
	in.Close()
	if err := gw.Close(); err != nil {
		cleanup()
		return 0, fmt.Errorf("gzip close: %w", err)
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return 0, fmt.Errorf("fsync gz: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("close gz: %w", err)
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("stat gz: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("rename gz: %w", err)
	}
	return fi.Size(), nil
}

// replaceAll is a stdlib-equivalent helper kept local so backup.go has no
// strings import (the rest of the package doesn't need it either).
func replaceAll(s, old, new string) string {
	if old == "" || old == new {
		return s
	}
	var b []byte
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			b = append(b, new...)
			i += len(old)
			continue
		}
		b = append(b, s[i])
		i++
	}
	return string(b)
}
