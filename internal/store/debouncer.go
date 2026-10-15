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
	// fired holds keys whose callback is waiting for a slot or running.
	// A timer that fires for such a key marks it dirty instead of
	// starting another goroutine; the running callback re-arms once.
	// Without this, every append during an ingest stall parked one more
	// goroutine on sem (64k in a 4-minute run on a copy of the store).
	fired   map[string]bool
	dirty   map[string]bool
	fns     map[string]func()
	stopped bool
}

func newDebouncer(window time.Duration) *debouncer {
	return &debouncer{
		timers: make(map[string]*time.Timer),
		window: window,
		fired:  make(map[string]bool),
		dirty:  make(map[string]bool),
		fns:    make(map[string]func()),
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
		fired:  make(map[string]bool),
		dirty:  make(map[string]bool),
		fns:    make(map[string]func()),
	}
}

// enqueue schedules fn to be called after the debounce window expires for key.
// If enqueue is called again for the same key before the window expires, the
// previous timer is cancelled and a new one starts — coalescing burst events
// into a single trailing-edge call.
func (d *debouncer) enqueue(key string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.fns[key] = fn // the latest fn wins, as with a reset timer
	if d.fired[key] {
		// A callback for key is queued or running; it reruns once.
		d.dirty[key] = true
		return
	}
	d.armLocked(key)
}

func (d *debouncer) armLocked(key string) {
	if t, ok := d.timers[key]; ok {
		t.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(d.window, func() {
		d.mu.Lock()
		if d.timers[key] != t || d.stopped {
			d.mu.Unlock()
			return // superseded or stopped
		}
		delete(d.timers, key)
		d.fired[key] = true
		d.mu.Unlock()
		d.run(key)
	})
	d.timers[key] = t
}

func (d *debouncer) run(key string) {
	for {
		if d.sem != nil {
			d.sem <- struct{}{}
		}
		d.mu.Lock()
		fn := d.fns[key]
		d.mu.Unlock()
		if fn != nil {
			fn()
		}
		if d.sem != nil {
			<-d.sem
		}
		d.mu.Lock()
		d.counter++
		if d.dirty[key] && !d.stopped {
			delete(d.dirty, key)
			d.mu.Unlock()
			continue
		}
		delete(d.dirty, key)
		delete(d.fired, key)
		delete(d.fns, key)
		d.mu.Unlock()
		return
	}
}

// stop cancels every pending timer so no debounced callback fires after the
// caller has begun shutting down (🎯T97.1). A callback already executing is
// unaffected; this only prevents not-yet-fired ingest work from racing the
// store's Close/checkpoint. Safe to call once on the watcher's exit path.
func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	for key, t := range d.timers {
		t.Stop()
		delete(d.timers, key)
	}
	for key := range d.dirty {
		delete(d.dirty, key)
	}
}

// invocations returns the number of callbacks that have fired.
// Intended for testing only.
func (d *debouncer) invocations() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counter
}
