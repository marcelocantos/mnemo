// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package reviewer

import (
	"context"
	"log/slog"
	"time"
)

// DefaultTickInterval is how often the worker scans the repo set.
// Cheap-signal gating (entry counts) means most ticks do no LLM work,
// so a frequent tick is fine. 10 minutes is a good balance: a high-
// activity repo crosses the high-entry threshold within one tick of
// its actual crossing, but the worker isn't burning CPU constantly.
const DefaultTickInterval = 10 * time.Minute

// Run starts the review worker loop. Returns when ctx is cancelled.
// Errors from individual ticks are logged but never terminate the
// loop — a bad LLM response or a transient DB hiccup must not stop
// reviews from happening on subsequent ticks.
//
// The first tick fires immediately (after a short startup delay so
// it doesn't compete with daemon initialisation), then every
// DefaultTickInterval.
func Run(ctx context.Context, r *Reviewer) {
	// Startup delay — let ingest backfill settle so the first tick
	// has accurate entry counts.
	select {
	case <-ctx.Done():
		return
	case <-time.After(60 * time.Second):
	}

	slog.Info("review worker started", "interval", DefaultTickInterval)
	r.Tick(ctx)

	t := time.NewTicker(DefaultTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("review worker stopping")
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}
