// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/api"
	"github.com/marcelocantos/mnemo/internal/store"
)

// TestPrintBudgetHuman shows throttle + spend fields (CLI body, 🎯T140).
func TestPrintBudgetHuman(t *testing.T) {
	snap := api.BudgetSnapshot{
		Budget: &store.BudgetStatus{
			CapUSD: 100, SpentUSD: 42, SpentPct: 42, ElapsedPct: 50,
			ProjectedUSD: 90, ProjectedPct: 90, Severity: "ok",
			Headline: "on track", GovernedUSD: 5, GovernedPct: 12, Priced: true,
			Period: store.BudgetPeriod{Label: "2026-08"},
		},
		Throttle: api.ThrottleSnapshot{
			Level: "reduced", Throttling: true,
			Detail: "background agents throttled to reduced",
			Remediation: "lifts when projection falls",
		},
		Trees: []store.AgentTree{{TreeCostUSD: 3.5, Agents: 2, Skill: "cv", Repo: "mnemo"}},
	}
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printBudgetHuman(snap)
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()
	for _, need := range []string{"cap", "spent", "projected", "Throttle", "ACTIVE", "reduced", "agent trees", "cv"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(need)) {
			t.Errorf("CLI output missing %q\n%s", need, out)
		}
	}
}
