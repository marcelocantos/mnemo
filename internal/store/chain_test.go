// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

// recordChainPair populates a definitive chain (pred → succ) via the
// connection-observer path: one connection records two session
// bindings, in order. The RecordConnectionSession observer writes the
// session_chains row as a side effect. Returns the store for follow-
// up queries.
func recordChainPair(t *testing.T, predID, succID string, predAt, succAt time.Time) *Store {
	t.Helper()
	s := newTestStore(t, t.TempDir())
	s.RecordConnectionSessionAt("conn-A", predID, predAt)
	s.RecordConnectionSessionAt("conn-A", succID, succAt)
	return s
}

// TestDefinitiveChainFromConnection verifies that a pair of session
// bindings on the same connection produces a session_chains row with
// mechanism='mcp_connection' and confidence='definitive'.
func TestDefinitiveChainFromConnection(t *testing.T) {
	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	s := recordChainPair(t, "sess-pred", "sess-succ", t0, t0.Add(5*time.Minute))

	pred, err := s.Predecessor("sess-succ")
	if err != nil {
		t.Fatal(err)
	}
	if pred != "sess-pred" {
		t.Fatalf("expected predecessor %q, got %q", "sess-pred", pred)
	}

	var mechanism, confidence string
	if err := s.db.QueryRow(
		"SELECT mechanism, confidence FROM session_chains WHERE successor_id = ?",
		"sess-succ").Scan(&mechanism, &confidence); err != nil {
		t.Fatal(err)
	}
	if mechanism != "mcp_connection" {
		t.Errorf("mechanism: got %q, want %q", mechanism, "mcp_connection")
	}
	if confidence != "definitive" {
		t.Errorf("confidence: got %q, want %q", confidence, "definitive")
	}
}

// TestDefinitiveChainIdempotent verifies that repeat bindings on the
// same (connection, session) do not overwrite or duplicate the chain
// row — the initial chain row stands.
func TestDefinitiveChainIdempotent(t *testing.T) {
	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	s := recordChainPair(t, "sess-pred", "sess-succ", t0, t0.Add(5*time.Minute))

	// Re-record the successor binding several times.
	s.RecordConnectionSessionAt("conn-A", "sess-succ", t0.Add(10*time.Minute))
	s.RecordConnectionSessionAt("conn-A", "sess-succ", t0.Add(15*time.Minute))

	var count int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM session_chains WHERE successor_id = ?",
		"sess-succ").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 chain row, got %d", count)
	}
}

// TestDefinitiveChainNoSpuriousLinkOnFirstSession verifies that the
// first session recorded on a fresh connection does not get a spurious
// predecessor.
func TestDefinitiveChainNoSpuriousLinkOnFirstSession(t *testing.T) {
	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	s := newTestStore(t, t.TempDir())
	s.RecordConnectionSessionAt("conn-A", "sess-first", t0)

	pred, err := s.Predecessor("sess-first")
	if err != nil {
		t.Fatal(err)
	}
	if pred != "" {
		t.Errorf("first session on a connection should have no predecessor, got %q", pred)
	}
}

// TestDefinitiveChainTraversal verifies that Chain() returns the full
// ordered chain when a single connection has recorded three sessions
// (two /clear events).
func TestDefinitiveChainTraversal(t *testing.T) {
	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	s := newTestStore(t, t.TempDir())
	s.RecordConnectionSessionAt("conn-A", "sess-1", t0)
	s.RecordConnectionSessionAt("conn-A", "sess-2", t0.Add(10*time.Minute))
	s.RecordConnectionSessionAt("conn-A", "sess-3", t0.Add(20*time.Minute))

	chain, err := s.Chain("sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 3 {
		t.Fatalf("expected chain of 3, got %d: %+v", len(chain), chain)
	}
	want := []string{"sess-1", "sess-2", "sess-3"}
	for i, w := range want {
		if chain[i].SessionID != w {
			t.Errorf("chain[%d]: got %q, want %q", i, chain[i].SessionID, w)
		}
	}
}

// TestPredecessorSuccessorNavigation checks that Predecessor and
// Successor navigate correctly across a recorded chain.
func TestPredecessorSuccessorNavigation(t *testing.T) {
	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	s := recordChainPair(t, "sess-pred", "sess-succ", t0, t0.Add(5*time.Minute))

	if succ, _ := s.Successor("sess-pred"); succ != "sess-succ" {
		t.Errorf("Successor(sess-pred): got %q, want sess-succ", succ)
	}
	if pred, _ := s.Predecessor("sess-succ"); pred != "sess-pred" {
		t.Errorf("Predecessor(sess-succ): got %q, want sess-pred", pred)
	}
	if succ, _ := s.Successor("sess-succ"); succ != "" {
		t.Errorf("Successor(tail): got %q, want empty", succ)
	}
	if pred, _ := s.Predecessor("sess-pred"); pred != "" {
		t.Errorf("Predecessor(head): got %q, want empty", pred)
	}
}

// TestChainSingleSession verifies Chain() returns a one-element slice
// for a session with no links.
func TestChainSingleSession(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.RecordConnectionSessionAt("conn-A", "sess-solo", time.Now())

	chain, err := s.Chain("sess-solo")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected 1 element, got %d", len(chain))
	}
	if chain[0].SessionID != "sess-solo" {
		t.Errorf("got %q, want sess-solo", chain[0].SessionID)
	}
}

// TestMultiConnectionSameSession captures the ctrl-c + claude --continue
// recovery case: session-A is observed on two connections (pre- and
// post-restart). The chain from any subsequent /clear reflects only
// the connection that observed the transition, not the other.
func TestMultiConnectionSameSession(t *testing.T) {
	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	s := newTestStore(t, t.TempDir())

	// Connection C1 sees A then B (a /clear).
	s.RecordConnectionSessionAt("C1", "A", t0)
	s.RecordConnectionSessionAt("C1", "B", t0.Add(5*time.Minute))
	// User ctrl-c; claude --continue reopens session B under a new
	// connection C2.
	s.RecordConnectionSessionAt("C2", "B", t0.Add(30*time.Minute))
	// No /clear happens on C2 — B is still the only session there.

	// Chain A → B should exist, with no duplicates from C2's
	// observation of B.
	pred, _ := s.Predecessor("B")
	if pred != "A" {
		t.Errorf("Predecessor(B): got %q, want A", pred)
	}
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM session_chains WHERE successor_id = ?", "B").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 chain row for B, got %d", count)
	}
}
