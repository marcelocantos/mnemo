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
