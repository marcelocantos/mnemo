// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"strings"
	"testing"
	"time"
)

// backup.recent must judge freshness from last_daily_success, not from a
// snapshot file in the backup directory. Nightly snapshots may be
// offloaded, so an empty directory is not "no backups exist".
//
// These cases are written against Freshness's existing signature so they
// compile on 217de19 (which still keys off on-disk files) and fail there
// as runtime assertions.

func TestFreshnessUsesLastDailySuccessNotSnapshotFiles(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	successAt := now.Add(-2 * time.Hour)
	staleAt := now.Add(-40 * time.Hour)

	t.Run("offloaded success is ok", func(t *testing.T) {
		last := &Attempt{
			At:               successAt,
			Tag:              TagDaily,
			OK:               true,
			LastDailySuccess: successAt,
		}
		healthy, detail, _ := Freshness(nil, last, now, MaxAge)
		if !healthy {
			t.Fatalf("success recorded, snapshot offloaded: healthy=false detail=%q, want ok", detail)
		}
		if strings.Contains(detail, "no backups exist") {
			t.Fatalf("offloaded success must not be reported as missing: %q", detail)
		}
	})

	t.Run("stale success warns", func(t *testing.T) {
		last := &Attempt{
			At:               staleAt,
			Tag:              TagDaily,
			OK:               true,
			LastDailySuccess: staleAt,
		}
		healthy, detail, _ := Freshness(nil, last, now, MaxAge)
		if healthy {
			t.Fatalf("stale last_daily_success: healthy=true detail=%q, want warn", detail)
		}
		if !strings.Contains(detail, "threshold") {
			t.Fatalf("stale success detail=%q, want it to name the age threshold", detail)
		}
		if strings.Contains(detail, "no backups exist") {
			t.Fatalf("stale recorded success must not be reported as missing: %q", detail)
		}
	})

	t.Run("failed latest attempt warns", func(t *testing.T) {
		last := &Attempt{
			At:               now.Add(-time.Hour),
			Tag:              TagDaily,
			Error:            "write: no space left on device",
			LastDailySuccess: successAt,
		}
		healthy, detail, _ := Freshness(nil, last, now, MaxAge)
		if healthy {
			t.Fatalf("failed latest attempt: healthy=true detail=%q, want warn", detail)
		}
		if !strings.Contains(detail, "failed") {
			t.Fatalf("failed attempt detail=%q, want it to say failed", detail)
		}
		if strings.Contains(detail, "no backups exist") {
			t.Fatalf("failed attempt with a recorded success must not claim no backups exist: %q", detail)
		}
	})

	t.Run("skipped latest attempt warns", func(t *testing.T) {
		last := &Attempt{
			At:               now.Add(-time.Minute),
			Tag:              TagDaily,
			Skipped:          true,
			Error:            "insufficient free space for backup: 2.0 GiB free, need 70.0 GiB",
			LastDailySuccess: successAt,
		}
		healthy, detail, _ := Freshness(nil, last, now, MaxAge)
		if healthy {
			t.Fatalf("skipped latest attempt: healthy=true detail=%q, want warn", detail)
		}
		if !strings.Contains(detail, "was skipped") {
			t.Fatalf("skipped attempt detail=%q, want it to say skipped", detail)
		}
		if strings.Contains(detail, "no backups exist") {
			t.Fatalf("skipped attempt with a recorded success must not claim no backups exist: %q", detail)
		}
	})
}
