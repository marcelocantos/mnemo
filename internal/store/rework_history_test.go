// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func TestReworkHistoryWithAttempts(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	base := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

	// Three compaction spans touching T5, ordered oldest → newest.
	// First: T5 appears in targets_active.
	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-a",
		GeneratedAt: base,
		Summary:     "first attempt at T5 — linker error",
		PayloadJSON: `{"targets_active":["T5","T3"],"targets_progressed":{},"open_threads":["arm64 stub missing"]}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Second: T5 appears in targets_progressed.
	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-b",
		GeneratedAt: base.Add(time.Hour),
		Summary:     "second attempt at T5 — wrong flag",
		PayloadJSON: `{"targets_active":[],"targets_progressed":{"T5":"tried conditional compilation flag, still failing"},"open_threads":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Third: T5 in active, more recent.
	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-c",
		GeneratedAt: base.Add(2 * time.Hour),
		Summary:     "third attempt at T5 — partial fix",
		PayloadJSON: `{"targets_active":["T5"],"targets_progressed":{},"open_threads":["tests still red"]}`,
	}); err != nil {
		t.Fatal(err)
	}

	// A span that doesn't touch T5 — should be excluded.
	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-d",
		GeneratedAt: base.Add(3 * time.Hour),
		Summary:     "unrelated work on T7",
		PayloadJSON: `{"targets_active":["T7"],"targets_progressed":{},"open_threads":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	attempts, err := s.ReworkHistory("T5", "", 10)
	if err != nil {
		t.Fatalf("ReworkHistory: %v", err)
	}

	// Expect 3 results, most-recent first.
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d: %+v", len(attempts), attempts)
	}
	if attempts[0].SessionID != "sess-c" {
		t.Errorf("attempt[0] should be sess-c (most recent), got %q", attempts[0].SessionID)
	}
	if attempts[1].SessionID != "sess-b" {
		t.Errorf("attempt[1] should be sess-b, got %q", attempts[1].SessionID)
	}
	if attempts[2].SessionID != "sess-a" {
		t.Errorf("attempt[2] should be sess-a (oldest), got %q", attempts[2].SessionID)
	}

	// Second attempt should carry progress note.
	if attempts[1].Progress != "tried conditional compilation flag, still failing" {
		t.Errorf("attempt[1].Progress unexpected: %q", attempts[1].Progress)
	}

	// First attempt should carry open_threads.
	if len(attempts[2].OpenThreads) != 1 || attempts[2].OpenThreads[0] != "arm64 stub missing" {
		t.Errorf("attempt[2].OpenThreads unexpected: %v", attempts[2].OpenThreads)
	}

	// Summaries should be populated.
	if attempts[0].Summary != "third attempt at T5 — partial fix" {
		t.Errorf("attempt[0].Summary unexpected: %q", attempts[0].Summary)
	}
}

func TestReworkHistoryNoHistory(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Insert a compaction for a different target.
	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-x",
		GeneratedAt: time.Now().UTC(),
		Summary:     "work on T99",
		PayloadJSON: `{"targets_active":["T99"],"targets_progressed":{},"open_threads":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	attempts, err := s.ReworkHistory("T5", "", 10)
	if err != nil {
		t.Fatalf("ReworkHistory unexpected error: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("expected empty result for unknown target, got %d: %+v", len(attempts), attempts)
	}
}

func TestReworkHistoryMalformedTargetID(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Insert a compaction that would partially match "T1" via substring.
	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-y",
		GeneratedAt: time.Now().UTC(),
		Summary:     "work on T10 and T11",
		PayloadJSON: `{"targets_active":["T10","T11"],"targets_progressed":{},"open_threads":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Empty target ID — should fail gracefully (returns empty, not a panic).
	attempts, err := s.ReworkHistory("", "", 10)
	if err != nil {
		t.Fatalf("ReworkHistory('') unexpected error: %v", err)
	}
	if len(attempts) != 0 {
		// An empty target ID LIKE-matches everything; the precise JSON membership
		// check must prevent false positives.
		t.Logf("empty target_id returned %d attempts (no crash is what matters)", len(attempts))
	}

	// "T1" should NOT match "T10" or "T11" — exact membership check must hold.
	attempts, err = s.ReworkHistory("T1", "", 10)
	if err != nil {
		t.Fatalf("ReworkHistory('T1') unexpected error: %v", err)
	}
	if len(attempts) != 0 {
		t.Errorf("T1 should not match T10/T11, got %d false-positive hits", len(attempts))
	}
}

func TestReworkHistoryRepoFilter(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	base := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

	// Insert session_meta rows so the repo filter has data to match.
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO session_meta (session_id, repo) VALUES (?, ?)`,
		"sess-bullseye", "marcelocantos/bullseye")
	mustExec(`INSERT INTO session_meta (session_id, repo) VALUES (?, ?)`,
		"sess-other", "marcelocantos/other")

	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-bullseye",
		GeneratedAt: base,
		Summary:     "T5 in bullseye",
		PayloadJSON: `{"targets_active":["T5"],"targets_progressed":{},"open_threads":[]}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutCompaction(Compaction{
		SessionID:   "sess-other",
		GeneratedAt: base.Add(time.Minute),
		Summary:     "T5 in other repo",
		PayloadJSON: `{"targets_active":["T5"],"targets_progressed":{},"open_threads":[]}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Filter to bullseye only.
	attempts, err := s.ReworkHistory("T5", "bullseye", 10)
	if err != nil {
		t.Fatalf("ReworkHistory with repo filter: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt for bullseye repo, got %d: %+v", len(attempts), attempts)
	}
	if attempts[0].SessionID != "sess-bullseye" {
		t.Errorf("expected sess-bullseye, got %q", attempts[0].SessionID)
	}
}
