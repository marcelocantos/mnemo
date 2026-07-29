// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"testing"
	"time"
)

// asstAgent builds an assistant record, optionally as a sub-agent turn.
func asstAgent(id, ts, agentID, skill, agentType string, out int) map[string]any {
	m := map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"uuid":      "uuid-" + id,
		"message": map[string]any{
			"role":    "assistant",
			"id":      "msg_" + id,
			"model":   "claude-sonnet-4-6",
			"content": "x",
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": out,
			},
		},
	}
	if agentID != "" {
		m["isSidechain"] = true
		m["agentId"] = agentID
		if skill != "" {
			m["attributionSkill"] = skill
		}
		if agentType != "" {
			m["attributionAgent"] = agentType
		}
	}
	return m
}

// TestFanOutRollsUpAboveASingleExpensiveAgent is the failure this whole
// feature exists for.
//
// Twenty sub-agents at $1.50 each is a bigger problem than one agent at
// $15, and a per-agent ranking shows the single agent on top with the
// twenty scattered below it, individually unremarkable. Only the
// aggregate at the root reverses that.
func TestFanOutRollsUpAboveASingleExpensiveAgent(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	base := time.Now().UTC().Add(-2 * time.Hour)
	ts := func(i int) string { return base.Add(time.Duration(i) * time.Second).Format(time.RFC3339) }

	// Session A: a wide fan-out of twenty individually modest agents.
	fanout := []map[string]any{asstAgent("a-main", ts(0), "", "", "", 1_000)}
	for i := 0; i < 20; i++ {
		fanout = append(fanout, asstAgent(
			fmt.Sprintf("a-%d", i), ts(i+1),
			fmt.Sprintf("agent%d", i), "release", "Explore", 100_000))
	}
	writeJSONL(t, dir, "p", "sess-fanout", fanout)

	// Session B: one agent, more expensive than any single agent in A.
	writeJSONL(t, dir, "p", "sess-single", []map[string]any{
		asstAgent("b-main", ts(0), "", "", "", 1_000),
		asstAgent("b-1", ts(1), "solo", "", "general-purpose", 500_000),
	})

	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	trees, err := s.AgentTrees(AgentTreeParams{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) < 2 {
		t.Fatalf("got %d trees, want 2", len(trees))
	}

	top := trees[0]
	if top.Agents != 20 {
		t.Errorf("top tree has %d agents, want the 20-agent fan-out. A "+
			"per-agent ranking would have put the single $-heavy agent first, "+
			"which is exactly the blindness this exists to fix", top.Agents)
	}
	if top.TreeCostUSD <= trees[1].TreeCostUSD {
		t.Errorf("fan-out tree $%.2f does not outrank the single agent $%.2f",
			top.TreeCostUSD, trees[1].TreeCostUSD)
	}
	// Each individual agent must look unremarkable next to the solo one —
	// otherwise the test is not exercising the case it claims to.
	for _, n := range top.Nodes {
		if n.CostUSD >= trees[1].TreeCostUSD {
			t.Fatalf("fan-out agent $%.2f is individually larger than the solo "+
				"agent $%.2f; this fixture does not exercise the aggregate case",
				n.CostUSD, trees[1].TreeCostUSD)
		}
	}
}

// TestRootCauseIsIdentifiable pins the actionability requirement. "You
// spent a lot" is not actionable; "the release skill spawned 20 agents"
// is.
func TestRootCauseIsIdentifiable(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	writeJSONL(t, dir, "p", "sess-root", []map[string]any{
		asstAgent("r-main", now, "", "", "", 1_000),
		asstAgent("r-1", now, "ag1", "release", "Explore", 50_000),
		asstAgent("r-2", now, "ag2", "release", "Explore", 50_000),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	trees, err := s.AgentTrees(AgentTreeParams{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) != 1 {
		t.Fatalf("got %d trees, want 1", len(trees))
	}
	tr := trees[0]
	if tr.Skill != "release" {
		t.Errorf("skill = %q, want release: without it the report cannot say "+
			"what started the fan-out", tr.Skill)
	}
	if len(tr.AgentTypes) == 0 || tr.AgentTypes[0] != "Explore" {
		t.Errorf("agent types = %v, want [Explore]", tr.AgentTypes)
	}
	if tr.Action == "" {
		t.Error("no action stated; a finished tree and a running one call for " +
			"completely different responses")
	}
}

// TestTreeCostIsSeparateFromDirectCost pins the distinction that decides
// where to look: a session expensive by itself is a different problem
// from one expensive because of its children.
func TestTreeCostIsSeparateFromDirectCost(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	writeJSONL(t, dir, "p", "sess-split", []map[string]any{
		asstAgent("s-main", now, "", "", "", 400_000), // the user's own turns
		asstAgent("s-1", now, "ag1", "", "Explore", 100_000),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	trees, err := s.AgentTrees(AgentTreeParams{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	tr := trees[0]
	if tr.DirectCostUSD <= 0 {
		t.Error("direct cost is zero despite main-line turns")
	}
	if tr.TreeCostUSD <= 0 {
		t.Error("tree cost is zero despite a sub-agent")
	}
	if tr.DirectCostUSD <= tr.TreeCostUSD {
		t.Errorf("direct $%.4f should exceed tree $%.4f for this fixture; the "+
			"two are being conflated", tr.DirectCostUSD, tr.TreeCostUSD)
	}
	if diff := tr.TotalCostUSD - (tr.DirectCostUSD + tr.TreeCostUSD); diff > 1e-9 || diff < -1e-9 {
		t.Errorf("total $%.6f != direct $%.6f + tree $%.6f",
			tr.TotalCostUSD, tr.DirectCostUSD, tr.TreeCostUSD)
	}
}

// TestUnpricedTreeIsNotRankedAtZero pins the refusal.
//
// A fan-out ranked at $0.00 because its model has no rate is worse than
// one ranked as unknown: zero sorts to the bottom and reads as harmless,
// which is the opposite of the truth.
func TestUnpricedTreeIsNotRankedAtZero(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	rec := asstAgent("u-1", now, "ag1", "", "Explore", 100_000)
	rec["message"].(map[string]any)["model"] = "some-unreleased-model"
	writeJSONL(t, dir, "p", "sess-unpriced", []map[string]any{
		asstAgent("u-main", now, "", "", "", 1_000), rec,
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	trees, err := s.AgentTrees(AgentTreeParams{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) != 1 {
		t.Fatalf("got %d trees, want 1", len(trees))
	}
	if trees[0].Priced {
		t.Error("tree reports as priced despite a model with no rate")
	}
	if len(trees[0].UnpricedModels) == 0 {
		t.Error("no unpriced model named; the figure cannot be interpreted " +
			"without knowing what is missing from it")
	}
}

// TestNoFanOutMeansNoTree keeps the report about trees. A session that
// never spawned an agent is not a one-node tree; it is not a tree.
func TestNoFanOutMeansNoTree(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	writeJSONL(t, dir, "p", "sess-plain", []map[string]any{
		asstAgent("p-1", now, "", "", "", 100_000),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	trees, err := s.AgentTrees(AgentTreeParams{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) != 0 {
		t.Errorf("got %d trees for a session with no sub-agents", len(trees))
	}
}
