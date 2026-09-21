// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/throttle"
)

// TestNotifyThrottleLevelChange drives the shipped notify helper used by
// EvaluateThrottle on governor level edges (🎯T140).
func TestNotifyThrottleLevelChange(t *testing.T) {
	r := NewRegistry(context.Background(), store.Config{}, "")
	n := diag.NewNotifier(diag.DefaultNotifierConfig("http://dash/#health"))
	var alerts []diag.Alert
	n.OnAlert(func(a diag.Alert) { alerts = append(alerts, a) })
	r.SetThrottleNotifier(n)

	// Engage: Full → Reduced via governor so Describe() is real.
	g := r.Governor()
	st := g.Evaluate(throttle.BudgetView{
		Priced: true, CapUSD: 100, SpentPct: 0, ProjectedPct: 150, WarnPct: 100,
	})
	if st.Level == throttle.Full {
		t.Fatal("expected engage")
	}
	r.notifyThrottleLevelChange(throttle.Full, st.Level, n)
	if len(alerts) != 1 {
		t.Fatalf("engage: %d alerts", len(alerts))
	}
	a := alerts[0]
	if a.Name != diag.ThrottleCheckName || a.Kind != "fail" || a.Severity != "fail" {
		t.Fatalf("engage payload %+v", a)
	}
	if a.Detail == "" {
		t.Fatal("engage detail empty")
	}
	if a.DashboardURL != "http://dash/#health" {
		t.Fatalf("dashboard URL %q", a.DashboardURL)
	}

	// Same level again: no second alert.
	r.notifyThrottleLevelChange(st.Level, st.Level, n)
	if len(alerts) != 1 {
		t.Fatalf("same-level pushed: %d", len(alerts))
	}

	// Lift.
	prev := st.Level
	st = g.Evaluate(throttle.BudgetView{
		Priced: true, CapUSD: 100, SpentPct: 5, ProjectedPct: 10, WarnPct: 100,
	})
	if st.Level != throttle.Full {
		t.Fatalf("want Full, got %v", st.Level)
	}
	r.notifyThrottleLevelChange(prev, st.Level, n)
	if len(alerts) != 2 || alerts[1].Kind != "recovery" {
		t.Fatalf("lift: %+v", alerts)
	}

	// Nil notifier is a no-op.
	r.notifyThrottleLevelChange(throttle.Full, throttle.Reduced, nil)
}

// TestEvaluateThrottleNoStore is a quiet path: no default user store.
func TestEvaluateThrottleNoStore(t *testing.T) {
	r := NewRegistry(context.Background(), store.Config{
		Budget: store.BudgetConfig{MonthlyCapUSD: 100},
	}, "")
	n := diag.NewNotifier(diag.DefaultNotifierConfig("http://x"))
	var nAlert int
	n.OnAlert(func(diag.Alert) { nAlert++ })
	r.SetThrottleNotifier(n)
	r.EvaluateThrottle("default")
	if nAlert != 0 {
		t.Fatalf("alerts without store: %d", nAlert)
	}
}

// TestEvaluateThrottleWithoutACapLiftsAndSkipsTheSpendAggregate covers the
// no-cap short-circuit (2026-09-21).
//
// Two properties. First, with no cap there is nothing to enforce, so a
// throttle left over from an earlier config that did have one must lift.
// Before the short-circuit it did not: Evaluate checks Priced before it
// checks the cap, and on an unpriced store the unpriced branch returned
// with the level untouched. Second, the lift must not depend on the
// month's spend — that aggregate is what delayed full health passes by
// tens of seconds, on machines with no budget configured at all.
func TestEvaluateThrottleWithoutACapLiftsAndSkipsTheSpendAggregate(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "t.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.AwaitStartup()

	r := NewRegistry(context.Background(), store.Config{}, dir) // no cap
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	// A throttle persisted under an earlier config with a cap.
	r.Governor().Evaluate(throttle.BudgetView{
		Priced: true, CapUSD: 100, SpentPct: 120, ProjectedPct: 200, WarnPct: 100,
	})
	if r.Governor().State().Level == throttle.Full {
		t.Fatal("setup: expected an engaged throttle")
	}

	n := diag.NewNotifier(diag.DefaultNotifierConfig("http://x"))
	var kinds []string
	n.OnAlert(func(a diag.Alert) { kinds = append(kinds, a.Kind) })
	r.SetThrottleNotifier(n)

	// Close the store so any attempt to compute spend would error out and
	// take the early return that leaves the level engaged. Reaching Full
	// therefore shows the lift came without touching the database.
	_ = s.Close()
	r.EvaluateThrottle("default")

	st := r.Governor().State()
	if st.Level != throttle.Full {
		t.Fatalf("level = %v, want Full: with no cap there is nothing to enforce", st.Level)
	}
	if st.Reason != "no budget cap configured" {
		t.Errorf("reason = %q, want the no-cap reason", st.Reason)
	}
	if len(kinds) != 1 || kinds[0] != "recovery" {
		t.Errorf("alerts = %v, want one recovery for the lift", kinds)
	}
}
