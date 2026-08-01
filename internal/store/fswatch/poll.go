// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultPollMaxPerTick bounds how many paths one poll cycle may stat.
const DefaultPollMaxPerTick = 500

// PollTracker records hot paths and round-robins known offsets so silent
// appends (open/write/close with no tree event) are eventually recovered
// without statting the full cold archive every tick.
//
// Each Candidates call states at most maxPerTick paths, in priority order:
//  1. live paths (caller-supplied)
//  2. recent tree-event paths (NoteEvent)
//  3. previously incomplete (size > offset)
//  4. round-robin sample of remaining keys in Offsets (jsonl preferred)
//
// Cold fully-caught-up paths are only stated when their turn in the rotation
// arrives — O(maxPerTick) stats per tick, not O(|Offsets|) every interval.
type PollTracker struct {
	mu          sync.Mutex
	recent      map[string]time.Time
	incomplete  map[string]struct{}
	recentFor   time.Duration
	maxPerTick  int
	samplePos   int // cursor into sorted offset keys
	statCalls   int
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
	// Stat is called for hot-set and a bounded rotation of Offsets keys.
	Stat StatFunc
}

// Candidates returns paths whose size exceeds offset (need re-ingest).
func (t *PollTracker) Candidates(args CandidatesArgs) []string {
	if args.Stat == nil {
		return nil
	}
	now := args.Now
	if now.IsZero() {
		now = time.Now()
	}

	t.mu.Lock()
	for p, ts := range t.recent {
		if now.Sub(ts) > t.recentFor {
			delete(t.recent, p)
		}
	}
	// Priority hot set: live ∪ recent ∪ incomplete.
	priority := make([]string, 0, len(args.Live)+len(t.recent)+len(t.incomplete))
	seen := make(map[string]struct{})
	addPri := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		priority = append(priority, p)
	}
	for _, p := range args.Live {
		addPri(p)
	}
	for p := range t.recent {
		addPri(p)
	}
	for p := range t.incomplete {
		addPri(p)
	}
	max := t.maxPerTick
	samplePos := t.samplePos
	t.mu.Unlock()

	// Round-robin keys from Offsets (prefer .jsonl — transcript append recovery).
	var keys []string
	for p := range args.Offsets {
		if _, ok := seen[p]; ok {
			continue
		}
		keys = append(keys, p)
	}
	sort.Strings(keys)
	// Prefer jsonl first so non-jsonl md offsets do not starve transcript recovery.
	sort.SliceStable(keys, func(i, j int) bool {
		iJ := strings.HasSuffix(keys[i], ".jsonl")
		jJ := strings.HasSuffix(keys[j], ".jsonl")
		if iJ != jJ {
			return iJ
		}
		return keys[i] < keys[j]
	})

	toStat := make([]string, 0, max)
	for _, p := range priority {
		if len(toStat) >= max {
			break
		}
		toStat = append(toStat, p)
	}
	if len(keys) > 0 && len(toStat) < max {
		if samplePos >= len(keys) {
			samplePos = 0
		}
		start := samplePos
		for len(toStat) < max {
			toStat = append(toStat, keys[samplePos])
			samplePos++
			if samplePos >= len(keys) {
				samplePos = 0
			}
			if samplePos == start {
				break // full cycle of remaining keys
			}
		}
	}
	t.mu.Lock()
	t.samplePos = samplePos
	t.mu.Unlock()

	out := make([]string, 0, len(toStat))
	for _, p := range toStat {
		off, known := args.Offsets[p]
		if !known {
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

// IsJSONL is a small helper for call sites filtering poll candidates.
func IsJSONL(path string) bool {
	return strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(filepath.Base(path), ".jsonl")
}
