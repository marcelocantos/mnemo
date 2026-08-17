// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strconv"

	"github.com/marcelocantos/mnemo/internal/store"
)

// BudgetSnapshot is the single payload for dashboard, CLI, and menubar spend
// surfaces (🎯T140). Built from BudgetStatusNow + Governor.Describe + optional
// AgentTrees — not from the generic health checklist alone.
type BudgetSnapshot struct {
	Budget   *store.BudgetStatus `json:"budget"`
	Throttle ThrottleSnapshot    `json:"throttle"`
	Trees    []store.AgentTree   `json:"agent_trees,omitempty"`
}

// ThrottleSnapshot is the control half of the budget surface.
type ThrottleSnapshot struct {
	// Level is full | reduced | minimal.
	Level string `json:"level"`
	// Throttling is true when Level is not full.
	Throttling  bool   `json:"throttling"`
	Reason      string `json:"reason,omitempty"`
	Lifts       string `json:"lifts,omitempty"`
	Since       string `json:"since,omitempty"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// BudgetProvider builds a BudgetSnapshot for the default user. Wired from
// main so the API layer does not import registry/throttle cycles.
type BudgetProvider func() (*BudgetSnapshot, error)

// SetBudgetProvider wires GET /api/budget (and the trees list embedded in it).
func (h *Handler) SetBudgetProvider(fn BudgetProvider) {
	h.budgetProvider = fn
}

// budget serves GET /api/budget — cap, projection, throttle, agent trees.
func (h *Handler) budget(w http.ResponseWriter, r *http.Request) {
	if h.budgetProvider == nil {
		http.Error(w, "budget provider not configured", http.StatusServiceUnavailable)
		return
	}
	snap, err := h.budgetProvider()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Optional trees limit for lighter CLI polls: ?trees=0 to omit.
	if r.URL.Query().Get("trees") == "0" && snap != nil {
		cp := *snap
		cp.Trees = nil
		writeJSON(w, &cp)
		return
	}
	writeJSON(w, snap)
}

// agentTrees serves GET /api/agent_trees?days=7&limit=20 — T137 surface after
// mnemo_agent_trees was removed (🎯T140).
func (h *Handler) agentTrees(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 7
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = store.DefaultAgentTreeLimit
	}
	trees, err := mem.AgentTrees(store.AgentTreeParams{
		Days:       days,
		RepoFilter: r.URL.Query().Get("repo"),
		Limit:      limit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if trees == nil {
		trees = []store.AgentTree{}
	}
	writeJSON(w, map[string]any{"trees": trees})
}
