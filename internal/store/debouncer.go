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
// Safe for concurrent use from multiple goroutines.
type debouncer struct {
	mu      sync.Mutex
	timers  map[string]*time.Timer
	window  time.Duration
	counter int64 // counts actual callback invocations; used in tests
}

func newDebouncer(window time.Duration) *debouncer {
	return &debouncer{
		timers: make(map[string]*time.Timer),
		window: window,
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
