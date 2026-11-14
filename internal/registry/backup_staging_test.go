// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/mnemo/internal/diag"
)

// 🎯T201: backup.recent distinguishes written to the destination, staged
// and waiting (healthy while fresh), and staged too long (warns).
func TestBackupRecentStagingStates(t *testing.T) {
	now := time.Now()
	setup := func(t *testing.T, waiting time.Duration, staged bool) (dir, statusDir, staging string) {
		t.Helper()
		statusDir = t.TempDir()
		staging = filepath.Join(statusDir, backup.StagingSubdir)
		volume := filepath.Join(t.TempDir(), "volume")
		if err := os.WriteFile(volume, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		dir = filepath.Join(volume, "backups") // away
		at := now.Add(-2 * time.Hour)
		if err := backup.RecordAttempt(statusDir, backup.Attempt{At: at, Tag: backup.TagDaily, OK: true,
			Staged: staged, StagedBecause: "backup destination not available"}); err != nil {
			t.Fatal(err)
		}
		if staged {
			if err := os.MkdirAll(staging, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(staging, backup.Filename(backup.TagDaily, at)), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(map[string]any{"waiting_since": now.Add(-waiting)})
			if err := os.WriteFile(filepath.Join(staging, ".staging.json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir, statusDir, staging
	}

	t.Run("nothing staged", func(t *testing.T) {
		dir, statusDir, staging := setup(t, 0, false)
		res := backupRecentStaged(dir, statusDir, staging, now)
		if res.Severity != diag.OK || strings.Contains(res.Detail, "staged on the database volume") {
			t.Fatalf("%v %q", res.Severity, res.Detail)
		}
	})
	t.Run("staged and waiting", func(t *testing.T) {
		dir, statusDir, staging := setup(t, 2*time.Hour, true)
		res := backupRecentStaged(dir, statusDir, staging, now)
		if res.Severity != diag.OK || !strings.Contains(res.Detail, "staged on the database volume") {
			t.Fatalf("%v %q, want healthy naming the staged snapshot", res.Severity, res.Detail)
		}
	})
	t.Run("staged too long", func(t *testing.T) {
		dir, statusDir, staging := setup(t, 8*24*time.Hour, true)
		res := backupRecentStaged(dir, statusDir, staging, now)
		if res.Severity != diag.Warn || !strings.Contains(res.Detail, "same disk as the database") {
			t.Fatalf("%v %q, want a warning that the newest backup shares the database's disk", res.Severity, res.Detail)
		}
		if !strings.Contains(res.Remediation, "allow it") {
			t.Errorf("remediation %q does not mention the macOS prompt", res.Remediation)
		}
	})
}
