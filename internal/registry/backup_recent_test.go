// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/mnemo/internal/diag"
)

func TestBackupRecentCheck(t *testing.T) {
	now := time.Now()
	snap := func(dir string, at time.Time) {
		p := filepath.Join(dir, backup.Filename(backup.TagDaily, at))
		if err := os.WriteFile(p, []byte("generic"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		setup func(dir string)
		sev   diag.Severity
		want  string
	}{
		{"none", func(string) {}, diag.Warn, "no successful backup recorded"},
		{"stale", func(d string) {
			at := now.Add(-40 * time.Hour)
			backup.RecordAttempt(d, backup.Attempt{At: at, Tag: backup.TagDaily, OK: true, LastDailySuccess: at})
		}, diag.Warn, "threshold"},
		{"failed", func(d string) {
			backup.RecordAttempt(d, backup.Attempt{At: now.Add(-time.Hour), Error: "write: no space left on device"})
		}, diag.Warn, "last backup attempt failed"},
		{"insufficient space", func(d string) {
			snap(d, now.Add(-20*time.Hour))
			backup.RecordAttempt(d, backup.Attempt{At: now.Add(-time.Minute), Skipped: true,
				Error: "insufficient free space for backup: 2.0 GiB free, need 70.0 GiB"})
		}, diag.Warn, "was skipped"},
		{"fresh", func(d string) {
			at := now.Add(-2 * time.Hour)
			snap(d, at)
			backup.RecordAttempt(d, backup.Attempt{At: at, Tag: backup.TagDaily, OK: true, LastDailySuccess: at})
		}, diag.OK, "last successful backup"},
		{"offloaded", func(d string) {
			at := now.Add(-2 * time.Hour)
			backup.RecordAttempt(d, backup.Attempt{At: at, Tag: backup.TagDaily, OK: true, LastDailySuccess: at})
		}, diag.OK, "last successful backup"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(dir)
			res := backupRecent(dir, dir, now)
			if res.Severity != c.sev || !strings.Contains(res.Detail, c.want) {
				t.Fatalf("severity=%v detail=%q, want %v containing %q", res.Severity, res.Detail, c.sev, c.want)
			}
		})
	}
}

// 🎯T199: a destination that is away is not "no backups exist". The
// record of the last attempt lives in statusDir, beside the database, so
// the verdict survives the volume being unplugged.
func TestBackupRecentDestinationAway(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		setup func(statusDir, dir string)
		sev   diag.Severity
		want  []string
		not   []string
	}{
		// Last night's snapshot succeeded, then the volume was unplugged:
		// the store is protected and there is nothing to do.
		{"fresh success", func(sd, _ string) {
			backup.RecordAttempt(sd, backup.Attempt{At: now.Add(-5 * time.Hour), Tag: backup.TagDaily, OK: true})
		}, diag.OK, []string{"last successful backup", "backup destination not available"}, nil},
		// The window found the volume missing.
		{"skipped night", func(sd, d string) {
			backup.RecordAttempt(sd, backup.Attempt{At: now.Add(-29 * time.Hour), Tag: backup.TagDaily, OK: true})
			backup.RecordUnavailable(sd, backup.TagDaily, errors.New("backup destination not available: "+d))
		}, diag.Warn, []string{"backup destination not available", "was skipped", "last successful backup"},
			[]string{"no backups exist", "no successful backup recorded"}},
		{"stale success", func(sd, _ string) {
			backup.RecordAttempt(sd, backup.Attempt{At: now.Add(-50 * time.Hour), Tag: backup.TagDaily, OK: true})
		}, diag.Warn, []string{"backup destination not available", "threshold"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			statusDir := t.TempDir()
			volume := filepath.Join(t.TempDir(), "volume")
			if err := os.WriteFile(volume, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(volume, "mnemo", "backups")
			c.setup(statusDir, dir)
			res := backupRecent(dir, statusDir, now)
			if res.Severity != c.sev {
				t.Fatalf("severity=%v detail=%q, want %v", res.Severity, res.Detail, c.sev)
			}
			for _, w := range c.want {
				if !strings.Contains(res.Detail, w) {
					t.Errorf("detail %q lacks %q", res.Detail, w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(res.Detail, n) {
					t.Errorf("detail %q claims %q", res.Detail, n)
				}
			}
			if c.sev == diag.Warn && !strings.Contains(res.Remediation, "reconnect") {
				t.Errorf("remediation %q does not say to reconnect the volume", res.Remediation)
			}
		})
	}

	// A custom destination that is present reads the record from
	// statusDir, not from beside the snapshots.
	statusDir, dir := t.TempDir(), t.TempDir()
	backup.RecordAttempt(statusDir, backup.Attempt{At: now.Add(-2 * time.Hour), Tag: backup.TagDaily, OK: true})
	if res := backupRecent(dir, statusDir, now); res.Severity != diag.OK {
		t.Errorf("custom destination present: severity=%v detail=%q, want OK", res.Severity, res.Detail)
	}
}
