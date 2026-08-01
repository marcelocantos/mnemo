// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"fmt"
	"testing"
	"time"
)

// Cold fully-ingested corpus must not be stated every tick (T142.5).
func TestPollTrackerSkipsColdCorpus(t *testing.T) {
	tr := NewPollTracker(time.Minute, 100)
	offsets := map[string]int64{}
	for i := 0; i < 10_000; i++ {
		p := fmt.Sprintf("/cold/session-%d.jsonl", i)
		offsets[p] = 100
	}
	// One live incomplete path.
	livePath := "/live/active.jsonl"
	offsets[livePath] = 50

	stat := func(path string) (int64, error) {
		if path == livePath {
			return 200, nil
		}
		// Cold paths should never be stated.
		t.Errorf("unexpected stat of cold path %s", path)
		return 100, nil
	}

	got := tr.Candidates(CandidatesArgs{
		Offsets: offsets,
		Live:    []string{livePath},
		Now:     time.Now(),
		Stat:    stat,
	})
	if len(got) != 1 || got[0] != livePath {
		t.Fatalf("candidates=%v want [%s]", got, livePath)
	}
	if tr.StatCallCount() != 1 {
		t.Fatalf("stat calls=%d want 1", tr.StatCallCount())
	}
	if tr.Stated("/cold/session-0.jsonl") {
		t.Fatal("cold path was stated")
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
	// Second tick still incomplete until cleared via size<=offset.
	got2 := tr.Candidates(CandidatesArgs{
		Offsets: offsets,
		Now:     now.Add(time.Second),
		Stat: func(p string) (int64, error) {
			return 10, nil // caught up
		},
	})
	if len(got2) != 0 {
		t.Fatalf("after catch-up got %v", got2)
	}
}
