// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
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
			res := backupRecent(dir, now)
			if res.Severity != c.sev || !strings.Contains(res.Detail, c.want) {
				t.Fatalf("severity=%v detail=%q, want %v containing %q", res.Severity, res.Detail, c.sev, c.want)
			}
		})
	}
}
