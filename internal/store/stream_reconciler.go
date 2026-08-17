// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"log/slog"
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
	// Order is not load-bearing: DriveStreamReconcilers runs each pass
	// with its own deadline in parallel so one slow stream cannot starve
	// the rest (🎯T145). Keep a stable, readable order for logs/doctor.
	return []StreamReconciler{
		mirrorReconcilerStream{s},
		sourceStateReconcilerStream{s},
		patternsReconcilerStream{s},
		calibrationReconcilerStream{s},
		clusterReconcilerStream{s},
	}
}

// calibrationReconcilerStream refreshes the per-corpus score
// distributions that cross-corpus ranking depends on (🎯T144).
//
// It runs on the same divergence-driven shape as the other streams: a
// corpus is recalibrated only when its stored distribution has aged out
// or its document count has moved enough to change its score profile.
// That second condition is the one that matters — corpora here grow
// continuously, and a distribution sampled when a corpus was a fraction
// of its current size mis-maps every score computed against it while
// producing an ordering that looks entirely reasonable.
type calibrationReconcilerStream struct{ s *Store }

func (c calibrationReconcilerStream) Name() string { return "search_calibration" }

// Interval is the tick cadence, not the recalibration cadence: each
// pass recalibrates only the corpora that have actually diverged.
func (c calibrationReconcilerStream) Interval() time.Duration { return time.Hour }

// PassTimeout bounds one calibration tick (🎯T145); corpora loop checks ctx.
func (c calibrationReconcilerStream) PassTimeout() time.Duration { return 10 * time.Minute }

func (c calibrationReconcilerStream) Reconcile(ctx context.Context, now time.Time) (int, error) {
	cals, err := c.s.LoadCalibrations()
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, spec := range searchCorpora() {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		docs := 0
		//nolint:gosec // table name comes from the internal corpus registry
		if err := c.s.readDB.QueryRow(`SELECT COUNT(*) FROM ` + spec.source).Scan(&docs); err != nil || docs == 0 {
			continue
		}
		if stale, _ := cals[spec.kind].Stale(now, docs); !stale {
			continue
		}
		if _, err := c.s.CalibrateCorpus(ctx, spec, now); err != nil {
			// A corpus too small to yield a distribution is a normal
			// state, not a fault: search degrades to fusion for it, and
			// saying so every minute would be noise.
			//
			// A corpus with ample documents that still fails to calibrate
			// is the opposite — something is wrong and the consequence
			// (ranking silently stuck on the degraded path) is invisible
			// from the outside. That one is worth a line.
			if docs > calibrationMinSamples {
				slog.Warn("calibration failed for a corpus large enough to calibrate; "+
					"its hits will rank by fusion until this clears",
					"corpus", spec.kind, "docs", docs, "err", err)
			} else {
				slog.Debug("calibration skipped: corpus too small",
					"corpus", spec.kind, "docs", docs, "err", err)
			}
			continue
		}
		changed++
	}
	return changed, nil
}

// clusterReconcilerStream runs document-level themes clustering on a
// long cadence (default 24h; 🎯T64.8). The worker ticks more often;
// Reconcile skips when the last successful run is still fresh.
type clusterReconcilerStream struct{ s *Store }

func (c clusterReconcilerStream) Name() string { return "themes_cluster" }

func (c clusterReconcilerStream) Interval() time.Duration {
	return 24 * time.Hour
}

// PassTimeout bounds one clustering pass (🎯T145). Clustering is 24h cadence
// but must not hold a worker without a wall-clock bound.
func (c clusterReconcilerStream) PassTimeout() time.Duration { return 30 * time.Minute }

func (c clusterReconcilerStream) Reconcile(ctx context.Context, now time.Time) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	last, err := c.s.LatestClusterRun()
	if err != nil {
		return 0, err
	}
	// Skip while a prior run is unfinished (ended_at empty). Orphan rows
	// from dead daemons are closed at startup by ResolveOrphanClusterRuns
	// so this is not permanent starvation (🎯T145).
	if last != nil && last.EndedAt == "" {
		return 0, nil
	}
	if last != nil && last.FailureMode == "" {
		if t, perr := time.Parse(time.RFC3339, last.EndedAt); perr == nil {
			if now.Sub(t) < c.Interval() {
				return 0, nil
			}
		}
	}
	res, err := c.s.RunCluster(ctx, ClusterRunArgs{
		Trigger: "interval",
		Now:     now,
	})
	if err != nil {
		return 0, err
	}
	return res.OutputThemes, nil
}

type mirrorReconcilerStream struct{ s *Store }

func (m mirrorReconcilerStream) Name() string            { return "mirror" }
func (m mirrorReconcilerStream) Interval() time.Duration { return time.Minute }
func (m mirrorReconcilerStream) PassTimeout() time.Duration {
	return 45 * time.Second
}
func (m mirrorReconcilerStream) Reconcile(ctx context.Context, now time.Time) (int, error) {
	return m.s.ReconcileStaleMirrors(ctx, now)
}

type sourceStateReconcilerStream struct{ s *Store }

func (s sourceStateReconcilerStream) Name() string            { return "source_state" }
func (s sourceStateReconcilerStream) Interval() time.Duration { return time.Minute }
func (s sourceStateReconcilerStream) PassTimeout() time.Duration {
	return 45 * time.Second
}
func (s sourceStateReconcilerStream) Reconcile(_ context.Context, now time.Time) (int, error) {
	return s.s.ReconcileSourceState(now)
}
