// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLeaseExclusiveHoldAndHandoff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "background.lease")

	var clock time.Time
	now := func() time.Time { return clock }
	clock = time.Unix(1_700_000_000, 0)

	a, err := NewLease(&LeaseArgs{Path: path, HolderID: "a", TTL: time.Second, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewLease(&LeaseArgs{Path: path, HolderID: "b", TTL: time.Second, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := a.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("a acquire: ok=%v err=%v", ok, err)
	}
	a.SetRunningBackground(true)
	if !a.Held() || !a.RunningBackground() {
		t.Fatal("a should hold and run bg")
	}

	ok, err = b.TryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	if ok || b.Held() {
		t.Fatal("b must not acquire while a holds live lease")
	}
	if b.RunningBackground() {
		t.Fatal("b must not run background")
	}

	// Crash a: stop heartbeats and advance clock past TTL.
	a.SetRunningBackground(false)
	// Simulate crash without clean Release — just forget hold in memory
	// but leave stale file; advance clock.
	clock = clock.Add(2 * time.Second)

	ok, err = b.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("b steal after expiry: ok=%v err=%v", ok, err)
	}
	if !b.Held() {
		t.Fatal("b should hold")
	}
	// a still thinks it holds until heartbeat fails
	_ = a.Heartbeat() // should notice lost ownership if file rewritten
	// After b wrote, a's heartbeat sees different holder.
	if a.Held() {
		// Heartbeat should have cleared held when holder mismatch
		t.Fatal("a should no longer hold after b stole")
	}

	// Clean handoff: b releases, a reacquires quickly.
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Millisecond)
	ok, err = a.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("a reacquire: ok=%v err=%v", ok, err)
	}
	st := a.Status()
	if !st.HeldLocally || st.FileHolder != "a" {
		t.Fatalf("status: %+v", st)
	}
}

func TestLeaseReleaseAllowsOther(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "l")
	clock := time.Unix(1_000, 0)
	now := func() time.Time { return clock }
	a, _ := NewLease(&LeaseArgs{Path: path, HolderID: "a", TTL: time.Second, Now: now})
	b, _ := NewLease(&LeaseArgs{Path: path, HolderID: "b", TTL: time.Second, Now: now})
	if ok, _ := a.TryAcquire(); !ok {
		t.Fatal("a")
	}
	_ = a.Release()
	if ok, _ := b.TryAcquire(); !ok {
		t.Fatal("b after release")
	}
	if a.Held() {
		t.Fatal("a released")
	}
}
