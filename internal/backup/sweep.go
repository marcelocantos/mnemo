// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultOrphanAge is how old a scratch file must be before the sweep
// will remove it. A backup of an 18 GB index spends about two minutes in
// VACUUM INTO, so six hours is three orders of magnitude of headroom
// against the only legitimate reason a scratch file is still present.
const DefaultOrphanAge = 6 * time.Hour

// liveTemps records scratch paths this process is currently writing, so
// the sweep never removes a file out from under a backup running
// concurrently in the same daemon. Cross-process safety comes from the
// age threshold instead: mnemo is single-writer by design (one process
// owns mnemo.db), so a second daemon mid-VACUUM is not a state the
// product produces.
var liveTemps sync.Map // path -> struct{}

func markTempLive(path string)    { liveTemps.Store(path, struct{}{}) }
func markTempDone(path string)    { liveTemps.Delete(path) }
func tempIsLive(path string) bool { _, ok := liveTemps.Load(path); return ok }

// SweptFile describes one file the sweep removed.
type SweptFile struct {
	Path   string
	Size   int64
	Age    time.Duration
	Reason string
}

// SweepOrphans removes stale scratch files from a backup directory:
// VACUUM INTO temps (`.backup-*.db` and their `-journal` / `-wal` /
// `-shm` siblings) and half-written compressor output (`*.tmp`).
//
// These are the files that made retention look like it was working while
// the directory grew without bound (🎯T158). Backup's own defers clean
// them up when it returns — even on error — so everything this finds is
// the residue of a process that died mid-backup, where no defer ran. A
// hard kill during the VACUUM of a large index strands a file the size
// of the whole database; seven of them, 104 GB, is what prompted this.
//
// Two things stop it deleting a file that is still in use: the path must
// not be one this process is currently writing, and it must be older
// than minAge (0 selects DefaultOrphanAge). Files it does not recognise
// are left alone — a sweep that guessed would be a worse bug than the
// one it fixes, since this is the code that deletes things.
func SweepOrphans(dir string, minAge time.Duration) ([]SweptFile, error) {
	if minAge <= 0 {
		minAge = DefaultOrphanAge
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	now := time.Now()
	var swept []SweptFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		reason, ok := orphanReason(name)
		if !ok {
			continue
		}
		path := filepath.Join(dir, name)
		if tempIsLive(path) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		age := now.Sub(fi.ModTime())
		if age < minAge {
			continue
		}
		if err := os.Remove(path); err != nil {
			slog.Warn("backup sweep: failed to remove orphan",
				"path", path, "err", err)
			continue
		}
		swept = append(swept, SweptFile{
			Path: path, Size: fi.Size(), Age: age, Reason: reason,
		})
	}
	if len(swept) > 0 {
		var bytes int64
		for _, s := range swept {
			bytes += s.Size
		}
		slog.Info("backup sweep: removed stale scratch files",
			"count", len(swept), "freed_mb", bytes/(1<<20), "dir", dir)
		for _, s := range swept {
			slog.Info("backup sweep: removed",
				"path", s.Path, "size_mb", s.Size/(1<<20),
				"age", s.Age.Round(time.Minute), "reason", s.Reason)
		}
	}
	return swept, nil
}

// orphanReason classifies a filename as sweepable scratch, naming why.
// It recognises only what this package itself creates.
func orphanReason(name string) (string, bool) {
	// os.CreateTemp(destDir, ".backup-*.db") — the VACUUM INTO target,
	// plus any SQLite sidecar left beside it.
	if strings.HasPrefix(name, ".backup-") {
		switch {
		case strings.HasSuffix(name, ".db"):
			return "stranded VACUUM INTO snapshot", true
		case strings.HasSuffix(name, ".db-journal"),
			strings.HasSuffix(name, ".db-wal"),
			strings.HasSuffix(name, ".db-shm"):
			return "stranded VACUUM INTO sidecar", true
		}
		return "", false
	}
	// compressFile writes destPath+".tmp" and renames. A leftover means
	// the compressor died before the rename.
	if strings.HasPrefix(name, "mnemo-") && strings.HasSuffix(name, ".tmp") {
		return "half-written compressor output", true
	}
	return "", false
}

// DiskUsage reports what a backup directory actually costs, which is not
// the same question as how many snapshots are retained.
//
// Retention counts files it recognises; this counts every byte in the
// directory. The gap between the two is exactly where 187 GB hid
// (🎯T158), so both numbers are reported and a caller comparing them can
// see the discrepancy rather than infer it.
type DiskUsage struct {
	TotalBytes    int64 // every file in the directory
	RetainedBytes int64 // files retention recognises as backups
	RetainedCount int
	OtherBytes    int64 // TotalBytes - RetainedBytes: scratch, orphans, strays
	OtherCount    int
}

// Usage measures dir. A missing directory is zero, not an error.
func Usage(dir string) (DiskUsage, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return DiskUsage{}, nil
		}
		return DiskUsage{}, err
	}
	var u DiskUsage
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		u.TotalBytes += fi.Size()
		if _, _, ok := parseFilename(e.Name()); ok {
			u.RetainedBytes += fi.Size()
			u.RetainedCount++
		} else {
			u.OtherBytes += fi.Size()
			u.OtherCount++
		}
	}
	return u, nil
}

// String renders a DiskUsage for logs and status output.
func (u DiskUsage) String() string {
	s := fmt.Sprintf("%d backups, %.2f GiB", u.RetainedCount, gib(u.RetainedBytes))
	if u.OtherCount > 0 {
		s += fmt.Sprintf(" (plus %d non-backup files, %.2f GiB)",
			u.OtherCount, gib(u.OtherBytes))
	}
	return s
}

func gib(b int64) float64 { return float64(b) / (1 << 30) }
