// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestBudgetEndpointThrottledPayload(t *testing.T) {
	h := New(func(string) (store.Backend, error) {
		return &fakeBackend{}, nil
	})
	h.SetBudgetProvider(func() (*BudgetSnapshot, error) {
		return &BudgetSnapshot{
			Budget: &store.BudgetStatus{
				CapUSD: 100, SpentUSD: 80, SpentPct: 80, ElapsedPct: 40,
				ProjectedUSD: 160, ProjectedPct: 160, Severity: "warn",
				Headline:       "at $x/day, period exceeds cap",
				ExhaustionDate: "2026-08-20",
				GovernedUSD:    10, GovernedPct: 12.5, Priced: true,
			},
			Throttle: ThrottleSnapshot{
				Level: "reduced", Throttling: true,
				Detail:      "background agents throttled to reduced",
				Remediation: "lifts when projection falls",
			},
			Trees: []store.AgentTree{{
				SessionID: "s1", Skill: "release", Agents: 4, TreeCostUSD: 12.5, Repo: "mnemo",
			}},
		}, nil
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/budget", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var snap BudgetSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Budget == nil || snap.Budget.CapUSD != 100 {
		t.Fatalf("budget: %+v", snap.Budget)
	}
	if !snap.Throttle.Throttling || snap.Throttle.Level != "reduced" {
		t.Fatalf("throttle: %+v", snap.Throttle)
	}
	if snap.Budget.ExhaustionDate == "" {
		t.Fatal("missing exhaustion_date")
	}
	if len(snap.Trees) != 1 {
		t.Fatalf("trees: %d", len(snap.Trees))
	}

	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, httptest.NewRequest("GET", "/api/budget?trees=0", nil))
	var snap2 BudgetSnapshot
	_ = json.Unmarshal(rr2.Body.Bytes(), &snap2)
	if len(snap2.Trees) != 0 {
		t.Fatalf("trees=0 still has trees: %d", len(snap2.Trees))
	}
}

func TestAgentTreesEndpoint(t *testing.T) {
	fb := &fakeBackend{}
	h := New(func(string) (store.Backend, error) { return fb, nil })
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/agent_trees?days=7&limit=5", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d %s", rr.Code, rr.Body.String())
	}
}

func TestDashboardReferencesBudgetEndpoint(t *testing.T) {
	// Static check: dashboard asset references /api/budget and renders throttle
	// without requiring health-panel expand (🎯T140).
	path := filepath.Join("..", "..", "ui", "dashboard.html")
	// tests run with cwd package dir internal/api
	b, err := os.ReadFile(path)
	if err != nil {
		// try from repo root
		b, err = os.ReadFile(filepath.Join("ui", "dashboard.html"))
	}
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, need := range []string{"/api/budget", "loadBudget", "cardBudget", "throttl"} {
		if !strings.Contains(s, need) {
			t.Errorf("dashboard missing %q", need)
		}
	}
}
