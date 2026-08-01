// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"fmt"
	"testing"
	"time"
)

// One tick must not stat the entire cold offset map (bounded per tick).
func TestPollTrackerBoundsStatsPerTick(t *testing.T) {
	const maxPer = 50
	tr := NewPollTracker(time.Minute, maxPer)
	offsets := map[string]int64{}
	for i := 0; i < 10_000; i++ {
		offsets[fmt.Sprintf("/cold/session-%d.jsonl", i)] = 100
	}
	livePath := "/live/active.jsonl"
	offsets[livePath] = 50

	stat := func(path string) (int64, error) {
		if path == livePath {
			return 200, nil
		}
		return 100, nil
	}

	got := tr.Candidates(CandidatesArgs{
		Offsets: offsets,
		Live:    []string{livePath},
		Now:     time.Now(),
		Stat:    stat,
	})
	if tr.StatCallCount() > maxPer {
		t.Fatalf("stat calls=%d want <= %d", tr.StatCallCount(), maxPer)
	}
	foundLive := false
	for _, p := range got {
		if p == livePath {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("live incomplete missing from candidates: %v", got)
	}
}

// Silent append (no NoteEvent) must be recovered via offset rotation (T142.5).
func TestPollTrackerRecoversSilentAppend(t *testing.T) {
	tr := NewPollTracker(time.Minute, 2)
	// Three known paths; only #2 will grow without a tree event.
	p0 := "/s/a.jsonl"
	p1 := "/s/b.jsonl"
	p2 := "/s/silent.jsonl"
	offsets := map[string]int64{p0: 10, p1: 10, p2: 10}
	sizes := map[string]int64{p0: 10, p1: 10, p2: 10}

	// Grow silent path without NoteEvent.
	sizes[p2] = 99

	stat := func(path string) (int64, error) { return sizes[path], nil }

	var found bool
	// With maxPerTick=2, need a few ticks to rotate onto p2.
	for tick := 0; tick < 6; tick++ {
		got := tr.Candidates(CandidatesArgs{
			Offsets: offsets,
			Now:     time.Now().Add(time.Duration(tick) * time.Second),
			Stat:    stat,
		})
		for _, p := range got {
			if p == p2 {
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("silent append never recovered via offset rotation")
	}
	if !tr.Stated(p2) {
		t.Fatal("silent path was never stated")
	}
}

func TestPollTrackerRecentAndIncomplete(t *testing.T) {
	tr := NewPollTracker(time.Minute, 50)
	now := time.Now()
	path := "/hot/updates.jsonl"
	tr.NoteEvent(path, now)
	offsets := map[string]int64{path: 10}

	got := tr.Candidates(CandidatesArgs{
		Offsets: offsets,
		Now:     now,
		Stat: func(p string) (int64, error) {
			return 99, nil
		},
	})
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	got2 := tr.Candidates(CandidatesArgs{
		Offsets: offsets,
		Now:     now.Add(time.Second),
		Stat: func(p string) (int64, error) {
			return 10, nil
		},
	})
	if len(got2) != 0 {
		t.Fatalf("after catch-up got %v", got2)
	}
}
