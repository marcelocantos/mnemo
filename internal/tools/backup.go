// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/mnemo/internal/store"
)

// osMkdirAll wraps os.MkdirAll so the mkdirAll var above can shadow it
// in tests without needing build tags.
func osMkdirAll(dir string, mode os.FileMode) error { return os.MkdirAll(dir, mode) }

// backupDir returns the directory snapshots are written to and listed
// from: the configured `backup.dir`, or ~/.mnemo/backups. It is the
// value the nightly worker uses; these tools used to hardcode the
// default, so with a custom directory backup_status reported on a folder
// the worker had stopped using and backup_now wrote to it (🎯T199).
func (h *callHandler) backupDir() string {
	return h.mem.BackupDir()
}

// statusDir returns where the record of the last attempt lives: always
// beside the database, so it can be read and written while a custom
// destination is away.
func (h *callHandler) statusDir() string {
	return backup.DefaultDir(h.mem.DBPath())
}

// backupKeep resolves the retention policy the same way the worker does,
// so a manual backup prunes to the configured depth rather than to a
// number of its own. An unreadable config falls back to the defaults.
func backupKeep() backup.Retention {
	cfg, err := store.LoadConfig()
	if err != nil {
		return store.BackupConfig{}.Retention()
	}
	return cfg.Backup.Retention()
}

// backupStatus implements mnemo_backup_status: list backups newest-first
// with tag, age, and size. Output is text rather than JSON because it's
// human-consumed (the typical caller is an agent paraphrasing it for the
// user, or the user running it directly).
func (h *callHandler) backupStatus() (string, bool, error) {
	dir := h.backupDir()
	head := backupProgress(h.statusDir())
	// Not "No backups found": the snapshots are on a volume that is not
	// attached, which is a different fact with a different remedy.
	if err := backup.Away(dir, h.statusDir()); err != nil {
		return head + fmt.Sprintf("Snapshots not listed: %v.\n"+
			"A nightly snapshot skipped because "+
			"the destination was missing is taken at the first quiet moment after it returns; "+
			"nothing is written anywhere else in the meantime.", err), false, nil
	}
	list, err := backup.List(dir)
	if err != nil {
		return fmt.Sprintf("backup status failed: %v", err), true, nil
	}
	if len(list) == 0 {
		return head + fmt.Sprintf("No backups found in %s.\nThe daemon's daily worker runs in the 03:00-04:00 local window; pre-migration backups fire when sqlift.Apply has a non-empty plan. Use mnemo_backup_now to trigger one manually.", dir), false, nil
	}
	var b strings.Builder
	b.WriteString(head)
	fmt.Fprintf(&b, "Backups in %s\n\n", dir)
	fmt.Fprintf(&b, "%-32s %-14s %10s %12s\n", "Filename", "Tag", "Size", "Age")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 72))
	now := time.Now()
	var totalBytes int64
	for _, info := range list {
		age := now.Sub(info.Time).Round(time.Minute)
		fmt.Fprintf(&b, "%-32s %-14s %10s %12s\n",
			info.Name, string(info.Tag), humanSize(info.Size), humanAge(age))
		totalBytes += info.Size
	}
	fmt.Fprintf(&b, "\nTotal: %d backups, %s\n", len(list), humanSize(totalBytes))

	// What retention manages is not the same as what the directory
	// costs. Reporting only the first number is how 187 GB of scratch
	// files sat unnoticed beside five correctly-retained backups
	// (🎯T158), so the gap is printed rather than left to be inferred.
	if u, err := backup.Usage(dir); err == nil {
		fmt.Fprintf(&b, "On disk:  %s total in the directory\n", humanSize(u.TotalBytes))
		if u.OtherCount > 0 {
			fmt.Fprintf(&b, "\nWARNING: %d file(s) totalling %s are not backups retention "+
				"recognises — scratch files from an interrupted run, or strays. The "+
				"daemon sweeps its own scratch files older than %s; anything else needs "+
				"a look.\n", u.OtherCount, humanSize(u.OtherBytes), backup.DefaultOrphanAge)
		}
	}
	return b.String(), false, nil
}

// backupNow implements mnemo_backup_now: start a snapshot and return,
// with hourly idempotency unless force=true. Tagged Manual.
//
// It returns as soon as the snapshot has started. A snapshot of a 40 GB
// store takes about 14 minutes, longer than the CLI's 5-minute tool
// timeout and than the tool-call timeouts of most MCP clients, so a
// blocking call reported a timeout while the daemon carried on and
// finished the backup unobserved (19 Oct 2026). Progress and the
// outcome are in op=backup_status; the outcome is also logged as
// "backup_now: snapshot complete" or "backup_now: snapshot failed".
func (h *callHandler) backupNow(args map[string]any) (string, bool, error) {
	force := false
	if v, ok := args["force"].(bool); ok {
		force = v
	}

	dir := h.backupDir()
	statusDir := h.statusDir()
	if r, ok := backup.InProgress(); ok {
		return fmt.Sprintf("A backup is already running: %s (started %s ago, %s). "+
			"Poll mnemo_ops op=backup_status.", filepath.Base(r.Path),
			humanAge(time.Since(r.Started).Round(time.Second)), r.Step), false, nil
	}
	if !force {
		list, err := backup.List(dir)
		if err == nil && len(list) > 0 {
			age := time.Since(list[0].Time)
			if age < time.Hour {
				return fmt.Sprintf("Recent backup exists (%s ago, %s). Pass force=true to take another.",
					humanAge(age.Round(time.Second)), list[0].Name), false, nil
			}
		}
	}

	src := h.mem.DBPath()

	// Ensure dir exists; CreateAndRetain does this too, but failing here,
	// synchronously, tells the caller why. A destination that is away is
	// recorded as a skip and nothing is written anywhere else.
	if err := ensureDir(dir); err != nil {
		uerr := fmt.Errorf("%w: %s (%v)", backup.ErrDestinationUnavailable, dir, err)
		slog.Error("backup_now: snapshot skipped", "err", uerr)
		backup.RecordUnavailable(statusDir, backup.TagManual, uerr)
		return fmt.Sprintf("backup_now: not started: %v. Reconnect the volume and run it again.", uerr), true, nil
	}
	// Refuse a doomed run here, synchronously, so the caller sees why.
	// BackupWith checks again (and records the skip) for every path.
	if need, err := backup.RequiredSpace(src); err == nil {
		if free, ferr := backup.FreeBytes(dir); ferr == nil && free < need {
			msg := fmt.Sprintf("backup_now: not started: %s free in %s, a snapshot needs %s%s",
				humanSize(free), dir, humanSize(need), backup.HeldBy(dir))
			slog.Error("backup_now: snapshot skipped", "err", fmt.Errorf("%w: %s", backup.ErrInsufficientSpace, msg))
			_ = backup.RecordAttempt(statusDir, backup.Attempt{At: time.Now(), Tag: backup.TagManual,
				Skipped: true, Error: msg})
			return msg, true, nil
		}
	}

	// CreateAndRetain, not Backup: this path used to snapshot and prune
	// nothing, so repeated manual backups grew the directory without
	// bound while the daily worker's retention looked healthy (🎯T158).
	keep := backupKeep()
	if !manualRunning.CompareAndSwap(false, true) {
		return "A backup_now snapshot is already starting. Poll mnemo_ops op=backup_status.", false, nil
	}
	go func() {
		defer manualRunning.Store(false)
		res, removed, err := backupCreate(src, dir, backup.TagManual, keep,
			&backup.BackupArgs{StatusDir: statusDir})
		backup.LogResult("backup_now", res, removed, err)
	}()
	return fmt.Sprintf("Backup started in %s (keep_manual=%d: manual snapshots are pruned only "+
		"by newer manual ones). A snapshot of this store takes several minutes; poll mnemo_ops "+
		"op=backup_status for progress and the result.", dir, keep.KeepManual), false, nil
}

// manualRunning keeps two backup_now calls from racing to start.
var manualRunning atomic.Bool

// backupCreate is backup.CreateAndRetain; a var so tests can stub it.
var backupCreate = backup.CreateAndRetain

// ensureDir creates dir with 0755 if missing. Returns nil on success or
// if dir already exists.
func ensureDir(dir string) error {
	return mkdirAll(dir)
}

// mkdirAll is split out so tests can stub it.
var mkdirAll = func(dir string) error {
	return osMkdirAll(dir, 0o755)
}

// humanSize formats bytes as the largest reasonable IEC unit.
func humanSize(n int64) string {
	const (
		KiB = 1 << 10
		MiB = 1 << 20
		GiB = 1 << 30
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/GiB)
	case n >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/MiB)
	case n >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/KiB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// humanAge formats a Duration as the largest non-zero unit (days,
// hours, minutes). Negative durations are flipped (in case a backup
// somehow timestamps the future).
func humanAge(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d >= 24*time.Hour:
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// backupProgress renders the running snapshot and the last recorded
// attempt, the two things a caller of backup_now polls for. statusDir is
// where the record lives (callHandler.statusDir). After an attempt that
// did not produce a snapshot it also says when the last one did, which
// is the answer still wanted while the destination is away.
func backupProgress(statusDir string) string {
	var b strings.Builder
	if r, ok := backup.InProgress(); ok {
		fmt.Fprintf(&b, "In progress: %s, started %s ago, step: %s\n",
			filepath.Base(r.Path), humanAge(time.Since(r.Started).Round(time.Second)), r.Step)
	}
	if a, ok, err := backup.LastAttempt(statusDir); err == nil && ok {
		ago := humanAge(time.Since(a.At).Round(time.Second))
		switch {
		case a.OK:
			fmt.Fprintf(&b, "Last attempt: complete %s ago, %s (raw %s, compressed %s, %s)\n",
				ago, filepath.Base(a.Path), humanSize(a.RawSize), humanSize(a.CompressedSize),
				a.Elapsed.Round(time.Second))
		case a.Skipped:
			fmt.Fprintf(&b, "Last attempt: SKIPPED %s ago: %s\n", ago, a.Error)
		default:
			fmt.Fprintf(&b, "Last attempt: FAILED %s ago: %s\n", ago, a.Error)
		}
		if !a.OK {
			if t := a.SuccessAt(); t.IsZero() {
				b.WriteString("Last success: none recorded\n")
			} else {
				fmt.Fprintf(&b, "Last success: %s ago\n", humanAge(time.Since(t).Round(time.Second)))
			}
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String()
}
