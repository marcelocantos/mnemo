// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/boot"
	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
)

// The health pass that runs when startup finishes lands just before the
// watcher's goroutine starts. Reporting that as "not running" produced a
// warning after every restart on a daemon whose watcher was fine.
func TestWatcherNotYetStartedIsStartingJustAfterReady(t *testing.T) {
	now := time.Date(2026, 9, 21, 18, 7, 22, 0, time.UTC)
	ready := boot.Status{Phase: boot.PhaseReady, Since: now.Add(-time.Second)}

	r, starting := watcherStarting(store.WatchTelemetry{}, ready, now)
	if !starting || r.Severity != diag.OK {
		t.Fatalf("never-started watcher 1s after ready: starting=%v severity=%v, want healthy",
			starting, r.Severity)
	}
}

// The grace must not hide a watcher that never starts.
func TestWatcherThatNeverStartsStillWarnsAfterGrace(t *testing.T) {
	now := time.Date(2026, 9, 21, 18, 7, 22, 0, time.UTC)
	ready := boot.Status{Phase: boot.PhaseReady, Since: now.Add(-watcherStartGrace)}
	if _, starting := watcherStarting(store.WatchTelemetry{}, ready, now); starting {
		t.Fatal("excused a watcher that has not started a full grace period after ready")
	}
}

// A watcher that ran and stopped is a real failure, grace or not.
func TestWatcherThatStoppedIsNotExcused(t *testing.T) {
	now := time.Date(2026, 9, 21, 18, 7, 22, 0, time.UTC)
	ready := boot.Status{Phase: boot.PhaseReady, Since: now.Add(-time.Second)}
	stopped := store.WatchTelemetry{Running: false, StartedAt: now.Add(-time.Hour)}
	if _, starting := watcherStarting(stopped, ready, now); starting {
		t.Fatal("excused a watcher that had run and stopped")
	}
}

// Before ready, the store-not-ready path owns the answer; this helper
// must not claim it.
func TestWatcherStartingOnlyAppliesOnceReady(t *testing.T) {
	now := time.Date(2026, 9, 21, 18, 7, 22, 0, time.UTC)
	opening := boot.Status{Phase: boot.PhaseOpeningStore, Since: now.Add(-time.Second)}
	if _, starting := watcherStarting(store.WatchTelemetry{}, opening, now); starting {
		t.Fatal("claimed the watcher was starting before the daemon was ready")
	}
}
