// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestDebouncerCoalesceBurst verifies that 100 rapid enqueue calls for the
// same key result in exactly one callback invocation.
func TestDebouncerCoalesceBurst(t *testing.T) {
	const window = 50 * time.Millisecond
	d := newDebouncer(window)

	var calls atomic.Int64
	for i := 0; i < 100; i++ {
		d.enqueue("key", func() { calls.Add(1) })
	}

	// Wait for the debounce window to expire, with headroom.
	time.Sleep(window * 5)

	if got := calls.Load(); got != 1 {
		t.Errorf("burst of 100 enqueues: want 1 callback invocation, got %d", got)
	}
	if got := d.invocations(); got != 1 {
		t.Errorf("debouncer.invocations(): want 1, got %d", got)
	}
}

// TestDebouncerDistinctKeys verifies that independent keys each fire once.
func TestDebouncerDistinctKeys(t *testing.T) {
	const window = 50 * time.Millisecond
	d := newDebouncer(window)

	var calls atomic.Int64
	const n = 5
	for i := 0; i < n; i++ {
		key := string(rune('A' + i))
		d.enqueue(key, func() { calls.Add(1) })
	}

	time.Sleep(window * 5)

	if got := calls.Load(); got != n {
		t.Errorf("distinct keys: want %d callback invocations, got %d", n, got)
	}
}

// TestDebouncerTrailingEdge verifies that a second burst after a quiet period
// fires a second callback — i.e. the debouncer rearms correctly.
func TestDebouncerTrailingEdge(t *testing.T) {
	const window = 50 * time.Millisecond
	d := newDebouncer(window)

	var calls atomic.Int64
	fn := func() { calls.Add(1) }

	// First burst.
	for i := 0; i < 10; i++ {
		d.enqueue("key", fn)
	}
	time.Sleep(window * 5)
	if got := calls.Load(); got != 1 {
		t.Errorf("first burst: want 1, got %d", got)
	}

	// Second burst after quiet.
	for i := 0; i < 10; i++ {
		d.enqueue("key", fn)
	}
	time.Sleep(window * 5)
	if got := calls.Load(); got != 2 {
		t.Errorf("after second burst: want 2, got %d", got)
	}
}

// While a key's callback is stalled (ingest blocked on the writer),
// further appends must not park a goroutine each. Before the fix every
// fired timer waited on the semaphore: a 4-minute append storm on a
// copy of the store parked 64k goroutines.
func TestDebouncerStalledCallbackDoesNotPileUp(t *testing.T) {
	d := newDebouncerWithConcurrency(time.Millisecond, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	fn := func() {
		if calls.Add(1) == 1 {
			<-release
		}
	}
	base := runtime.NumGoroutine()
	d.enqueue("file-a", fn)
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 2000; i++ {
		d.enqueue("file-a", fn)
		if i%100 == 0 {
			time.Sleep(2 * time.Millisecond) // let timers fire
		}
	}
	time.Sleep(20 * time.Millisecond)
	if g := runtime.NumGoroutine() - base; g > 5 {
		t.Fatalf("goroutines grew by %d while the callback was stalled, want <= 5", g)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for d.invocations() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("callback ran %d times, want 2 (the stalled one plus one coalesced rerun)", n)
	}
}
