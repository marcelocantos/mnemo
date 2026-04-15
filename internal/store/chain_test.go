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

// TestChainLinksImmediateSuccessor verifies that a /clear directly after
// the predecessor's last message links the two sessions with gap_ms
// recorded.
func TestChainLinksImmediateSuccessor(t *testing.T) {
	s, predID, succID := ingestChainPair(t,
		"2026-04-10T10:00:10Z", // pred last message
		"2026-04-10T10:00:11Z", // succ first message (1s gap)
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

	var confidence string
	var gapMs int64
	if err := s.db.QueryRow(
		"SELECT confidence, gap_ms FROM session_chains WHERE successor_id = ?", succID,
	).Scan(&confidence, &gapMs); err != nil {
		t.Fatal(err)
	}
	if confidence != "high" {
		t.Errorf("expected high confidence, got %q", confidence)
	}
	if gapMs != 1000 {
		t.Errorf("expected gap_ms=1000, got %d", gapMs)
	}
}

// TestChainLinksAcrossLongGap verifies the no-window invariant: a
// predecessor that ended hours or days before the /clear still links,
// as long as it is the most recent session in the same cwd. The old
// 5-second window mistakenly dropped these legitimate chains.
func TestChainLinksAcrossLongGap(t *testing.T) {
	s, predID, succID := ingestChainPair(t,
		"2026-04-10T10:00:10Z", // pred last message
		"2026-04-12T15:30:00Z", // succ first message (~2 days later)
		"/Users/dev/work/myrepo",
	)

	pred, err := s.Predecessor(succID)
	if err != nil {
		t.Fatal(err)
	}
	if pred != predID {
		t.Fatalf("expected predecessor %q, got %q (chain must survive long gaps)", predID, pred)
	}

	var gapMs int64
	if err := s.db.QueryRow(
		"SELECT gap_ms FROM session_chains WHERE successor_id = ?", succID,
	).Scan(&gapMs); err != nil {
		t.Fatal(err)
	}
	if gapMs < 2*24*3600*1000 {
		t.Errorf("expected gap_ms to reflect ~2 day delay, got %d", gapMs)
	}
}

// TestChainRequiresSameCwd verifies that a /clear in one repo does not
// reach back to a session in a different cwd, even if it is the most
// recent overall.
func TestChainRequiresSameCwd(t *testing.T) {
	projectDir := t.TempDir()

	// Predecessor lives in cwd A.
	writeJSONL(t, projectDir, "projA", "pred-A", []map[string]any{
		metaMsg("user", "Work in repo A", "2026-04-10T10:00:00Z",
			"/Users/dev/work/repoA", "feat/x"),
		msg("assistant", "OK", "2026-04-10T10:00:05Z"),
	})
	// Successor opens a /clear in cwd B. No predecessor exists in B.
	writeJSONL(t, projectDir, "projB", "succ-B", []map[string]any{
		metaMsg("user", "<command-name>/clear</command-name>", "2026-04-10T10:00:10Z",
			"/Users/dev/work/repoB", "main"),
		msg("assistant", "Context cleared.", "2026-04-10T10:00:11Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	pred, err := s.Predecessor("succ-B")
	if err != nil {
		t.Fatal(err)
	}
	if pred != "" {
		t.Errorf("expected no predecessor (cross-cwd must not link), got %q", pred)
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
