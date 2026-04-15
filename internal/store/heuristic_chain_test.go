// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

// clearMsg returns a user-role message whose text is the /clear
// slash-command envelope — the marker InferChainHeuristic looks for
// to identify rollover sessions.
func clearMsg(ts, cwd string) map[string]any {
	m := map[string]any{
		"type":      "user",
		"timestamp": ts,
		"message":   map[string]any{"content": "<command-name>/clear</command-name>"},
	}
	if cwd != "" {
		m["cwd"] = cwd
	}
	return m
}

// TestInferChainHeuristicFindsRecentPredecessor verifies the
// cwd-most-recent rule: a /clear-marked session finds its predecessor
// by matching cwd and ordering by last_msg.
func TestInferChainHeuristicFindsRecentPredecessor(t *testing.T) {
	projectDir := t.TempDir()
	cwd := "/Users/dev/work/myrepo"

	writeJSONL(t, projectDir, "proj", "pred-session", []map[string]any{
		metaMsg("user", "first session.", "2026-04-10T10:00:00Z", cwd, ""),
		msg("user", "end", "2026-04-10T10:00:10Z"),
	})
	writeJSONL(t, projectDir, "proj", "succ-session", []map[string]any{
		clearMsg("2026-04-10T10:00:12Z", cwd),
		msg("user", "continue.", "2026-04-10T10:00:12Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	cands, err := s.InferChainHeuristic("succ-session", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(cands), cands)
	}
	if cands[0].PredecessorID != "pred-session" {
		t.Errorf("predecessor: got %q, want pred-session", cands[0].PredecessorID)
	}
	if cands[0].Mechanism != "cwd_most_recent" {
		t.Errorf("mechanism: got %q, want cwd_most_recent", cands[0].Mechanism)
	}
	if cands[0].Confidence != "high" {
		t.Errorf("confidence: got %q, want high (gap < 30s)", cands[0].Confidence)
	}
}

// TestInferChainHeuristicConfidenceDegradesWithGap verifies that a
// larger gap between predecessor and successor downgrades confidence
// to "medium".
func TestInferChainHeuristicConfidenceDegradesWithGap(t *testing.T) {
	projectDir := t.TempDir()
	cwd := "/Users/dev/work/myrepo"

	writeJSONL(t, projectDir, "proj", "pred-slow", []map[string]any{
		metaMsg("user", "prior session", "2026-04-10T10:00:00Z", cwd, ""),
		msg("user", "end", "2026-04-10T10:00:10Z"),
	})
	writeJSONL(t, projectDir, "proj", "succ-slow", []map[string]any{
		clearMsg("2026-04-10T10:05:00Z", cwd), // 4m50s gap
		msg("user", "continue", "2026-04-10T10:05:00Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	cands, err := s.InferChainHeuristic("succ-slow", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	if cands[0].Confidence != "medium" {
		t.Errorf("confidence for large gap: got %q, want medium", cands[0].Confidence)
	}
}

// TestInferChainHeuristicReturnsEmptyForNonRollover verifies that a
// session whose first user message is NOT a /clear command yields no
// candidates — the heuristic only applies to rollovers.
func TestInferChainHeuristicReturnsEmptyForNonRollover(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "proj", "fresh-session", []map[string]any{
		metaMsg("user", "starting fresh", "2026-04-10T10:00:00Z", "/some/cwd", ""),
		msg("assistant", "ok", "2026-04-10T10:00:05Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	cands, err := s.InferChainHeuristic("fresh-session", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("non-rollover should produce no candidates, got %+v", cands)
	}
}

// TestInferChainHeuristicScopeByCwd verifies that predecessors in a
// different cwd are NOT returned.
func TestInferChainHeuristicScopeByCwd(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "projA", "pred-other-cwd", []map[string]any{
		metaMsg("user", "different repo", "2026-04-10T10:00:00Z", "/Users/dev/work/repoA", ""),
		msg("user", "end", "2026-04-10T10:00:10Z"),
	})
	writeJSONL(t, projectDir, "projB", "succ-this-cwd", []map[string]any{
		metaMsg("user", "<command-name>/clear</command-name>", "2026-04-10T10:00:12Z",
			"/Users/dev/work/repoB", ""),
		msg("user", "continue", "2026-04-10T10:00:12Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	cands, err := s.InferChainHeuristic("succ-this-cwd", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("cross-cwd sessions should not appear as candidates, got %+v", cands)
	}
}

// TestInferChainHeuristicMultipleCandidates verifies that limit>1
// returns multiple candidates when they exist, ordered newest-first.
func TestInferChainHeuristicMultipleCandidates(t *testing.T) {
	projectDir := t.TempDir()
	cwd := "/Users/dev/work/myrepo"

	writeJSONL(t, projectDir, "proj", "pred-older", []map[string]any{
		metaMsg("user", "older", "2026-04-10T09:00:00Z", cwd, ""),
		msg("user", "end", "2026-04-10T09:00:10Z"),
	})
	writeJSONL(t, projectDir, "proj", "pred-newer", []map[string]any{
		metaMsg("user", "newer", "2026-04-10T09:30:00Z", cwd, ""),
		msg("user", "end", "2026-04-10T09:30:10Z"),
	})
	writeJSONL(t, projectDir, "proj", "succ-chain", []map[string]any{
		clearMsg("2026-04-10T10:00:00Z", cwd),
		msg("user", "continue", "2026-04-10T10:00:00Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	cands, err := s.InferChainHeuristic("succ-chain", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %+v", len(cands), cands)
	}
	// Newest first.
	if cands[0].PredecessorID != "pred-newer" {
		t.Errorf("first candidate should be newest, got %q", cands[0].PredecessorID)
	}
	if cands[1].PredecessorID != "pred-older" {
		t.Errorf("second candidate should be older, got %q", cands[1].PredecessorID)
	}
}
