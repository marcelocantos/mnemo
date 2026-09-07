// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestWALStuckDetail pins the escalation boundary for 🎯T172.
//
// The bar for a fail here is deliberately high: it fires an OS
// notification, so a false positive is a user staring at an alert with
// nothing wrong and nothing to do. Most of these cases therefore assert
// that the check stays QUIET — the healthy-but-alarming shapes are the
// ones worth defending against, not the fault.
func TestWALStuckDetail(t *testing.T) {
	const big = 512 << 20
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	for _, tc := range []struct {
		name        string
		attempt     time.Time // last checkpoint attempted
		stuckSince  time.Time // when checkpoints stopped copying frames (zero = last one advanced)
		wantStuck   bool
		wantBecause string
	}{
		{
			name:        "no checkpoint ever attempted",
			wantBecause: "absence of attempts says nothing about whether checkpoints could advance",
		},
		{
			// The case this whole target exists to NOT fire on: the
			// 7 Sep backfill grew the WAL to 1,016 MiB over a sustained
			// period and was entirely healthy.
			name:        "busy backfill, checkpoints still advancing",
			attempt:     ago(30 * time.Second),
			stuckSince:  time.Time{},
			wantBecause: "frames are still being copied, so no reader is pinning the log",
		},
		{
			name:        "long reader mid-run, well inside the window",
			attempt:     ago(time.Minute),
			stuckSince:  ago(10 * time.Minute), // a backup VACUUM INTO takes 5-11 min
			wantBecause: "the daemon's own longest job has not yet exceeded the window",
		},
		{
			name:        "just inside the window",
			attempt:     ago(time.Minute),
			stuckSince:  ago(walStuckWindow - time.Minute),
			wantBecause: "the window has not elapsed",
		},
		{
			name:       "past the window with attempts ongoing",
			attempt:    ago(time.Minute),
			stuckSince: ago(walStuckWindow + time.Minute),
			wantStuck:  true,
		},
		{
			// A daemon that stopped trying is a different fault, and
			// this check must not claim it as a pinned reader.
			name:        "no advance for hours but attempts also stopped",
			attempt:     ago(3 * time.Hour),
			stuckSince:  ago(4 * time.Hour),
			wantBecause: "attempts stopped too, so the checkpointer is stalled, not blocked",
		},
		{
			name:        "never advanced, but only just started trying",
			attempt:     ago(time.Minute),
			stuckSince:  ago(time.Minute),
			wantBecause: "a freshly started daemon has no history to judge",
		},
		{
			name:       "never advanced since the daemon started, past the window",
			attempt:    ago(time.Minute),
			stuckSince: ago(walStuckWindow + 5*time.Minute),
			wantStuck:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newWALProgressStore(t, tc.attempt, tc.stuckSince)
			detail, stuck := walStuckDetail(s, big, now)
			if stuck != tc.wantStuck {
				t.Fatalf("stuck = %v, want %v — %s", stuck, tc.wantStuck, tc.wantBecause)
			}
			if stuck && detail == "" {
				t.Error("a fail must carry detail; it becomes an OS notification")
			}
			if !stuck && detail != "" {
				t.Errorf("no fail should carry no detail, got %q", detail)
			}
		})
	}
}

// TestWALStuckWindowExceedsKnownReaders guards the premise the low
// false-positive rate rests on: the window must be comfortably longer
// than any reader the daemon opens itself. wal.go names the backup's
// VACUUM INTO at 5-11 minutes as the worst offender.
func TestWALStuckWindowExceedsKnownReaders(t *testing.T) {
	const longestKnownReader = 11 * time.Minute
	if walStuckWindow < 3*longestKnownReader {
		t.Errorf("walStuckWindow = %s, want at least 3x the longest known reader (%s); "+
			"a window close to a legitimate reader's runtime is how this starts crying wolf",
			walStuckWindow, longestKnownReader)
	}
}

func newWALProgressStore(t *testing.T, attempt, stuckSince time.Time) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetWALCheckpointProgressForTest(attempt, stuckSince)
	return s
}
