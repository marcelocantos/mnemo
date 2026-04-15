// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
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
