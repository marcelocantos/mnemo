// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestStreamReconcilersRegistered locks in the 🎯T68.7 abstraction:
// the periodic-stream reconcilers extracted from T68.5 (mirror) and
// T68.6 (source_state) are both reachable through StreamReconcilers().
// Adding a new periodic stream is one slice entry; this test catches
// accidental removal.
func TestStreamReconcilersRegistered(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	got := map[string]bool{}
	for _, sr := range s.StreamReconcilers() {
		got[sr.Name()] = true
		if sr.Interval() <= 0 {
			t.Errorf("stream %q reports non-positive interval %v", sr.Name(), sr.Interval())
		}
		// Reconcile must be safe to call on a fresh store (no
		// candidates, no drift) — capstone idempotency.
		if _, err := sr.Reconcile(context.Background(), time.Now().UTC()); err != nil {
			t.Errorf("stream %q reconcile on empty store: %v", sr.Name(), err)
		}
	}
	for _, want := range []string{"mirror", "source_state"} {
		if !got[want] {
			t.Errorf("stream %q not registered; have %v", want, got)
		}
	}
}
