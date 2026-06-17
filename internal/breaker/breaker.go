// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package breaker provides a circuit breaker for background tasks that
// can fail systemically (🎯T84).
//
// mnemo runs several long-lived workers (the compaction watcher, the
// stream reconcilers, the CLAUDE.md reviewer). When one fails for a
// reason that recurs on every attempt — a missing working directory,
// claude absent from PATH, a wedged database — retrying hot wastes CPU
// and contends shared resources (the single SQLite writer), starving the
// other workers. The compaction quarantine (🎯T77) handles this
// per-*item*; this breaker handles it per-*task*.
//
// A task consults Allow before each attempt and folds the outcome back
// with Record. After Threshold consecutive failures the breaker trips
// Open: Allow returns false until Cooldown elapses, at which point a
// single HalfOpen trial is permitted. A success closes the breaker; a
// failed trial re-opens it with a fresh cooldown. The breaker is
// safe for concurrent use and exposes a Snapshot for the diagnostics
// suite (🎯T83): a tripped breaker is a fail-severity health signal.
package breaker

import (
	"sync"
	"time"
)

// State is the breaker's current position.
type State string

const (
	StateClosed   State = "closed"    // normal operation
	StateOpen     State = "open"      // tripped; attempts skipped during cooldown
	StateHalfOpen State = "half_open" // cooldown elapsed; one trial permitted
)

// Breaker is a single circuit breaker. Construct with New.
type Breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration

	state     State
	failures  int       // consecutive failures while closed
	openedAt  time.Time // when the breaker last tripped open
	lastErr   string    // most recent failure message
	tripCount int       // lifetime trips, for diagnostics
}

// New returns a closed breaker that trips after threshold consecutive
// failures and stays open for cooldown before allowing a trial. A
// threshold below 1 is clamped to 1.
func New(threshold int, cooldown time.Duration) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, state: StateClosed}
}

// Allow reports whether the task should attempt work at time now. While
// open and within the cooldown it returns false so the caller can skip
// the attempt (and back off). Once the cooldown has elapsed it
// transitions to half-open and returns true to permit a single trial.
func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen {
		if now.Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = StateHalfOpen
	}
	return true
}

// Record folds an attempt's outcome into the breaker. errMsg is retained
// for diagnostics when success is false; it is ignored on success.
func (b *Breaker) Record(now time.Time, success bool, errMsg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if success {
		b.failures = 0
		b.state = StateClosed
		b.lastErr = ""
		return
	}
	b.lastErr = errMsg
	if b.state == StateHalfOpen {
		// The trial failed — re-open with a fresh cooldown.
		b.state = StateOpen
		b.openedAt = now
		b.tripCount++
		return
	}
	b.failures++
	if b.state == StateClosed && b.failures >= b.threshold {
		b.state = StateOpen
		b.openedAt = now
		b.tripCount++
	}
}

// Snapshot is an immutable view of breaker state for diagnostics.
type Snapshot struct {
	State     State     `json:"state"`
	Open      bool      `json:"open"`
	Failures  int       `json:"consecutive_failures"`
	TripCount int       `json:"lifetime_trips"`
	LastError string    `json:"last_error,omitempty"`
	OpenedAt  time.Time `json:"opened_at,omitempty"`
}

// Snapshot returns the current breaker state.
func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{
		State:     b.state,
		Open:      b.state == StateOpen,
		Failures:  b.failures,
		TripCount: b.tripCount,
		LastError: b.lastErr,
		OpenedAt:  b.openedAt,
	}
}
