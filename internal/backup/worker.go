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
//  4. Snapshot into dir/Filename(TagDaily, now) and apply retention,
//     via CreateAndRetain.
//  5. Loop back to step 1 (tomorrow's window).
//
// Housekeeping — the sweep for stranded scratch files, and retention
// itself — runs at startup and again every cycle, whether or not a
// backup was taken. That independence is the point: retention used to
// run only after a SUCCESSFUL DAILY backup, so a run of failed backups,
// a disabled schedule, or a day of migrations pruned nothing while
// snapshots kept arriving (🎯T158).
func (w *Worker) Run(ctx context.Context) {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		slog.Error("backup worker: cannot create backup dir; worker exiting",
			"dir", w.dir, "err", err)
		return
	}
	// At startup, before waiting a day for the first window: an orphan
	// from the crash that just happened should not survive until
	// tomorrow morning, and a directory over retention should come down
	// now.
	w.housekeep()
	for {
		next := w.scheduleNext(time.Now())
		slog.Info("backup worker: next attempt scheduled", "at", next)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		// Housekeeping first, so it happens even on a day the backup
		// itself is skipped for lack of a quiescent window.
		w.housekeep()
		w.attemptBackup(ctx)
	}
}

// housekeep sweeps stranded scratch files and enforces retention,
// independently of whether a backup succeeds. Both steps log their own
// failures and neither is fatal to the worker.
func (w *Worker) housekeep() {
	if _, err := SweepOrphans(w.dir, DefaultOrphanAge); err != nil {
		slog.Warn("backup worker: orphan sweep failed", "dir", w.dir, "err", err)
	}
	removed, err := GCOldest(w.dir, w.keep)
	if err != nil {
		slog.Warn("backup worker: scheduled retention failed", "dir", w.dir, "err", err)
		return
	}
	if len(removed) > 0 {
		slog.Info("backup worker: scheduled retention removed old snapshots",
			"count", len(removed), "keep", w.keep)
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

	res, removed, err := CreateAndRetain(w.src, w.dir, TagDaily, w.keep, nil)
	if err != nil {
		slog.Error("backup worker: snapshot failed", "err", err)
		return
	}
	slog.Info("backup worker: snapshot complete",
		"path", res.Path,
		"raw_mb", res.RawSize/(1<<20),
		"compressed_mb", res.CompressedSize/(1<<20),
		"elapsed", res.Elapsed.Round(time.Second),
		"retention_removed", len(removed))
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
	// BOTH extensions, deliberately. Retention only manages files this
	// function recognises, so a format switch that taught it the new
	// suffix and forgot the old one — or vice versa — would leave a pile
	// of snapshots invisible to GC and growing forever. That is precisely
	// how ~187 GB of unmanaged files accumulated beside a correctly
	// functioning retention pool (🎯T158).
	suffix := ""
	for _, ext := range []string{ExtZstd, ExtGzip} {
		if strings.HasSuffix(name, ext) {
			suffix = ext
			break
		}
	}
	if !strings.HasPrefix(name, prefix) || suffix == "" {
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

// CreateAndRetain takes a snapshot and immediately applies retention.
//
// This exists because "take a backup" and "prune old backups" were
// separate calls, and only ONE of the three call sites made both
// (🎯T158). The pre-migration path and `mnemo_ops op=backup_now` each
// created snapshots and pruned nothing, so a day of migrations — or a
// few manual backups — grew the directory without bound while the daily
// worker's retention looked healthy. At keep=1 a non-pruning path
// doubles the directory every time it runs.
//
// Fixing the two call sites would have fixed the bug; making retention
// part of taking a backup is what stops the fourth call site from
// reintroducing it. New code should reach for this, not Backup.
//
// Retention failure is reported but does not fail the backup: a snapshot
// that exists and was verified is worth more than a tidy directory, and
// the caller has already paid for it.
func CreateAndRetain(srcPath, dir string, tag Tag, keep int, args *BackupArgs) (Result, []string, error) {
	if keep < 1 {
		keep = 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, nil, fmt.Errorf("create backup dir: %w", err)
	}
	destPath, err := uniqueDest(dir, tag, time.Now().UTC())
	if err != nil {
		return Result{}, nil, err
	}
	res, err := BackupWith(srcPath, destPath, args)
	if err != nil {
		return Result{}, nil, err
	}
	removed, gcErr := GCOldest(dir, keep)
	if gcErr != nil {
		slog.Warn("backup: snapshot written but retention failed; old backups may accumulate",
			"dir", dir, "err", gcErr)
	} else if len(removed) > 0 {
		slog.Info("backup: retention removed old snapshots",
			"count", len(removed), "keep", keep)
	}
	return res, removed, nil
}

// uniqueDest picks a destination path that does not already exist.
//
// Filename's timestamp has one-second granularity, so two backups
// started in the same second name the same file and the second silently
// overwrites the first — the rename at the end of Backup replaces
// whatever is there. Backups normally take minutes, so this needs two
// forced manual runs in quick succession to hit, which is exactly what
// happened while testing op=backup_now. Nothing is corrupted, but a
// snapshot the caller was told was written has quietly replaced another,
// and with keep>1 the directory ends up holding fewer snapshots than
// retention promises.
//
// Advancing a second at a time keeps the canonical filename shape, so
// parseFilename, ordering and retention are unaffected.
func uniqueDest(dir string, tag Tag, when time.Time) (string, error) {
	const maxAttempts = 60
	for i := 0; i < maxAttempts; i++ {
		p := filepath.Join(dir, Filename(tag, when.Add(time.Duration(i)*time.Second)))
		_, err := os.Stat(p)
		if os.IsNotExist(err) {
			return p, nil
		}
		if err != nil {
			return "", fmt.Errorf("stat candidate backup path: %w", err)
		}
	}
	return "", fmt.Errorf("no free backup filename for tag %s near %s after %d attempts",
		tag, when.Format(time.RFC3339), maxAttempts)
}
