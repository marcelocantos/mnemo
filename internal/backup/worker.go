// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ActivityProvider reports the wall-clock time of the most recent write
// activity. The worker reads this to detect a quiescent window before
// snapshotting. The zero Time is interpreted as "no activity yet" —
// fully quiescent.
type ActivityProvider interface {
	LastWriteAt() time.Time
}

// Worker is the periodic backup goroutine. Constructed by NewWorker and
// driven by Run; cancel its context to stop. Concurrency model: a single
// Run call is in flight at a time; the registry starts one worker per
// daemon process.
type Worker struct {
	src         string
	dir         string
	keep        int
	windowStart time.Duration // offset from midnight, e.g. 3h for 03:00
	windowEnd   time.Duration // offset from midnight, e.g. 4h for 04:00
	quiescence  time.Duration
	activity    ActivityProvider
	rng         *rand.Rand
	// pollInterval gates how often the worker re-checks for quiescence
	// inside the daily window. Test hook so we can run a fast version
	// in unit tests; production uses time.Minute.
	pollInterval time.Duration
}

// Config bundles the parameters a Worker needs. Resolved from
// store.BackupConfig at construction time so the worker doesn't depend
// on the store package.
type Config struct {
	SrcPath      string           // path to mnemo.db
	Dir          string           // backup directory
	Keep         int              // number of backups to retain
	WindowStart  time.Duration    // offset from midnight (e.g. 3h)
	WindowEnd    time.Duration    // offset from midnight (e.g. 4h)
	Quiescence   time.Duration    // min idle period before snapshotting
	Activity     ActivityProvider // reads LastWriteAt for quiescence
	PollInterval time.Duration    // re-check cadence inside the window (default 1m)
	Seed         uint64           // RNG seed; 0 → time-based
}

// NewWorker constructs a Worker from cfg. Returns an error if cfg fields
// are missing or unusable. Doesn't start the worker — call Run.
func NewWorker(cfg Config) (*Worker, error) {
	if cfg.SrcPath == "" {
		return nil, fmt.Errorf("backup.NewWorker: SrcPath is required")
	}
	if cfg.Dir == "" {
		return nil, fmt.Errorf("backup.NewWorker: Dir is required")
	}
	if cfg.Keep <= 0 {
		return nil, fmt.Errorf("backup.NewWorker: Keep must be >0, got %d", cfg.Keep)
	}
	if cfg.WindowEnd <= cfg.WindowStart {
		return nil, fmt.Errorf("backup.NewWorker: WindowEnd must be > WindowStart")
	}
	if cfg.Activity == nil {
		return nil, fmt.Errorf("backup.NewWorker: Activity is required")
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = time.Minute
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	return &Worker{
		src:          cfg.SrcPath,
		dir:          cfg.Dir,
		keep:         cfg.Keep,
		windowStart:  cfg.WindowStart,
		windowEnd:    cfg.WindowEnd,
		quiescence:   cfg.Quiescence,
		activity:     cfg.Activity,
		rng:          rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15)),
		pollInterval: poll,
	}, nil
}

// Run starts the worker loop. Blocks until ctx is done.
//
// Per cycle:
//  1. Compute the next attempt time: a random instant in the next
//     [WindowStart, WindowEnd) local-time window.
//  2. Sleep until that time.
//  3. Wait for ≥Quiescence of inactivity. Poll every PollInterval. If
//     no quiescent moment is observed by WindowEnd+1h, log warn and
//     skip the day.
//  4. Snapshot via Backup() into dir/Filename(TagDaily, now).
//  5. GC: keep only the most-recent Keep backups across all tags.
//  6. Loop back to step 1 (tomorrow's window).
func (w *Worker) Run(ctx context.Context) {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		slog.Error("backup worker: cannot create backup dir; worker exiting",
			"dir", w.dir, "err", err)
		return
	}
	for {
		next := w.scheduleNext(time.Now())
		slog.Info("backup worker: next attempt scheduled", "at", next)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		w.attemptBackup(ctx)
	}
}

// scheduleNext computes the next backup-attempt instant: a random time
// inside the next [windowStart, windowEnd) window relative to now, in
// the local timezone.
func (w *Worker) scheduleNext(now time.Time) time.Time {
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := today.Add(w.windowStart)
	end := today.Add(w.windowEnd)
	if !now.Before(end) {
		// Today's window has already passed; move to tomorrow.
		start = start.Add(24 * time.Hour)
		end = end.Add(24 * time.Hour)
	} else if now.After(start) {
		// We're inside today's window already (daemon started during
		// the window). Aim for a random instant in the remaining span.
		start = now
	}
	spread := end.Sub(start)
	if spread <= 0 {
		// Window collapsed (window_end too close to "now"); pick
		// in the next 24h instead.
		return start.Add(24 * time.Hour)
	}
	return start.Add(time.Duration(w.rng.Int64N(int64(spread))))
}

// attemptBackup waits for quiescence then snapshots. Returns silently on
// any failure (errors are logged); the worker loop continues regardless.
func (w *Worker) attemptBackup(ctx context.Context) {
	deadline := time.Now().Add(time.Hour) // bail if not quiescent within an hour past window_end
	for {
		if w.isQuiescent() {
			break
		}
		if time.Now().After(deadline) {
			slog.Warn("backup worker: no quiescent moment observed within deadline; skipping today",
				"last_write_at", w.activity.LastWriteAt())
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.pollInterval):
		}
	}

	destPath := filepath.Join(w.dir, Filename(TagDaily, time.Now().UTC()))
	res, err := Backup(w.src, destPath)
	if err != nil {
		slog.Error("backup worker: snapshot failed", "err", err)
		return
	}
	slog.Info("backup worker: snapshot complete",
		"path", res.Path,
		"raw_mb", res.RawSize/(1<<20),
		"gz_mb", res.GzippedSize/(1<<20),
		"elapsed", res.Elapsed.Round(time.Second))

	if removed, err := GCOldest(w.dir, w.keep); err != nil {
		slog.Warn("backup worker: GC failed; old backups may accumulate",
			"err", err)
	} else if len(removed) > 0 {
		slog.Info("backup worker: GC'd old backups", "count", len(removed))
	}
}

// isQuiescent reports whether enough time has passed since the last
// recorded write activity for a backup to be safe. Treats zero last-write
// (no activity yet) as fully quiescent.
func (w *Worker) isQuiescent() bool {
	last := w.activity.LastWriteAt()
	if last.IsZero() {
		return true
	}
	return time.Since(last) >= w.quiescence
}

// Info describes a single backup file on disk.
type Info struct {
	Path string
	Name string
	Tag  Tag
	Time time.Time
	Size int64
}

// List enumerates backup files in dir, parsing tag and timestamp from
// the canonical filename. Returns entries sorted newest-first.
//
// Non-matching files (.tmp, .backup-*.db scratch files, files dropped by
// the user) are silently skipped — List is a read-only inventory and
// doesn't mutate the directory.
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		tag, ts, ok := parseFilename(name)
		if !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{
			Path: filepath.Join(dir, name),
			Name: name,
			Tag:  tag,
			Time: ts,
			Size: fi.Size(),
		})
	}
	// Newest first.
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// GCOldest deletes the oldest backups in dir beyond `keep`. Returns
// the list of removed file paths (empty if nothing to remove). All
// backups share a single retention pool — daily, pre-migration, and
// manual backups compete for the same N slots.
func GCOldest(dir string, keep int) ([]string, error) {
	if keep < 1 {
		return nil, fmt.Errorf("GCOldest: keep must be >0, got %d", keep)
	}
	list, err := List(dir)
	if err != nil {
		return nil, err
	}
	if len(list) <= keep {
		return nil, nil
	}
	var removed []string
	for _, b := range list[keep:] {
		if err := os.Remove(b.Path); err != nil {
			slog.Warn("backup GC: failed to remove",
				"path", b.Path, "err", err)
			continue
		}
		removed = append(removed, b.Path)
	}
	return removed, nil
}

// parseFilename inverts Filename: extracts (tag, time) from a string
// like "mnemo-daily-20260518T031742Z.db.gz". Returns ok=false on any
// shape mismatch — caller treats that as "not a backup file".
func parseFilename(name string) (Tag, time.Time, bool) {
	const prefix = "mnemo-"
	const suffix = ".db.gz"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return "", time.Time{}, false
	}
	mid := name[len(prefix) : len(name)-len(suffix)]
	// Split tag from timestamp. Timestamp is the trailing
	// YYYYMMDDTHHMMSSZ component (16 chars). Tag is everything before
	// the final dash that separates them.
	idx := strings.LastIndex(mid, "-")
	if idx < 1 || idx == len(mid)-1 {
		return "", time.Time{}, false
	}
	tag := Tag(mid[:idx])
	ts, err := time.Parse("20060102T150405Z", mid[idx+1:])
	if err != nil {
		return "", time.Time{}, false
	}
	return tag, ts, true
}
