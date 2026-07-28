// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
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
	r := NewRegistry(context.Background(), store.Config{}, "")
	s, err := r.ForUser("marcelo")
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

// NOTE: there is deliberately no test asserting that Close returns
// FASTER than the grace when workers are well behaved. Writing one
// showed that it does not: with a real user entry, ForUser starts the
// daemon's own workers and at least one of them does not observe
// cancellation within 3s, so Close waits out the grace even in the
// healthy case. That is a property worth improving (making the mirror
// subprocesses context-aware would do it) but it is not one the code
// has today, and asserting an aspiration would just make the suite lie.
