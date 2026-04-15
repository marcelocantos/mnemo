// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

// TestConnectionLifecycle verifies that RecordConnectionOpen and
// RecordConnectionClose persist rows and that OpenConnections returns
// only the still-open ones.
func TestConnectionLifecycle(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.RecordConnectionOpen("conn-a", 4242, now)
	s.RecordConnectionOpen("conn-b", 5353, now.Add(time.Second))

	open, err := s.OpenConnections()
	if err != nil {
		t.Fatalf("OpenConnections: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("expected 2 open connections, got %d: %+v", len(open), open)
	}
	if open[0].ConnectionID != "conn-a" || open[0].PID != 4242 {
		t.Errorf("first row unexpected: %+v", open[0])
	}
	if open[1].ConnectionID != "conn-b" || open[1].PID != 5353 {
		t.Errorf("second row unexpected: %+v", open[1])
	}

	// Close one; only the other should remain open.
	s.RecordConnectionClose("conn-a", now.Add(5*time.Second))
	open, err = s.OpenConnections()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 open connection after close, got %d", len(open))
	}
	if open[0].ConnectionID != "conn-b" {
		t.Errorf("wrong connection remained open: %+v", open[0])
	}

	// Close the other and confirm neither is open.
	s.RecordConnectionClose("conn-b", now.Add(6*time.Second))
	open, err = s.OpenConnections()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("expected no open connections, got %+v", open)
	}
}

// TestConnectionSessionBinding verifies the upsert semantics of
// RecordConnectionSession: first-seen is preserved, last-seen is
// bumped on repeat observations, and SessionsForConnection /
// ConnectionsForSession both reflect the recorded bindings.
func TestConnectionSessionBinding(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	t0 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)
	t2 := t0.Add(30 * time.Minute)

	// Connection C1 sees session A, then session B (after a /clear).
	// Connection C2 sees session A too (cross-connection recovery,
	// e.g. ctrl-c + claude --continue reopening the same session in
	// a new proxy process).
	s.RecordConnectionSessionAt("C1", "A", t0)
	s.RecordConnectionSessionAt("C1", "A", t1) // repeat bumps last_seen
	s.RecordConnectionSessionAt("C1", "B", t2)
	s.RecordConnectionSessionAt("C2", "A", t2.Add(time.Hour))

	// All sessions for C1, oldest first.
	c1Sessions, err := s.SessionsForConnection("C1")
	if err != nil {
		t.Fatal(err)
	}
	if len(c1Sessions) != 2 {
		t.Fatalf("C1 should have 2 sessions, got %d", len(c1Sessions))
	}
	if c1Sessions[0].SessionID != "A" {
		t.Errorf("first session should be A, got %q", c1Sessions[0].SessionID)
	}
	if c1Sessions[1].SessionID != "B" {
		t.Errorf("second session should be B, got %q", c1Sessions[1].SessionID)
	}
	if !c1Sessions[0].FirstSeenAt.Equal(t0) {
		t.Errorf("C1/A first_seen should equal t0, got %v", c1Sessions[0].FirstSeenAt)
	}
	if !c1Sessions[0].LastSeenAt.Equal(t1) {
		t.Errorf("C1/A last_seen should bump to t1, got %v", c1Sessions[0].LastSeenAt)
	}

	// All connections that saw session A (both C1 and C2).
	aConns, err := s.ConnectionsForSession("A")
	if err != nil {
		t.Fatal(err)
	}
	if len(aConns) != 2 {
		t.Fatalf("session A should have 2 owning connections, got %d", len(aConns))
	}
	if aConns[0].ConnectionID != "C1" || aConns[1].ConnectionID != "C2" {
		t.Errorf("wrong owners for A: %+v", aConns)
	}
}

// TestConnectionSessionEmptyInputs verifies that empty connection_id
// or session_id is silently ignored — the binding recorder is called
// from hot paths (every tool call) and must never fail loudly.
func TestConnectionSessionEmptyInputs(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.RecordConnectionSession("", "A")  // ignored
	s.RecordConnectionSession("C1", "") // ignored
	got, err := s.SessionsForConnection("C1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty inputs should record nothing, got %+v", got)
	}
}

// TestConnectionCloseIdempotent verifies that closing an already-
// closed or unknown connection does not error.
func TestConnectionCloseIdempotent(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now().UTC()
	s.RecordConnectionClose("never-opened", now) // no-op, silent
	s.RecordConnectionOpen("conn-x", 1, now)
	s.RecordConnectionClose("conn-x", now.Add(time.Second))
	s.RecordConnectionClose("conn-x", now.Add(2*time.Second)) // second close is also a no-op
	open, err := s.OpenConnections()
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("expected no open, got %+v", open)
	}
}
