// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"os/user"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestCloseCheckpointsDespiteStuckWorker is the regression guard for the
// half of 🎯T122 that made every shutdown a forced exit.
//
// Registry.Close used to call e.workers.Wait() unconditionally before
// closing the store. Cancellation does not reach every worker — the
// mirror streams shell out to `gh` and `git log` via exec.Command with
// no context, so a subprocess mid-flight runs to completion whatever
// shutdown wants. Because Store.Close is the ONLY caller of the WAL
// checkpoint, blocking there meant the checkpoint never ran: 11 of 11
// shutdowns in one session force-exited and the -wal grew to 2.3 GB.
//
// The stuck goroutine below stands in for that subprocess. Close must
// give up on it and still close the store.
func TestCloseCheckpointsDespiteStuckWorker(t *testing.T) {
	// Isolate the store: ForUser resolves the user's REAL home, so
	// without this the test opens ~/.mnemo/mnemo.db — the live database,
	// alongside a possibly-running daemon — and checkpoints it on Close.
	t.Setenv(store.MnemoHomeEnv, t.TempDir())

	r := NewRegistry(context.Background(), store.Config{}, "")
	s, err := r.ForUser(currentUser(t))
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}

	// A worker that ignores cancellation entirely, like a subprocess
	// already in flight. Released only after Close has returned, so the
	// test proves Close did not wait for it.
	stuck := make(chan struct{})
	r.mu.Lock()
	for _, e := range r.stores {
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			<-stuck
		}()
	}
	r.mu.Unlock()

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		r.Close()
		done <- time.Since(start)
	}()

	// Close must return on the order of workerDrainGrace, not hang until
	// the process is killed. Generous slack for CI scheduling.
	var elapsed time.Duration
	select {
	case elapsed = <-done:
	case <-time.After(workerDrainGrace + 20*time.Second):
		close(stuck)
		t.Fatal("Registry.Close blocked on a worker that will not stop; " +
			"the WAL checkpoint is unreachable")
	}
	close(stuck)

	if elapsed < workerDrainGrace {
		t.Errorf("Close returned in %s, before the %s grace — it is not "+
			"giving cancellable workers their chance to finish", elapsed, workerDrainGrace)
	}

	// The point of giving up: the store actually got closed, which is
	// what runs the checkpoint. A closed store rejects queries.
	if _, err := s.Query("SELECT 1 AS ok"); err == nil {
		t.Error("store still answering after Close; the checkpoint path did not run")
	}
}

// TestClosePromptWhenWorkersCooperate is the test 🎯T123 had to delete,
// reinstated by 🎯T124.
//
// It was removed because it failed: with a real user entry, ForUser
// starts the daemon's own workers and at least one did not observe
// cancellation within the grace, so Close waited the full 3s even with
// nothing wrong. The cause was the mirror streams shelling out via
// exec.Command with no context — a `gh` or `git log` subprocess in
// flight ran to completion whatever shutdown wanted, and the worker
// driving it could not return until it did.
//
// With those subprocesses context-aware, the bounded wait added by
// 🎯T122 becomes a genuine backstop rather than something the healthy
// path relies on. If this test starts failing again, a worker has
// stopped observing cancellation — that is the regression to hunt, not
// a reason to widen the bound.
func TestClosePromptWhenWorkersCooperate(t *testing.T) {
	t.Setenv(store.MnemoHomeEnv, t.TempDir())

	r := NewRegistry(context.Background(), store.Config{}, "")
	if _, err := r.ForUser(currentUser(t)); err != nil {
		t.Fatalf("ForUser: %v", err)
	}

	start := time.Now()
	r.Close()
	elapsed := time.Since(start)

	if elapsed >= workerDrainGrace {
		t.Errorf("Close took %s, reaching the %s grace with no stuck worker — "+
			"some worker is not observing cancellation, so shutdown is "+
			"abandoning it rather than draining it", elapsed, workerDrainGrace)
	}
}

// currentUser returns a username the registry can resolve a home
// directory for. Hardcoding one works on a dev machine and fails in CI,
// where that account does not exist.
func currentUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve the current user: %v", err)
	}
	return u.Username
}
