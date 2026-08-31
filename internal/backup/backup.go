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
//	mnemo-{tag}-YYYYMMDDTHHMMSSZ.db.zst
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
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/klauspost/compress/zstd"

	"github.com/marcelocantos/mnemo/internal/zstdc"
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
	Path           string        // final on-disk path of the compressed backup
	RawSize        int64         // size of the VACUUM INTO output before compression
	CompressedSize int64         // size of the on-disk .db.zst file
	Elapsed        time.Duration // total wall-clock for VACUUM + integrity + compress + verify
}

// Filename returns the canonical backup filename for the given tag and
// timestamp. The format is sortable lexicographically by chronological
// order, so worker rotation logic can just sort by name.
func Filename(tag Tag, t time.Time) string {
	return fmt.Sprintf("mnemo-%s-%s%s", tag, t.UTC().Format("20060102T150405Z"), ExtZstd)
}

// Backup file extensions. ExtZstd is what new backups are written with
// (🎯T159); ExtGzip is still recognised so snapshots taken before the
// switch remain listed, rotated and restorable.
const (
	ExtZstd = ".db.zst"
	ExtGzip = ".db.gz"
)

// BackupArgs configures optional Backup behaviour. Zero value is fine.
type BackupArgs struct {
	// OnStep is called with a short human label at each long stage
	// (vacuum_into, integrity_check, gzip). Used by pre-migration so
	// /health can show which multi-minute step is in progress. Nil is OK.
	OnStep func(step string)
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
//  3. Compress with multithreaded zstd to destPath.tmp, then verify the
//     artefact reads back before returning.
//  4. fsync + atomic rename to destPath.
//
// On any failure the function leaves no partial output (temp files are
// cleaned up). The compressed backup at destPath either exists fully or
// does not exist at all.
func Backup(srcPath, destPath string) (Result, error) {
	return BackupWith(srcPath, destPath, nil)
}

// BackupWith is Backup plus optional progress callbacks (see BackupArgs).
func BackupWith(srcPath, destPath string, args *BackupArgs) (_ Result, err error) {
	start := time.Now()
	step := func(s string) {
		if args != nil && args.OnStep != nil {
			args.OnStep(s)
		}
	}
	if !strings.HasSuffix(destPath, ExtZstd) {
		return Result{}, fmt.Errorf("destPath must end in %s: %s", ExtZstd, destPath)
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
	// Register before the long stages so a concurrent sweep in this
	// process cannot mistake an in-progress VACUUM for an orphan.
	markTempLive(tmpDBPath)
	defer markTempDone(tmpDBPath)
	defer os.Remove(tmpDBPath)

	step("VACUUM INTO (consistent snapshot)")
	if err := vacuumInto(srcPath, tmpDBPath); err != nil {
		return Result{}, err
	}

	rawInfo, err := os.Stat(tmpDBPath)
	if err != nil {
		return Result{}, fmt.Errorf("stat tmpdb: %w", err)
	}

	step("integrity_check on snapshot")
	if err := integrityCheck(tmpDBPath); err != nil {
		return Result{}, err
	}

	step(fmt.Sprintf("compressing %d MB snapshot", rawInfo.Size()/(1<<20)))
	// Own the destination namespace's cleanup here rather than trusting
	// the compressor to tidy up after itself. compressFile does remove
	// its own .tmp on error, but this function's documented contract is
	// that a failure leaves NOTHING behind, and a contract that depends
	// on a collaborator's diligence is one refactor from being false.
	defer func() {
		if err != nil {
			os.Remove(destPath + ".tmp")
		}
	}()
	size, err := compressFileFn(tmpDBPath, destPath)
	if err != nil {
		return Result{}, err
	}

	// Verify the artefact, not just the snapshot that went into it.
	// integrity_check above proves the DATABASE was sound; this proves the
	// FILE we are about to keep can be read back. It matters more since
	// retention became one snapshot (🎯T158): the caller deletes the
	// previous backup after this returns, so an unreadable output would
	// leave nothing. Decompression validates the frame's XXH64 checksum
	// end to end and costs seconds at zstd speeds.
	step("verifying compressed snapshot")
	if err := verifyCompressed(destPath, rawInfo.Size()); err != nil {
		os.Remove(destPath)
		return Result{}, err
	}

	return Result{
		Path:           destPath,
		RawSize:        rawInfo.Size(),
		CompressedSize: size,
		Elapsed:        time.Since(start),
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

// compressFileFn is the seam a test uses to fail compression mid-backup
// and assert nothing is left behind. Production always uses compressFile.
var compressFileFn = compressFile

// compressFile compresses srcPath into destPath (atomic rename via .tmp)
// using the vendored multithreaded libzstd.
//
// It replaced gzip level 1 (🎯T159). End to end on the live 18.2 GB
// index, 16 workers: 8.5s at ratio 0.548 (2132 MB/s), against gzip -1's
// ~83 MB/s — a ~3.8 minute CPU burn every backup. Verification adds 11.2s
// (single-threaded decode), so the whole compress-and-check stage costs
// about 20s where compression alone used to cost 228s.
//
// The size was a bonus; the time was the point. Level 3 is zstd's own
// default and sits at the knee — higher levels buy a few percent for
// multiples of the CPU, which is the wrong trade for a snapshot that gets
// replaced tomorrow.
func compressFile(srcPath, destPath string) (int64, error) {
	tmpPath := destPath + ".tmp"
	markTempLive(tmpPath)
	defer markTempDone(tmpPath)
	// 0 workers means one per CPU. libzstd stitches the parallel jobs into
	// a single frame, so the artefact is identical in shape to one written
	// single-threaded.
	st, err := zstdc.CompressFile(srcPath, tmpPath, zstdc.LevelFast, 0)
	if err != nil {
		os.Remove(tmpPath)
		return 0, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("rename compressed backup: %w", err)
	}
	return st.OutBytes, nil
}

// verifyCompressed reads path back through the pure-Go decoder, checking
// that it decompresses cleanly and yields wantBytes. Decompression
// validates the frame's embedded XXH64 checksum, so this catches a
// truncated or corrupted artefact before the caller retires the previous
// backup.
func verifyCompressed(path string, wantBytes int64) error {
	n, err := decompressTo(path, io.Discard)
	if err != nil {
		return fmt.Errorf("verify %s: %w", filepath.Base(path), err)
	}
	if n != wantBytes {
		return fmt.Errorf("verify %s: decompressed to %d bytes, snapshot was %d",
			filepath.Base(path), n, wantBytes)
	}
	return nil
}

// Decompress expands a backup file to destPath. It reads both .db.zst and
// .db.gz, so a snapshot taken before 🎯T159 is still restorable.
//
// It exists so restoring never depends on having the zstd CLI installed:
// a disaster-recovery artefact that needs a package manager first is a
// poor disaster-recovery artefact. gzip was universally available; zstd
// is not yet, so mnemo carries its own reader.
func Decompress(srcPath, destPath string) (int64, error) {
	out, err := os.Create(destPath)
	if err != nil {
		return 0, fmt.Errorf("create restore target: %w", err)
	}
	n, err := decompressTo(srcPath, out)
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(destPath)
		return 0, err
	}
	return n, nil
}

// decompressTo streams srcPath into w, picking the reader from the file
// extension. Returns the number of uncompressed bytes.
func decompressTo(srcPath string, w io.Writer) (int64, error) {
	in, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	var r io.Reader
	switch {
	case strings.HasSuffix(srcPath, ExtZstd), strings.HasSuffix(srcPath, ExtZstd+".tmp"):
		// Pure Go on purpose: only compression needs cgo, so restoring
		// works in any build.
		dec, err := zstd.NewReader(in)
		if err != nil {
			return 0, err
		}
		defer dec.Close()
		r = dec
	case strings.HasSuffix(srcPath, ExtGzip):
		gr, err := gzip.NewReader(in)
		if err != nil {
			return 0, err
		}
		defer gr.Close()
		r = gr
	default:
		return 0, fmt.Errorf("unrecognised backup extension: %s", filepath.Base(srcPath))
	}
	return io.Copy(w, r)
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
