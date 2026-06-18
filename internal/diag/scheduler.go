// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"context"
	"log/slog"
	"time"
)

// Default scheduler cadences (🎯T83). Fast checks run often; the full
// suite (env re-validation, convergence) runs at startup, on the full
// interval, and on demand via mnemo_doctor.
const (
	DefaultFastInterval = 3 * time.Minute
	DefaultFullInterval = time.Hour
)

// Scheduler drives a diagnostics Registry on a cadence and feeds each
// report to a Notifier. The full suite runs once at startup, the Fast
// tier runs every FastInterval, and the full suite re-runs every
// FullInterval. The /health endpoint and mnemo_doctor call the Registry
// directly, so the scheduler exists purely to keep the timed checks (and
// thus notifications) flowing.
type Scheduler struct {
	reg      *Registry
	notifier *Notifier
	fast     time.Duration
	full     time.Duration
	now      func() time.Time
}

// NewScheduler builds a scheduler. A zero interval uses the default;
// notifier may be nil to run checks without notifications.
func NewScheduler(reg *Registry, notifier *Notifier, fast, full time.Duration) *Scheduler {
	if fast <= 0 {
		fast = DefaultFastInterval
	}
	if full <= 0 {
		full = DefaultFullInterval
	}
	return &Scheduler{reg: reg, notifier: notifier, fast: fast, full: full, now: time.Now}
}

// Run executes the full suite once, then loops until ctx is cancelled,
// running the Fast tier each FastInterval and the full suite each
// FullInterval. Blocks; start it in a goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	lastFull := s.now()
	s.runOnce(ctx, true) // startup: full validation

	t := time.NewTicker(s.fast)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := s.now()
			full := now.Sub(lastFull) >= s.full
			if full {
				lastFull = now
			}
			s.runOnce(ctx, full)
		}
	}
}

// runOnce runs the checks (full or fast tier), notifies, and logs a
// one-line summary whenever anything is not ok.
func (s *Scheduler) runOnce(ctx context.Context, full bool) {
	rep := s.reg.Run(ctx, full, s.now())
	if s.notifier != nil {
		s.notifier.Observe(rep, s.now())
	}
	if rep.Fail > 0 || rep.Warn > 0 {
		slog.Warn("diag: health degraded",
			"fail", rep.Fail, "warn", rep.Warn, "ok", rep.OK, "tier_full", full)
	}
}
