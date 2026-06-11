// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"sync"
	"time"
)

// debouncer coalesces rapid successive calls for the same key into a single
// deferred invocation. Each call to enqueue resets the timer for that key,
// so only the trailing edge of a burst fires the callback.
//
// When constructed with newDebouncerWithConcurrency, at most maxConcurrent
// callbacks run concurrently — additional goroutines block until a slot is
// free, providing backpressure against ingest bursts.
//
// Safe for concurrent use from multiple goroutines.
type debouncer struct {
	mu      sync.Mutex
	timers  map[string]*time.Timer
	window  time.Duration
	counter int64         // counts actual callback invocations; used in tests
	sem     chan struct{} // nil = unbounded concurrency
}

func newDebouncer(window time.Duration) *debouncer {
	return &debouncer{
		timers: make(map[string]*time.Timer),
		window: window,
	}
}

// newDebouncerWithConcurrency is like newDebouncer but caps the number of
// callbacks that run concurrently. Use this when callbacks do heavy I/O
// (e.g., file read + JSON parse + SQLite write) and unbounded parallelism
// would spike CPU/memory under large event bursts.
func newDebouncerWithConcurrency(window time.Duration, maxConcurrent int) *debouncer {
	return &debouncer{
		timers: make(map[string]*time.Timer),
		window: window,
		sem:    make(chan struct{}, maxConcurrent),
	}
}

// enqueue schedules fn to be called after the debounce window expires for key.
// If enqueue is called again for the same key before the window expires, the
// previous timer is cancelled and a new one starts — coalescing burst events
// into a single trailing-edge call.
func (d *debouncer) enqueue(key string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if t, ok := d.timers[key]; ok {
		t.Stop()
	}
	d.timers[key] = time.AfterFunc(d.window, func() {
		if d.sem != nil {
			d.sem <- struct{}{}
			defer func() { <-d.sem }()
		}
		fn()

		d.mu.Lock()
		delete(d.timers, key)
		d.counter++
		d.mu.Unlock()
	})
}

// invocations returns the number of callbacks that have fired.
// Intended for testing only.
func (d *debouncer) invocations() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counter
}
