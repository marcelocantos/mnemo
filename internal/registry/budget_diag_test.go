// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/throttle"
)

// TestBudgetDiagChecks exercise the shipped budget.projection and
// budget.throttle checks via BuildDiagRegistry (🎯T140 hygiene).
func TestBudgetDiagChecks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	s, err := store.New(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	r := NewRegistry(context.Background(), store.Config{
		Budget: store.BudgetConfig{MonthlyCapUSD: 500, WarnAtPct: 100},
	}, dir)
	r.governor = throttle.New(t.TempDir())
	// Inject store as default user so checks can resolve it.
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	// Force throttle engaged so budget.throttle is warn.
	_ = r.governor.Evaluate(throttle.BudgetView{
		Priced: true, CapUSD: 100, SpentPct: 0, ProjectedPct: 200, WarnPct: 100,
	})

	reg := r.BuildDiagRegistry("default", time.Now().Add(-time.Hour))
	// Run only the named checks by scanning full report.
	rep := reg.Run(context.Background(), true, time.Now())
	var sawProj, sawThr bool
	for _, res := range rep.Results {
		switch res.Name {
		case "budget.projection":
			sawProj = true
			// With no priced usage, may be unpriced warn or healthy no-spend —
			// either is a valid check result (not a panic).
			if res.Severity != "ok" && res.Severity != "warn" {
				t.Errorf("budget.projection severity=%s detail=%s", res.Severity, res.Detail)
			}
		case "budget.throttle":
			sawThr = true
			if res.Severity != "warn" {
				t.Errorf("budget.throttle want warn when engaged, got %s detail=%s",
					res.Severity, res.Detail)
			}
			if res.Detail == "" {
				t.Error("budget.throttle empty detail")
			}
		}
	}
	if !sawProj || !sawThr {
		t.Fatalf("missing checks proj=%v thr=%v (results=%d)", sawProj, sawThr, len(rep.Results))
	}
	_ = diag.OK
}

// TestBudgetDiagThrottleFullIsHealthy when governor is Full.
func TestBudgetDiagThrottleFullIsHealthy(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	s, err := store.New(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	r := NewRegistry(context.Background(), store.Config{}, dir)
	// Isolate durable governor state from other tests (same ~/.mnemo path).
	r.governor = throttle.New(t.TempDir())
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()
	// Governor starts Full.
	reg := r.BuildDiagRegistry("default", time.Now())
	rep := reg.Run(context.Background(), false, time.Now())
	for _, res := range rep.Results {
		if res.Name == "budget.throttle" && res.Severity != "ok" {
			t.Fatalf("want ok when Full, got %s %s", res.Severity, res.Detail)
		}
	}
}
