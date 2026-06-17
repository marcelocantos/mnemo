// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package breaker

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

func TestTripsAfterThreshold(t *testing.T) {
	b := New(3, time.Minute)
	for i := 0; i < 2; i++ {
		if !b.Allow(t0) {
			t.Fatalf("attempt %d should be allowed before tripping", i)
		}
		b.Record(t0, false, "boom")
	}
	// Still closed after 2 of 3 failures.
	if s := b.Snapshot(); s.Open {
		t.Fatalf("breaker tripped early: %+v", s)
	}
	if !b.Allow(t0) {
		t.Fatal("third attempt should be allowed")
	}
	b.Record(t0, false, "boom") // third failure trips it
	s := b.Snapshot()
	if !s.Open || s.State != StateOpen || s.TripCount != 1 || s.LastError != "boom" {
		t.Fatalf("expected tripped open: %+v", s)
	}
}

func TestSkipsDuringCooldownThenHalfOpens(t *testing.T) {
	b := New(1, time.Minute)
	b.Record(t0, false, "x") // trips immediately (threshold 1)

	// Within cooldown: skipped.
	if b.Allow(t0.Add(30 * time.Second)) {
		t.Fatal("should skip within cooldown")
	}
	// After cooldown: half-open, one trial allowed.
	if !b.Allow(t0.Add(90 * time.Second)) {
		t.Fatal("should allow a trial after cooldown")
	}
	if s := b.Snapshot(); s.State != StateHalfOpen {
		t.Fatalf("expected half-open: %+v", s)
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b := New(1, time.Minute)
	b.Record(t0, false, "x")             // open
	_ = b.Allow(t0.Add(2 * time.Minute)) // → half-open
	b.Record(t0.Add(2*time.Minute), false, "again")
	s := b.Snapshot()
	if !s.Open || s.TripCount != 2 {
		t.Fatalf("half-open failure should re-open with a new trip: %+v", s)
	}
	// Cooldown restarts from the re-open time.
	if b.Allow(t0.Add(2*time.Minute + 30*time.Second)) {
		t.Fatal("should skip within the new cooldown")
	}
}

func TestHalfOpenSuccessCloses(t *testing.T) {
	b := New(2, time.Minute)
	b.Record(t0, false, "x")
	b.Record(t0, false, "x")                  // open
	_ = b.Allow(t0.Add(2 * time.Minute))      // → half-open
	b.Record(t0.Add(2*time.Minute), true, "") // trial succeeds → closed
	s := b.Snapshot()
	if s.Open || s.State != StateClosed || s.Failures != 0 {
		t.Fatalf("success should close and reset: %+v", s)
	}
}

func TestSuccessResetsFailureRun(t *testing.T) {
	b := New(3, time.Minute)
	b.Record(t0, false, "x")
	b.Record(t0, false, "x")
	b.Record(t0, true, "") // reset
	b.Record(t0, false, "x")
	if s := b.Snapshot(); s.Open || s.Failures != 1 {
		t.Fatalf("success should have reset the failure run: %+v", s)
	}
}
