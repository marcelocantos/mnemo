// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/mnemo/internal/store"
)

// osMkdirAll wraps os.MkdirAll so the mkdirAll var above can shadow it
// in tests without needing build tags.
func osMkdirAll(dir string, mode os.FileMode) error { return os.MkdirAll(dir, mode) }

// backupDir returns the conventional backup directory for the user's
// mnemo.db: ~/.mnemo/backups (or wherever the daemon was pointed). The
// MCP tools don't (yet) read BackupConfig themselves — config-driven
// custom dirs are inspectable via direct filesystem access. v1 punts on
// that to keep the tool surface small.
func (h *callHandler) backupDir() string {
	return filepath.Join(filepath.Dir(h.mem.DBPath()), "backups")
}

// backupKeep resolves the retention count the same way the worker does,
// so a manual backup prunes to the configured depth rather than to a
// number of its own. An unreadable config falls back to the same default
// EffectiveKeepDailies applies.
func backupKeep() int {
	cfg, err := store.LoadConfig()
	if err != nil {
		return store.BackupConfig{}.EffectiveKeepDailies()
	}
	return cfg.Backup.EffectiveKeepDailies()
}

// backupStatus implements mnemo_backup_status: list backups newest-first
// with tag, age, and size. Output is text rather than JSON because it's
// human-consumed (the typical caller is an agent paraphrasing it for the
// user, or the user running it directly).
func (h *callHandler) backupStatus() (string, bool, error) {
	dir := h.backupDir()
	list, err := backup.List(dir)
	if err != nil {
		return fmt.Sprintf("backup status failed: %v", err), true, nil
	}
	if len(list) == 0 {
		return fmt.Sprintf("No backups found in %s.\nThe daemon's daily worker runs in the 03:00-04:00 local window; pre-migration backups fire when sqlift.Apply has a non-empty plan. Use mnemo_backup_now to trigger one manually.", dir), false, nil
	}
	var b strings.Builder
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

// backupNow implements mnemo_backup_now: trigger an immediate backup,
// with hourly idempotency unless force=true. Tagged Manual.
//
// Blocking call (~1-2 min on a multi-GB DB). The MCP framing already
// handles long-running tool calls via JSON-RPC; the caller waits for
// the result.
func (h *callHandler) backupNow(args map[string]any) (string, bool, error) {
	force := false
	if v, ok := args["force"].(bool); ok {
		force = v
	}

	dir := h.backupDir()
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

	// Best-effort ensure dir exists; CreateAndRetain does this too, but
	// failing here gives a clearer message than failing mid-backup.
	if err := ensureDir(dir); err != nil {
		return fmt.Sprintf("backup_now: cannot ensure backup dir %s: %v", dir, err), true, nil
	}

	// CreateAndRetain, not Backup: this path used to snapshot and prune
	// nothing, so repeated manual backups grew the directory without
	// bound while the daily worker's retention looked healthy (🎯T158).
	keep := backupKeep()
	res, removed, err := backup.CreateAndRetain(src, dir, backup.TagManual, keep, nil)
	if err != nil {
		return fmt.Sprintf("backup_now failed: %v", err), true, nil
	}
	out := fmt.Sprintf("Backup written: %s\nRaw: %s | Compressed: %s | Elapsed: %s",
		res.Path,
		humanSize(res.RawSize), humanSize(res.CompressedSize),
		res.Elapsed.Round(time.Second))
	if len(removed) > 0 {
		out += fmt.Sprintf("\nRetention (keep=%d) removed %d older snapshot(s).", keep, len(removed))
	}
	return out, false, nil
}

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
