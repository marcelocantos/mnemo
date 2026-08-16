// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
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
