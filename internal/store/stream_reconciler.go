// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"
)

// StreamReconciler converges one derived stream toward its desired
// state (🎯T68.7 capstone). Implementations must be idempotent —
// calling Reconcile when nothing has diverged is a cheap no-op.
//
// The current shape covers the periodic reconcilers extracted from
// 🎯T68.4–🎯T68.6:
//   - mirror reconcile (CI / GitHub / commits cursor sweep)
//   - source-state reconcile (Law-2 valid-time tag sweep)
//
// Event-driven streams (fsnotify ingest), on-demand tools (vault GC),
// and per-stream cursors fit the same shape as the abstraction grows —
// see docs/design/convergence-data-plane.md.
type StreamReconciler interface {
	// Name uniquely identifies the stream for logging and observability.
	Name() string
	// Interval is the target reconcile cadence. The scheduler may run
	// more often (catch-up) or less often (load) than this hint.
	Interval() time.Duration
	// Reconcile drives one pass toward the fixed point. Returns the
	// number of changes applied this pass (zero on quiescence).
	Reconcile(ctx context.Context, now time.Time) (changed int, err error)
}

// StreamReconcilers returns the periodic-stream reconcilers a worker
// should drive on each tick (🎯T68.7). Adding a new periodic stream is
// one entry in this slice; the registry worker stays the same.
func (s *Store) StreamReconcilers() []StreamReconciler {
	return []StreamReconciler{
		mirrorReconcilerStream{s},
		sourceStateReconcilerStream{s},
	}
}

type mirrorReconcilerStream struct{ s *Store }

func (m mirrorReconcilerStream) Name() string            { return "mirror" }
func (m mirrorReconcilerStream) Interval() time.Duration { return time.Minute }
func (m mirrorReconcilerStream) Reconcile(_ context.Context, now time.Time) (int, error) {
	return m.s.ReconcileStaleMirrors(now)
}

type sourceStateReconcilerStream struct{ s *Store }

func (s sourceStateReconcilerStream) Name() string            { return "source_state" }
func (s sourceStateReconcilerStream) Interval() time.Duration { return time.Minute }
func (s sourceStateReconcilerStream) Reconcile(_ context.Context, now time.Time) (int, error) {
	return s.s.ReconcileSourceState(now)
}
