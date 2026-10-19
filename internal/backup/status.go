// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatusFile records the outcome of the most recent snapshot attempt in
// the backup directory. It lives on disk, not in memory, because the
// failure it reports usually outlives the process: on 19 Oct 2026 the
// nightly run failed on a full disk, and /health went on reporting the
// backup directory as healthy with zero snapshots in it.
const StatusFile = ".last-attempt.json"

// Attempt is one snapshot attempt, successful, failed or skipped.
type Attempt struct {
	At             time.Time     `json:"at"`
	Tag            Tag           `json:"tag,omitempty"`
	Path           string        `json:"path,omitempty"`
	OK             bool          `json:"ok"`
	Skipped        bool          `json:"skipped,omitempty"` // precheck refused to start
	Error          string        `json:"error,omitempty"`
	RawSize        int64         `json:"raw_size,omitempty"`
	CompressedSize int64         `json:"compressed_size,omitempty"`
	Elapsed        time.Duration `json:"elapsed,omitempty"`
}

// RecordAttempt writes a to dir/StatusFile atomically.
func RecordAttempt(dir string, a Attempt) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, StatusFile+".new")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, StatusFile))
}

// LastAttempt reads dir/StatusFile. ok is false when none was recorded.
func LastAttempt(dir string) (a Attempt, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if errors.Is(err, os.ErrNotExist) {
		return Attempt{}, false, nil
	}
	if err != nil {
		return Attempt{}, false, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return Attempt{}, false, fmt.Errorf("parse %s: %w", StatusFile, err)
	}
	return a, true, nil
}

// LogResult logs a finished snapshot attempt. Every path that takes a
// snapshot (the nightly worker, mnemo_ops op=backup_now) logs through it,
// so "snapshot complete" and "snapshot failed" are greppable regardless
// of what triggered the run. source prefixes the message.
func LogResult(source string, res Result, removed []string, err error) {
	if err != nil {
		slog.Error(source+": snapshot failed", "err", err)
		return
	}
	slog.Info(source+": snapshot complete",
		"path", res.Path,
		"raw_mb", res.RawSize/(1<<20),
		"compressed_mb", res.CompressedSize/(1<<20),
		"elapsed", res.Elapsed.Round(time.Second),
		"retention_removed", len(removed))
}

// Running describes a snapshot in progress in this process.
type Running struct {
	Path    string
	Started time.Time
	Step    string
}

var (
	runMu  sync.Mutex
	runNow *Running
)

// InProgress reports the snapshot this process is taking, if any.
func InProgress() (Running, bool) {
	runMu.Lock()
	defer runMu.Unlock()
	if runNow == nil {
		return Running{}, false
	}
	return *runNow, true
}

func setRunning(r *Running) {
	runMu.Lock()
	runNow = r
	runMu.Unlock()
}

func setRunningStep(step string) {
	runMu.Lock()
	if runNow != nil {
		runNow.Step = step
	}
	runMu.Unlock()
}

// MaxAge is how old the newest snapshot may be before /health warns.
//
// The nightly worker picks a random instant in the 03:00-04:00 window and
// then waits up to an hour for a quiet moment, and a snapshot of a ~40 GB
// store takes 10-15 minutes. The widest legitimate gap between two
// successful nightly snapshots is therefore 03:00 one day to about 05:15
// the next, 26h15m. 30h leaves under four hours for a slow disk or a
// sleeping machine; past that, a night was missed, which is the failure
// this check exists to report.
const MaxAge = 30 * time.Hour

// Freshness judges whether the backups in a directory protect the store.
// list is newest first (List's order); last is the recorded attempt.
func Freshness(list []Info, last *Attempt, now time.Time, maxAge time.Duration) (healthy bool, detail, remediation string) {
	if last != nil && !last.OK && (len(list) == 0 || last.At.After(list[0].Time)) {
		what := "failed"
		if last.Skipped {
			what = "was skipped"
		}
		detail = fmt.Sprintf("last backup attempt %s %s ago: %s", what,
			now.Sub(last.At).Round(time.Minute), last.Error)
		if len(list) == 0 {
			detail += "; no backups exist"
		} else {
			detail += fmt.Sprintf("; newest backup %s is %s old", list[0].Name,
				now.Sub(list[0].Time).Round(time.Minute))
		}
		return false, detail, "free space on the backup volume if that was the cause, then run " +
			"`mnemo ops backup_now` and watch `mnemo ops backup_status`"
	}
	if len(list) == 0 {
		return false, "no backups exist", "run `mnemo ops backup_now`; check that backups are " +
			"enabled in config.json and the daemon log for 'backup worker'"
	}
	age := now.Sub(list[0].Time)
	if age > maxAge {
		return false, fmt.Sprintf("newest backup %s is %s old (threshold %s)", list[0].Name,
				age.Round(time.Minute), maxAge),
			"the nightly worker has missed at least one run: check the daemon log for " +
				"'backup worker', then run `mnemo ops backup_now`"
	}
	return true, fmt.Sprintf("newest backup %s is %s old", list[0].Name, age.Round(time.Minute)), ""
}
