// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Default scheduler cadences (🎯T83). Fast checks run often; the full
// suite (env re-validation, convergence) runs at startup, on the full
// interval, and on demand via mnemo_ops op=doctor.
const (
	DefaultFastInterval = 3 * time.Minute
	DefaultFullInterval = time.Hour
)

// Scheduler drives a diagnostics Registry on a cadence and feeds each
// report to a Notifier. The full suite runs once at startup, the Fast
// tier runs every FastInterval, and the full suite re-runs every
// FullInterval. The /health endpoint and mnemo_ops op=doctor call the Registry
// directly, so the scheduler exists purely to keep the timed checks (and
// thus notifications) flowing.
type Scheduler struct {
	reg      *Registry
	notifier *Notifier
	fast     time.Duration
	full     time.Duration
	now      func() time.Time
	// onReport, when set, receives every report the scheduler produces, so the
	// live dashboard panel and status glyph can update via the SSE hub (🎯T86).
	onReport   func(Report)
	beforeFull func()
	// snap merges every run, Fast and Full, into one complete report.
	// It is what /health serves and what onReport is handed.
	snap *Snapshot
	// ready, when set, reports whether the daemon has finished starting.
	// See SetReady.
	ready     func() bool
	readyPoll time.Duration
	// beforeFullBusy keeps at most one beforeFull running.
	beforeFullBusy atomic.Bool
}

// defaultReadyPoll is how often the scheduler asks whether startup has
// finished, until it has. The predicate is a mutex read, so polling is
// cheap; the interval bounds how long /health can show startup-era results
// after the daemon is actually serving.
const defaultReadyPoll = 2 * time.Second

// SetReady gives the scheduler a readiness predicate, and makes it run a
// full pass the moment the daemon becomes ready.
//
// Without this, /health (which serves the snapshot) reported the startup
// pass for longer than it was true. That pass runs as the daemon comes up,
// usually before the store has opened, so it records "opening store" for
// every check that needs one. The Fast checks were corrected at the next
// tick, three minutes later; the Full-tier ones were not corrected for an
// hour. On 2026-09-21 budget.projection sat at "no default-user store yet"
// on a daemon that had been serving for minutes, and /health showed seven
// warnings on a healthy daemon for the first three minutes of every run.
//
// The Fast ticker is untouched, so a daemon that never becomes ready — a
// failed startup — still has its health refreshed rather than frozen on
// the first pass.
func (s *Scheduler) SetReady(fn func() bool) { s.ready = fn }

// NewScheduler builds a scheduler. A zero interval uses the default;
// notifier may be nil to run checks without notifications.
func NewScheduler(reg *Registry, notifier *Notifier, fast, full time.Duration) *Scheduler {
	if fast <= 0 {
		fast = DefaultFastInterval
	}
	if full <= 0 {
		full = DefaultFullInterval
	}
	return &Scheduler{reg: reg, notifier: notifier, fast: fast, full: full,
		now: time.Now, snap: NewSnapshot(), readyPoll: defaultReadyPoll}
}

// Latest is the merged report of every check's most recent result. ok is
// false until the startup run has finished, so /health can fall back to
// a live run in that window instead of serving nothing.
func (s *Scheduler) Latest() (Report, bool) { return s.snap.Report() }

// OnReport registers a sink for every report the scheduler produces (startup,
// each fast tick, and each hourly full pass). The daemon wires this to the SSE
// hub so the native dashboard panel and status glyph update live. (🎯T86)
//
// The sink receives the merged snapshot, not the run that just finished. A
// Fast run carries only Fast checks, and handing that on as-is made the
// shim drop every Full-tier result for up to an hour after each tick.
func (s *Scheduler) OnReport(fn func(Report)) { s.onReport = fn }

// Run executes the full suite once, then loops until ctx is cancelled,
// running the Fast tier each FastInterval and the full suite each
// FullInterval. Blocks; start it in a goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	lastFull := s.now()
	s.runOnce(ctx, true) // startup: full validation

	t := time.NewTicker(s.fast)
	defer t.Stop()

	// Poll for readiness only while startup is in progress. Once the
	// transition has been seen, readyC is set to nil, and a nil channel is
	// never selected, so the poll costs nothing for the rest of the run.
	var readyC <-chan time.Time
	if s.ready != nil && !s.ready() {
		rt := time.NewTicker(s.readyPoll)
		defer rt.Stop()
		readyC = rt.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-readyC:
			if s.ready() {
				readyC = nil
				lastFull = s.now()
				s.runOnce(ctx, true)
			}
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

// BeforeFull runs immediately before each full pass. The daemon uses it
// to re-evaluate the budget throttle (🎯T136) so the check that reports
// throttle state reads a value computed in the same pass rather than one
// from an hour ago.
func (s *Scheduler) BeforeFull(fn func()) { s.beforeFull = fn }

// runOnce runs the checks (full or fast tier), notifies, and logs a
// one-line summary whenever anything is not ok — including the check
// names, so a fail=1 line is not anonymous.
func (s *Scheduler) runOnce(ctx context.Context, full bool) {
	if full && s.beforeFull != nil {
		// Off the critical path. beforeFull refreshes state that a check
		// later reads (the throttle governor, for budget.throttle), and it
		// was run synchronously so that read would be current. The cost
		// was that every full pass waited for it — and the budget
		// evaluation it wraps measured over 30s on a busy daemon. The
		// check reads whatever the governor last settled on, which between
		// full passes it always did anyway; health does not wait for it.
		// One evaluation at a time: a slow one is not stacked upon.
		if s.beforeFullBusy.CompareAndSwap(false, true) {
			go func() {
				defer s.beforeFullBusy.Store(false)
				s.beforeFull()
			}()
		}
	}
	rep := s.reg.Run(ctx, full, s.now())
	// The notifier sees the run itself: it tracks transitions per check,
	// and only the checks that just ran can have transitioned.
	if s.notifier != nil {
		s.notifier.Observe(rep, s.now())
	}
	s.snap.Observe(rep, full)
	if s.onReport != nil {
		if merged, ok := s.snap.Report(); ok {
			s.onReport(merged)
		}
	}
	if rep.Fail > 0 || rep.Warn > 0 {
		var failed, warned []string
		for _, r := range rep.Results {
			switch r.Severity {
			case "fail":
				failed = append(failed, r.Name)
			case "warn":
				warned = append(warned, r.Name)
			}
		}
		slog.Warn("diag: health degraded",
			"fail", rep.Fail, "warn", rep.Warn, "ok", rep.OK, "tier_full", full,
			"failed", failed, "warned", warned)
	}
}
