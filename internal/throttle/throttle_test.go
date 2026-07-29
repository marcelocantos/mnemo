// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package throttle

import (
	"strings"
	"testing"
)

// healthy is a budget comfortably inside its cap.
func healthy() BudgetView {
	return BudgetView{Priced: true, CapUSD: 500, SpentPct: 20, ProjectedPct: 40, WarnPct: 100}
}

// TestRefusesToActOnAnUntrustworthyNumber is the prerequisite guard.
//
// With no rate card nothing is priceable, so every figure is zero — and
// zero is indistinguishable from thrift. Both available responses are
// wrong if taken as a judgement: throttling would be arbitrary, and not
// throttling looks exactly like being under budget. The governor must
// hold its level and say why.
func TestRefusesToActOnAnUntrustworthyNumber(t *testing.T) {
	g := New(t.TempDir())

	// Get it throttled on a trustworthy reading first.
	g.Evaluate(BudgetView{Priced: true, CapUSD: 500, SpentPct: 120, ProjectedPct: 150, WarnPct: 100})
	if g.State().Level != Minimal {
		t.Fatalf("setup failed: level = %s, want minimal", g.State().Level)
	}

	// Now the rate card disappears. Zeroes everywhere.
	st := g.Evaluate(BudgetView{Priced: false})
	if st.Level != Minimal {
		t.Errorf("level = %s, want minimal retained: an unpriced reading is not "+
			"evidence that spending stopped", st.Level)
	}
	if !strings.Contains(st.Reason, "unpriced") && !strings.Contains(st.Reason, "no rate card") {
		t.Errorf("reason %q does not explain that the number is untrustworthy", st.Reason)
	}
	if st.Lifts == "" {
		t.Error("no remediation offered for an unenforceable budget")
	}
}

// TestHysteresisPreventsOscillation pins the margin.
//
// Spend hovering at the boundary would otherwise cycle: throttle,
// projection falls because the throttle worked, un-throttle, projection
// rises. Recovery must mean something changed, not that one measurement
// was lucky.
func TestHysteresisPreventsOscillation(t *testing.T) {
	g := New(t.TempDir())

	over := BudgetView{Priced: true, CapUSD: 500, SpentPct: 50, ProjectedPct: 105, WarnPct: 100}
	if st := g.Evaluate(over); st.Level != Reduced {
		t.Fatalf("level = %s, want reduced at 105%% projected", st.Level)
	}

	// Just under the threshold, but inside the margin: must stay throttled.
	justUnder := over
	justUnder.ProjectedPct = 95
	if st := g.Evaluate(justUnder); st.Level != Reduced {
		t.Errorf("level = %s at 95%% projected, want reduced retained: 95 is "+
			"inside the %d-point margin below the 100%% threshold",
			st.Level, HysteresisMargin)
	}

	// Clear of the margin: full rate resumes.
	clear := over
	clear.ProjectedPct = 100 - HysteresisMargin - 1
	if st := g.Evaluate(clear); st.Level != Full {
		t.Errorf("level = %s at %.0f%% projected, want full: it has cleared the margin",
			st.Level, clear.ProjectedPct)
	}
}

// TestSegmenterIsPausedNotSlowed pins the one class that must not be
// merely rate-limited.
//
// A drip costs ~45,000 input tokens regardless of payload, because the
// model's system prompt and tool definitions dominate it by ~50x. Halving
// the rate therefore pays nearly the same money per call for spans that
// arrive too late to be fresh — the same spend for a product whose entire
// value was timeliness. Batch finalisation covers those conversations at
// session close, so pausing costs freshness, not coverage.
func TestSegmenterIsPausedNotSlowed(t *testing.T) {
	g := New(t.TempDir())
	g.Evaluate(BudgetView{Priced: true, CapUSD: 500, SpentPct: 50, ProjectedPct: 120, WarnPct: 100})

	if !g.Paused(Segmenter) {
		t.Error("segmenter not paused while throttled; a half-rate segmenter " +
			"spends nearly the same money for spans too late to be useful")
	}
	if ok, _ := g.Allow(Segmenter); ok {
		t.Error("Allow permitted a paused segmenter run")
	}
	if delays[Reduced][Segmenter] != 0 || delays[Minimal][Segmenter] != 0 {
		t.Error("segmenter has a delay configured; it must be paused outright, " +
			"and a delay would silently take precedence in a future edit")
	}
}

// TestThrottleOrderIsByTimeInsensitivity pins the ordering rationale.
//
// Backfill yields before compaction because delaying deep history costs
// only latency. Cost order would be the obvious choice and the wrong one.
func TestThrottleOrderIsByTimeInsensitivity(t *testing.T) {
	g := New(t.TempDir())

	// First response to a projected overrun: backfill slows, recent
	// compaction does not.
	g.Evaluate(BudgetView{Priced: true, CapUSD: 500, SpentPct: 50, ProjectedPct: 110, WarnPct: 100})
	if delays[Reduced][Backfill] == 0 {
		t.Error("backfill not slowed at the first level; it is the work that " +
			"loses least by happening later")
	}
	if delays[Reduced][Compaction] != 0 {
		t.Error("recent compaction slowed at the first level, ahead of backfill")
	}

	// An actual breach reaches compaction too.
	g.Evaluate(BudgetView{Priced: true, CapUSD: 500, SpentPct: 110, ProjectedPct: 130, WarnPct: 100})
	if delays[Minimal][Compaction] == 0 {
		t.Error("compaction still unthrottled with the budget exhausted")
	}
	if delays[Minimal][Backfill] <= delays[Reduced][Backfill] {
		t.Error("backfill is not slowed further at the harsher level")
	}
}

// TestStateSurvivesRestart pins durability.
//
// A throttle that resets on restart is trivially defeated by the
// auto-upgrade path, which restarts the daemon on its own schedule — so a
// budget breach would be forgotten every few days by a mechanism that
// exists for a completely unrelated reason.
func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	g := New(dir)
	g.Evaluate(BudgetView{Priced: true, CapUSD: 500, SpentPct: 120, ProjectedPct: 150, WarnPct: 100})

	revived := New(dir)
	if revived.State().Level != Minimal {
		t.Errorf("level after restart = %s, want minimal", revived.State().Level)
	}
	if revived.State().Reason == "" {
		t.Error("reason lost across restart; the throttle would be unexplainable")
	}
	if !revived.Paused(Segmenter) {
		t.Error("segmenter resumed after a restart despite a breached budget")
	}
}

// TestThrottlingIsLoud pins the reporting requirement.
//
// A silent throttle is indistinguishable from a hang, and the first thing
// anyone does about an apparent hang is restart the daemon.
func TestThrottlingIsLoud(t *testing.T) {
	g := New(t.TempDir())
	g.Evaluate(BudgetView{Priced: true, CapUSD: 500, SpentPct: 50, ProjectedPct: 130, WarnPct: 100})

	detail, remediation := g.Describe()
	for _, want := range []string{"throttled", "reduced", "projected"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q omits %q", detail, want)
		}
	}
	if !strings.Contains(detail, "paused") {
		t.Errorf("detail %q does not say the segmenter is paused", detail)
	}
	if !strings.Contains(remediation, "lifts when") {
		t.Errorf("remediation %q does not say what would restore full rate", remediation)
	}
	// The partial coverage must be stated, not implied: otherwise a rising
	// total alongside an active throttle reads as a broken throttle.
	if !strings.Contains(remediation, "cannot be gated") {
		t.Errorf("remediation %q does not state that coverage is partial", remediation)
	}
}

// TestFullRateWhenHealthy is the ordinary case: no cap breach, no delays.
func TestFullRateWhenHealthy(t *testing.T) {
	g := New(t.TempDir())
	if st := g.Evaluate(healthy()); st.Level != Full {
		t.Fatalf("level = %s on a healthy budget, want full", st.Level)
	}
	for _, c := range []Class{Backfill, Compaction, Segmenter, Review} {
		if ok, wait := g.Allow(c); !ok {
			t.Errorf("%s blocked at full rate (wait %s)", c, wait)
		}
	}
}

// TestNoCapMeansNoThrottle covers the default posture: mnemo measures
// whether or not it has been asked to police anything.
func TestNoCapMeansNoThrottle(t *testing.T) {
	g := New(t.TempDir())
	st := g.Evaluate(BudgetView{Priced: true, CapUSD: 0})
	if st.Level != Full {
		t.Errorf("level = %s with no cap configured, want full", st.Level)
	}
	if g.Paused(Segmenter) {
		t.Error("segmenter paused with no budget configured")
	}
}

// TestAllowIsSoftNotRefusal pins that throttling delays work rather than
// discarding it — the property that keeps this self-contained, with no
// admission control anywhere.
func TestAllowIsSoftNotRefusal(t *testing.T) {
	g := New(t.TempDir())
	g.Evaluate(BudgetView{Priced: true, CapUSD: 500, SpentPct: 50, ProjectedPct: 110, WarnPct: 100})

	if ok, _ := g.Allow(Backfill); !ok {
		t.Fatal("first backfill attempt blocked; the delay is BETWEEN runs")
	}
	ok, wait := g.Allow(Backfill)
	if ok {
		t.Error("second immediate backfill attempt allowed; no delay applied")
	}
	if wait <= 0 {
		t.Error("blocked without telling the caller how long to wait, which " +
			"turns a delay into a refusal")
	}
}
