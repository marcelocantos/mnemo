// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package throttle slows the agents mnemo invokes itself once a budget's
// soft limit is breached (🎯T136).
//
// It governs the compactor, the streaming segmenter, the reviewer and the
// image describer — and nothing else. Not because those are the expensive
// ones, but because they are the only ones reachable. Work started from
// Claude Code, and the sub-agent fan-outs it spawns, passes through
// nothing mnemo can gate.
//
// So the honest claim is narrow: this covers the BLIND SPOT — unattended
// work nobody is watching — not spend in general. The asymmetry is the
// premise rather than a limitation to engineer away. Observation is
// universal, because every session leaves a transcript and mnemo ingests
// them all; control is partial; the user closes the gap when 🎯T135's
// report tells them to. Which is why the governed fraction is reported
// (BudgetStatus.GovernedPct): a throttle that governs a minority of spend
// while the headline keeps climbing looks broken unless the report says
// which is which.
//
// Two properties distinguish this from the safeguards already shipped.
//
// It is BUDGET-driven, where 🎯T139's ErrSpendCeiling and the compactor's
// per-session ratio cap are INVOCATION-driven. Those bound a single
// runaway; this bounds the aggregate — many well-behaved invocations that
// collectively overrun. They compose, and neither replaces the other.
//
// And it is POST HOC and SOFT. Nothing is refused up front and nothing is
// hard-stopped, which is what keeps it self-contained: no admission
// control, no cross-process coordination, no agreement needed with
// anything outside this daemon.
package throttle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Class is a kind of mnemo-invoked work, ordered by TIME-INSENSITIVITY
// rather than by cost.
//
// Cost order would be the obvious choice and is the wrong one. What
// matters when spend must be reduced is which work loses least by
// happening later — deep history is just as useful tomorrow, whereas a
// live segmenter's entire product is timeliness.
type Class int

const (
	// Backfill is deep-history work: old-session compaction, image
	// description, retrospective indexing. Delaying it costs nothing but
	// latency, so it yields first.
	Backfill Class = iota

	// Compaction is recent-session summarisation. Slowed second: it has a
	// freshness cost, but a bounded one.
	Compaction

	// Segmenter is the live streaming segmenter, and is the exception —
	// it is PAUSED rather than slowed.
	//
	// Halving its rate would be the worst of both. A drip costs roughly
	// 45,000 input tokens regardless of payload, because the model's own
	// system prompt and tool definitions dominate it by ~50x, so a
	// half-rate segmenter still pays nearly the same per call while
	// producing spans too late to be fresh. That is the same money for a
	// product whose entire value was timeliness. Batch finalisation
	// covers those conversations at session close, so pausing costs
	// freshness, not coverage.
	Segmenter

	// Review is CLAUDE.md review and similar advisory work.
	Review
)

func (c Class) String() string {
	switch c {
	case Backfill:
		return "backfill"
	case Compaction:
		return "compaction"
	case Segmenter:
		return "segmenter"
	case Review:
		return "review"
	}
	return "unknown"
}

// Level is how hard the brakes are on.
type Level int

const (
	// Full is normal operation: no delay, nothing paused.
	Full Level = iota

	// Reduced is the first response to a projected overrun. Backfill and
	// review slow; the segmenter pauses.
	Reduced

	// Minimal is the response to an actual breach. Everything mnemo
	// invokes slows sharply and the segmenter stays paused.
	Minimal
)

func (l Level) String() string {
	switch l {
	case Reduced:
		return "reduced"
	case Minimal:
		return "minimal"
	}
	return "full"
}

// delays is the per-class minimum interval between invocations at each
// level. Zero means no throttling; a delay is applied BETWEEN attempts,
// never to refuse one.
var delays = map[Level]map[Class]time.Duration{
	Full: {},
	Reduced: {
		Backfill: 5 * time.Minute,
		Review:   30 * time.Minute,
	},
	Minimal: {
		Backfill:   30 * time.Minute,
		Compaction: 10 * time.Minute,
		Review:     2 * time.Hour,
	},
}

// paused reports whether a class stops entirely at a level.
func paused(l Level, c Class) bool {
	return c == Segmenter && l != Full
}

// State is the durable throttle state.
//
// Durable because a throttle that resets on restart is trivially defeated
// by the auto-upgrade path, which restarts the daemon on its own schedule
// — a budget breach would be forgotten every few days by a mechanism that
// exists for an unrelated reason.
type State struct {
	Level  Level     `json:"level"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitempty"`
	// Lifts states what would restore full rate, so the report can say it
	// rather than leaving the user to infer it.
	Lifts string `json:"lifts,omitempty"`
}

// Governor decides and holds the throttle level.
type Governor struct {
	mu    sync.Mutex
	state State
	path  string

	// last records the most recent invocation per class, so Delay can be
	// enforced without any class needing its own timer.
	last map[Class]time.Time
}

// stateFile is where the level persists, under ~/.mnemo.
const stateFile = "throttle.json"

// New returns a governor, restoring any persisted state from dir.
func New(dir string) *Governor {
	g := &Governor{path: filepath.Join(dir, stateFile), last: map[Class]time.Time{}}
	if data, err := os.ReadFile(g.path); err == nil {
		var st State
		if json.Unmarshal(data, &st) == nil {
			g.state = st
		}
	}
	return g
}

// HysteresisMargin is how far below the warning threshold the projection
// must fall before full rate resumes.
//
// Without it, spend sitting near the boundary would oscillate the system:
// throttle, projection drops because the throttle worked, un-throttle,
// projection rises again. The margin makes recovery mean something
// happened, not that the last measurement was lucky.
const HysteresisMargin = 10 // percentage points

// BudgetView is the subset of 🎯T135's status this needs. An interface-free
// struct so the throttle can be tested without a store.
type BudgetView struct {
	// Priced is false when nothing could be costed. The governor REFUSES
	// to act in that case.
	Priced bool

	// CapUSD is zero when no budget is configured.
	CapUSD float64

	// SpentPct and ProjectedPct are against the cap.
	SpentPct     float64
	ProjectedPct float64

	// WarnPct is the configured projected-consumption threshold.
	WarnPct float64
}

// Evaluate updates the throttle level from a budget reading.
//
// It REFUSES TO ACT on a number it cannot trust. With no rate card
// nothing is priceable, so every figure is zero — and zero is
// indistinguishable from thrift. Throttling on that would be arbitrary;
// worse, NOT throttling on it looks identical to being under budget. The
// governor stays at its current level and says why, rather than guessing
// in either direction.
//
// The same applies to spend 🎯T135 reports as uncountable: it never
// reaches SpentPct, so it can neither trigger this nor mask it.
func (g *Governor) Evaluate(b BudgetView) State {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case !b.Priced:
		g.state.Reason = "budget not enforced: no rate card, so spend cannot be " +
			"priced. Zero here means unpriced, not unspent"
		g.state.Lifts = `enable {"pricing": {"enabled": true}} so the rate card can be fetched`
		return g.state
	case b.CapUSD <= 0:
		return g.set(Full, "no budget cap configured", "")
	}

	warn := b.WarnPct
	if warn <= 0 {
		warn = 100
	}

	switch {
	case b.SpentPct >= 100:
		return g.set(Minimal,
			fmt.Sprintf("budget exhausted: %.0f%% of cap spent", b.SpentPct),
			"the next budget period, or a higher monthly_cap_usd")
	case b.ProjectedPct >= warn:
		return g.set(Reduced,
			fmt.Sprintf("projected to finish at %.0f%% of cap (threshold %.0f%%)",
				b.ProjectedPct, warn),
			fmt.Sprintf("projection falling below %.0f%%", warn-HysteresisMargin))
	}

	// Recovery requires clearing the threshold by the margin, not merely
	// dipping under it.
	if g.state.Level != Full && b.ProjectedPct >= warn-HysteresisMargin {
		return g.state
	}
	return g.set(Full, "", "")
}

// set records a new level, persisting it when it changes.
func (g *Governor) set(l Level, reason, lifts string) State {
	if g.state.Level == l && g.state.Reason == reason {
		return g.state
	}
	g.state = State{Level: l, Reason: reason, Lifts: lifts, Since: time.Now().UTC()}
	if l == Full {
		g.state.Since = time.Time{}
	}
	if data, err := json.Marshal(g.state); err == nil {
		_ = os.WriteFile(g.path, data, 0o644)
	}
	return g.state
}

// State returns the current throttle state.
func (g *Governor) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Allow reports whether a class may run now, and how long to wait if not.
//
// Soft by construction: a false here means "not yet", never "no". Callers
// wait and retry rather than dropping work, so throttling delays spend
// instead of discarding the product of it — with the single exception of
// the paused segmenter, whose work is covered by batch finalisation at
// session close.
func (g *Governor) Allow(c Class) (ok bool, wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if paused(g.state.Level, c) {
		return false, 0 // 0 = indefinite; caller should stop, not spin
	}
	d := delays[g.state.Level][c]
	if d == 0 {
		g.last[c] = time.Now()
		return true, 0
	}
	if since := time.Since(g.last[c]); since < d {
		return false, d - since
	}
	g.last[c] = time.Now()
	return true, 0
}

// Paused reports whether a class is stopped outright rather than slowed.
func (g *Governor) Paused(c Class) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return paused(g.state.Level, c)
}

// Describe renders the state for the health report and the dashboard.
//
// Throttling is LOUD by requirement: a silent throttle is
// indistinguishable from a hang, and the first thing anyone does about an
// apparent hang is restart the daemon — which, without durable state,
// would have cleared the throttle.
func (g *Governor) Describe() (detail, remediation string) {
	st := g.State()
	if st.Level == Full {
		if st.Reason != "" {
			return "not throttling: " + st.Reason, st.Lifts
		}
		return "background agents running at full rate", ""
	}
	d := fmt.Sprintf("background agents throttled to %s — %s", st.Level, st.Reason)
	if !st.Since.IsZero() {
		d += fmt.Sprintf(" (since %s)", st.Since.Format(time.RFC3339))
	}
	var classes []string
	for _, c := range []Class{Backfill, Compaction, Segmenter, Review} {
		switch {
		case paused(st.Level, c):
			classes = append(classes, c.String()+": paused")
		case delays[st.Level][c] > 0:
			classes = append(classes, fmt.Sprintf("%s: min %s between runs",
				c, delays[st.Level][c]))
		}
	}
	for i, c := range classes {
		if i == 0 {
			d += ". "
		} else {
			d += "; "
		}
		d += c
	}
	rem := "this governs only agents mnemo invokes itself; spend from Claude " +
		"Code sessions and their sub-agents is observed but cannot be gated here"
	if st.Lifts != "" {
		rem = "lifts when " + st.Lifts + ". " + rem
	}
	return d, rem
}
