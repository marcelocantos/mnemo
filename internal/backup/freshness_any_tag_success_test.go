// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"strings"
	"testing"
	"time"
)

// backup.recent must treat a successful snapshot of any tag (daily,
// manual backup_now, pre-migration) as the last success time. The
// warning's remediation is `mnemo ops backup_now`; a successful manual
// run must therefore clear a stale or failed night. last_daily_success
// stays daily-only for the worker window.
//
// Written against Freshness / RecordAttempt / Attempt as they exist at
// 8643982 so the cases compile there and fail as runtime assertions.

func TestFreshnessUsesAnyTagSuccess(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	staleDaily := now.Add(-40 * time.Hour)
	manualAt := now.Add(-2 * time.Hour)

	t.Run("stale daily then successful manual is ok", func(t *testing.T) {
		last := &Attempt{
			At:               manualAt,
			Tag:              TagManual,
			OK:               true,
			LastDailySuccess: staleDaily,
		}
		healthy, detail, _ := Freshness(nil, last, now, MaxAge)
		if !healthy {
			t.Fatalf("stale daily then successful backup_now: healthy=false detail=%q, want ok", detail)
		}
	})

	t.Run("failed daily then successful manual is ok", func(t *testing.T) {
		dir := t.TempDir()
		if err := RecordAttempt(dir, Attempt{
			At: now.Add(-3 * time.Hour), Tag: TagDaily, Error: "write: no space left on device",
		}); err != nil {
			t.Fatal(err)
		}
		if err := RecordAttempt(dir, Attempt{
			At: manualAt, Tag: TagManual, OK: true,
		}); err != nil {
			t.Fatal(err)
		}
		a, ok, err := LastAttempt(dir)
		if err != nil || !ok {
			t.Fatalf("LastAttempt ok=%v err=%v", ok, err)
		}
		healthy, detail, _ := Freshness(nil, &a, now, MaxAge)
		if !healthy {
			t.Fatalf("failed daily then successful backup_now: healthy=false detail=%q, want ok", detail)
		}
	})

	t.Run("successful manual then failed attempt still warns", func(t *testing.T) {
		dir := t.TempDir()
		if err := RecordAttempt(dir, Attempt{
			At: manualAt, Tag: TagManual, OK: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := RecordAttempt(dir, Attempt{
			At: now.Add(-time.Hour), Tag: TagDaily, Error: "write: no space left on device",
		}); err != nil {
			t.Fatal(err)
		}
		a, ok, err := LastAttempt(dir)
		if err != nil || !ok {
			t.Fatalf("LastAttempt ok=%v err=%v", ok, err)
		}
		healthy, detail, _ := Freshness(nil, &a, now, MaxAge)
		if healthy {
			t.Fatalf("successful backup_now then failed attempt: healthy=true detail=%q, want warn", detail)
		}
		if !strings.Contains(detail, "failed") {
			t.Fatalf("failed attempt detail=%q, want it to say failed", detail)
		}
		if strings.Contains(detail, "no successful daily backup recorded") {
			t.Fatalf("failed attempt after a successful manual must not claim no success: %q", detail)
		}
		if !strings.Contains(detail, "last successful backup") {
			t.Fatalf("failed attempt after a successful manual should still name that success: %q", detail)
		}
	})
}
