// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"sync"
	"time"
)

// DefaultPollMaxPerTick bounds how many paths one poll cycle may select.
const DefaultPollMaxPerTick = 500

// PollTracker records hot paths so safety poll never walks the cold archive.
//
// Candidates are drawn only from:
//   - live paths (caller-supplied, e.g. lsof-open transcripts),
//   - paths that recently received a tree event (NoteEvent),
//   - paths previously observed incomplete (size > offset).
//
// Fully-ingested cold paths are never re-stated every tick.
type PollTracker struct {
	mu          sync.Mutex
	recent      map[string]time.Time
	incomplete  map[string]struct{}
	recentFor   time.Duration
	maxPerTick  int
	statCalls   int // test counter
	pathsStated map[string]int
}

// NewPollTracker returns a tracker with the given recent window and max paths
// per tick. Zero values use 2 minutes and DefaultPollMaxPerTick.
func NewPollTracker(recentFor time.Duration, maxPerTick int) *PollTracker {
	if recentFor <= 0 {
		recentFor = 2 * time.Minute
	}
	if maxPerTick <= 0 {
		maxPerTick = DefaultPollMaxPerTick
	}
	return &PollTracker{
		recent:      make(map[string]time.Time),
		incomplete:  make(map[string]struct{}),
		recentFor:   recentFor,
		maxPerTick:  maxPerTick,
		pathsStated: make(map[string]int),
	}
}

// NoteEvent marks path as recently active (tree event or successful partial ingest).
func (t *PollTracker) NoteEvent(path string, now time.Time) {
	if path == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.IsZero() {
		now = time.Now()
	}
	t.recent[path] = now
}

// NoteIncomplete records that path was observed with size > offset.
func (t *PollTracker) NoteIncomplete(path string) {
	if path == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.incomplete[path] = struct{}{}
}

// ClearIncomplete drops path from the incomplete set after catch-up.
func (t *PollTracker) ClearIncomplete(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.incomplete, path)
}

// StatFunc returns current size for path (and optional error).
type StatFunc func(path string) (size int64, err error)

// CandidatesArgs is the input to PollTracker.Candidates.
type CandidatesArgs struct {
	// Offsets maps path → last ingested byte offset.
	Offsets map[string]int64
	// Live paths that should always be considered (session still open).
	Live []string
	// Now is the clock (tests inject).
	Now time.Time
	// Stat is called only for hot-set paths, never for arbitrary cold offsets.
	Stat StatFunc
}

// Candidates returns paths whose size exceeds offset (need re-ingest), stating
// only the hot set. Cold fully-ingested paths in Offsets are not stated.
func (t *PollTracker) Candidates(args CandidatesArgs) []string {
	if args.Stat == nil {
		return nil
	}
	now := args.Now
	if now.IsZero() {
		now = time.Now()
	}

	t.mu.Lock()
	// Expire recent.
	for p, ts := range t.recent {
		if now.Sub(ts) > t.recentFor {
			delete(t.recent, p)
		}
	}
	// Build hot set: live ∪ recent ∪ incomplete (must also appear in offsets or live).
	hot := make(map[string]struct{})
	for _, p := range args.Live {
		if p != "" {
			hot[p] = struct{}{}
		}
	}
	for p := range t.recent {
		hot[p] = struct{}{}
	}
	for p := range t.incomplete {
		hot[p] = struct{}{}
	}
	max := t.maxPerTick
	t.mu.Unlock()

	out := make([]string, 0, len(hot))
	for p := range hot {
		if len(out) >= max {
			break
		}
		off, known := args.Offsets[p]
		if !known {
			// Live/recent path not yet in offsets — still check size > 0.
			off = 0
		}
		t.mu.Lock()
		t.statCalls++
		t.pathsStated[p]++
		t.mu.Unlock()

		size, err := args.Stat(p)
		if err != nil {
			t.mu.Lock()
			delete(t.incomplete, p)
			t.mu.Unlock()
			continue
		}
		if size > off {
			out = append(out, p)
			t.NoteIncomplete(p)
		} else {
			t.ClearIncomplete(p)
		}
	}
	return out
}

// StatCallCount returns how many Stat invocations have occurred (tests).
func (t *PollTracker) StatCallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.statCalls
}

// Stated reports whether path was ever stated (tests).
func (t *PollTracker) Stated(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pathsStated[path] > 0
}
