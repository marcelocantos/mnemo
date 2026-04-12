// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

// clearMsg returns a user message containing the /clear marker, simulating the
// first message of a successor session after a /clear command.
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

// ingestAndChain sets up two sessions: a predecessor (no /clear) and a
// successor (starts with /clear marker), then runs IngestAll and returns the
// store plus the two session IDs.
func ingestChainPair(t *testing.T, predLastTS, succFirstTS, cwd string) (*Store, string, string) {
	t.Helper()
	projectDir := t.TempDir()

	predID := "pred-session-0001"
	succID := "succ-session-0002"

	writeJSONL(t, projectDir, "myproject", predID, []map[string]any{
		metaMsg("user", "Let's implement feature X.", "2026-04-10T10:00:00Z", cwd, "feature/x"),
		msg("assistant", "Sure, I'll start.", "2026-04-10T10:00:05Z"),
		msg("user", "Done?", predLastTS),
		msg("assistant", "Yes.", predLastTS),
	})

	writeJSONL(t, projectDir, "myproject", succID, []map[string]any{
		clearMsg(succFirstTS, cwd),
		msg("assistant", "Context cleared.", succFirstTS),
		msg("user", "Continue feature X.", succFirstTS),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	return s, predID, succID
}

// TestChainHighConfidence verifies that a gap < 2s produces a high-confidence link.
func TestChainHighConfidence(t *testing.T) {
	s, predID, succID := ingestChainPair(t,
		"2026-04-10T10:00:10Z",  // pred last message
		"2026-04-10T10:00:11Z",  // succ first message (1s gap)
		"/Users/dev/work/myrepo",
	)

	pred, err := s.Predecessor(succID)
	if err != nil {
		t.Fatal(err)
	}
	if pred != predID {
		t.Fatalf("expected predecessor %q, got %q", predID, pred)
	}

	succ, err := s.Successor(predID)
	if err != nil {
		t.Fatal(err)
	}
	if succ != succID {
		t.Fatalf("expected successor %q, got %q", succID, succ)
	}

	// Verify confidence in DB.
	var confidence string
	var gapMs int64
	err = s.db.QueryRow(
		"SELECT confidence, gap_ms FROM session_chains WHERE successor_id = ?", succID,
	).Scan(&confidence, &gapMs)
	if err != nil {
		t.Fatal(err)
	}
	if confidence != "high" {
		t.Errorf("expected high confidence, got %q", confidence)
	}
	if gapMs != 1000 {
		t.Errorf("expected gap_ms=1000, got %d", gapMs)
	}
}

// TestChainMediumConfidence verifies that a 2–5s gap produces a medium-confidence link.
func TestChainMediumConfidence(t *testing.T) {
	s, predID, succID := ingestChainPair(t,
		"2026-04-10T10:00:10Z",
		"2026-04-10T10:00:13Z", // 3s gap
		"/Users/dev/work/myrepo",
	)

	pred, err := s.Predecessor(succID)
	if err != nil {
		t.Fatal(err)
	}
	if pred != predID {
		t.Fatalf("expected predecessor %q, got %q", predID, pred)
	}

	var confidence string
	s.db.QueryRow(
		"SELECT confidence FROM session_chains WHERE successor_id = ?", succID,
	).Scan(&confidence)
	if confidence != "medium" {
		t.Errorf("expected medium confidence, got %q", confidence)
	}
}

// TestChainGapTooLarge verifies that a gap > 5s produces no link.
func TestChainGapTooLarge(t *testing.T) {
	s, _, succID := ingestChainPair(t,
		"2026-04-10T10:00:10Z",
		"2026-04-10T10:00:17Z", // 7s gap — over the 5s threshold
		"/Users/dev/work/myrepo",
	)

	pred, err := s.Predecessor(succID)
	if err != nil {
		t.Fatal(err)
	}
	if pred != "" {
		t.Errorf("expected no predecessor for large gap, got %q", pred)
	}
}

// TestChainNoPredecessor verifies no link when the session has no /clear marker.
func TestChainNoPredecessor(t *testing.T) {
	projectDir := t.TempDir()
	sessID := "solo-session-0001"
	writeJSONL(t, projectDir, "proj", sessID, []map[string]any{
		metaMsg("user", "Start fresh.", "2026-04-10T10:00:00Z", "/Users/dev/myrepo", ""),
		msg("assistant", "OK.", "2026-04-10T10:00:05Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	pred, err := s.Predecessor(sessID)
	if err != nil {
		t.Fatal(err)
	}
	if pred != "" {
		t.Errorf("expected no predecessor for solo session, got %q", pred)
	}
}

// TestChainPicksClosest verifies that when multiple candidates exist in the
// window, the one with the smallest gap is chosen.
func TestChainPicksClosest(t *testing.T) {
	projectDir := t.TempDir()
	cwd := "/Users/dev/work/myrepo"

	// Three sessions: far, close, target.
	farID := "far-session-001"
	closeID := "close-session-002"
	succID := "succ-session-003"

	writeJSONL(t, projectDir, "proj", farID, []map[string]any{
		metaMsg("user", "Farther session.", "2026-04-10T10:00:00Z", cwd, ""),
		msg("user", "end", "2026-04-10T10:00:06Z"), // 6s before succ
	})
	writeJSONL(t, projectDir, "proj", closeID, []map[string]any{
		metaMsg("user", "Closer session.", "2026-04-10T10:00:00Z", cwd, ""),
		msg("user", "end", "2026-04-10T10:00:09Z"), // 3s before succ
	})
	writeJSONL(t, projectDir, "proj", succID, []map[string]any{
		clearMsg("2026-04-10T10:00:12Z", cwd), // 3s after closeID's last, 6s after farID's last
		msg("user", "Continue.", "2026-04-10T10:00:12Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	pred, err := s.Predecessor(succID)
	if err != nil {
		t.Fatal(err)
	}
	if pred != closeID {
		t.Errorf("expected closest predecessor %q, got %q", closeID, pred)
	}
}

// TestChainTraversal verifies that Chain() returns the full ordered chain.
func TestChainTraversal(t *testing.T) {
	projectDir := t.TempDir()
	cwd := "/Users/dev/work/myrepo"

	id1 := "session-chain-001"
	id2 := "session-chain-002"
	id3 := "session-chain-003"

	writeJSONL(t, projectDir, "proj", id1, []map[string]any{
		metaMsg("user", "First session.", "2026-04-10T10:00:00Z", cwd, ""),
		msg("user", "end", "2026-04-10T10:00:10Z"),
	})
	writeJSONL(t, projectDir, "proj", id2, []map[string]any{
		clearMsg("2026-04-10T10:00:11Z", cwd),
		msg("user", "Second session.", "2026-04-10T10:00:11Z"),
		msg("user", "end", "2026-04-10T10:00:20Z"),
	})
	writeJSONL(t, projectDir, "proj", id3, []map[string]any{
		clearMsg("2026-04-10T10:00:21Z", cwd),
		msg("user", "Third session.", "2026-04-10T10:00:21Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Chain from the middle session should still return all three.
	chain, err := s.Chain(id2)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 3 {
		t.Fatalf("expected chain of 3, got %d: %v", len(chain), chain)
	}
	if chain[0].SessionID != id1 {
		t.Errorf("chain[0] expected %q, got %q", id1, chain[0].SessionID)
	}
	if chain[1].SessionID != id2 {
		t.Errorf("chain[1] expected %q, got %q", id2, chain[1].SessionID)
	}
	if chain[2].SessionID != id3 {
		t.Errorf("chain[2] expected %q, got %q", id3, chain[2].SessionID)
	}

	// chain[0] and chain[1] should have gap/confidence set (they connect forward).
	if chain[0].Confidence == "" {
		t.Error("chain[0] should have confidence set (it links to chain[1])")
	}
	if chain[1].Confidence == "" {
		t.Error("chain[1] should have confidence set (it links to chain[2])")
	}
	// chain[2] is the tail — no forward link, so Confidence should be "".
	if chain[2].Confidence != "" {
		t.Errorf("chain[2] tail should have empty confidence, got %q", chain[2].Confidence)
	}
}

// TestPredecessorSuccessorNavigation verifies basic prev/next navigation.
func TestPredecessorSuccessorNavigation(t *testing.T) {
	s, predID, succID := ingestChainPair(t,
		"2026-04-10T10:00:10Z",
		"2026-04-10T10:00:11Z",
		"/Users/dev/work/nav-test",
	)

	// Forward from pred.
	succ, err := s.Successor(predID)
	if err != nil {
		t.Fatal(err)
	}
	if succ != succID {
		t.Errorf("expected successor %q, got %q", succID, succ)
	}

	// Backward from succ.
	pred, err := s.Predecessor(succID)
	if err != nil {
		t.Fatal(err)
	}
	if pred != predID {
		t.Errorf("expected predecessor %q, got %q", predID, pred)
	}

	// Successor of tail should be "".
	tailSucc, err := s.Successor(succID)
	if err != nil {
		t.Fatal(err)
	}
	if tailSucc != "" {
		t.Errorf("expected no successor of tail, got %q", tailSucc)
	}

	// Predecessor of head should be "".
	headPred, err := s.Predecessor(predID)
	if err != nil {
		t.Fatal(err)
	}
	if headPred != "" {
		t.Errorf("expected no predecessor of head, got %q", headPred)
	}
}

// TestChainSingleSession verifies that Chain() on a session with no links
// returns a one-element slice.
func TestChainSingleSession(t *testing.T) {
	projectDir := t.TempDir()
	sessID := "solo-session-0001"
	writeJSONL(t, projectDir, "proj", sessID, []map[string]any{
		metaMsg("user", "Standalone session.", "2026-04-10T10:00:00Z", "/Users/dev/myrepo", ""),
		msg("assistant", "OK.", "2026-04-10T10:00:05Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	chain, err := s.Chain(sessID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected single-element chain, got %d elements", len(chain))
	}
	if chain[0].SessionID != sessID {
		t.Errorf("expected %q, got %q", sessID, chain[0].SessionID)
	}
}
