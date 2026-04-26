// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
	"time"
)

func TestShouldReview(t *testing.T) {
	cfg := ReviewTriggerConfig{
		HighEntryThreshold: 500,
		LowEntryThreshold:  50,
		LowMinAge:          24 * time.Hour,
	}
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		lastReview  time.Time
		entries     int
		wantTrigger bool
		wantReason  string // substring match
	}{
		{
			name:        "high threshold fires immediately regardless of age",
			lastReview:  now.Add(-1 * time.Hour),
			entries:     500,
			wantTrigger: true,
			wantReason:  "high=500",
		},
		{
			name:        "high threshold fires even with no prior review",
			lastReview:  time.Time{},
			entries:     1000,
			wantTrigger: true,
			wantReason:  "high=500",
		},
		{
			name:        "low+age: both conditions met fires",
			lastReview:  now.Add(-25 * time.Hour),
			entries:     50,
			wantTrigger: true,
			wantReason:  "low=50",
		},
		{
			name:        "low+age: entries met but too recent does not fire",
			lastReview:  now.Add(-1 * time.Hour),
			entries:     100,
			wantTrigger: false,
		},
		{
			name:        "low+age: age met but too few entries does not fire",
			lastReview:  now.Add(-7 * 24 * time.Hour),
			entries:     10,
			wantTrigger: false,
		},
		{
			name:        "below both thresholds does not fire",
			lastReview:  now.Add(-1 * time.Hour),
			entries:     5,
			wantTrigger: false,
		},
		{
			name:        "no prior review + low entries fires (no age gate)",
			lastReview:  time.Time{},
			entries:     50,
			wantTrigger: true,
			wantReason:  "no prior review",
		},
		{
			name:        "no prior review + zero entries does not fire",
			lastReview:  time.Time{},
			entries:     0,
			wantTrigger: false,
		},
		{
			name:        "exactly at high threshold fires",
			lastReview:  now.Add(-1 * time.Hour),
			entries:     500,
			wantTrigger: true,
			wantReason:  "high=500",
		},
		{
			name:        "high+1 fires",
			lastReview:  now.Add(-1 * time.Minute),
			entries:     501,
			wantTrigger: true,
			wantReason:  "high=500",
		},
		{
			name:        "exactly at age threshold fires",
			lastReview:  now.Add(-24 * time.Hour),
			entries:     50,
			wantTrigger: true,
			wantReason:  "≥ 24h",
		},
		{
			name:        "1ns under age threshold does not fire",
			lastReview:  now.Add(-24*time.Hour + 1),
			entries:     50,
			wantTrigger: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTrigger, gotReason := ShouldReview(cfg, c.lastReview, c.entries, now)
			if gotTrigger != c.wantTrigger {
				t.Fatalf("trigger = %v, want %v (reason=%q)", gotTrigger, c.wantTrigger, gotReason)
			}
			if c.wantTrigger && c.wantReason != "" && !strings.Contains(gotReason, c.wantReason) {
				t.Errorf("reason %q does not contain %q", gotReason, c.wantReason)
			}
			if !c.wantTrigger && gotReason != "" {
				t.Errorf("reason should be empty when not triggered, got %q", gotReason)
			}
		})
	}
}

func TestRecordAndLatestReview(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// No review on file initially.
	got, err := s.LatestReview("alice/foo")
	if err != nil {
		t.Fatalf("LatestReview empty: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for repo with no reviews, got %+v", got)
	}

	r1 := ClaudeMDReview{
		Repo:            "alice/foo",
		ReviewedAt:      "2026-04-26T10:00:00Z",
		CommitID:        "abc123",
		Summary:         "old summary",
		Verdict:         "current",
		ProposedSummary: "",
	}
	if err := s.RecordReview(r1); err != nil {
		t.Fatalf("RecordReview r1: %v", err)
	}

	r2 := ClaudeMDReview{
		Repo:            "alice/foo",
		ReviewedAt:      "2026-04-26T11:00:00Z",
		CommitID:        "def456",
		Summary:         "old summary",
		Verdict:         "stale",
		ProposedSummary: "new summary",
	}
	if err := s.RecordReview(r2); err != nil {
		t.Fatalf("RecordReview r2: %v", err)
	}

	got, err = s.LatestReview("alice/foo")
	if err != nil {
		t.Fatalf("LatestReview: %v", err)
	}
	if got == nil {
		t.Fatal("expected latest review, got nil")
	}
	if got.ReviewedAt != "2026-04-26T11:00:00Z" {
		t.Errorf("LatestReview returned older row: %s", got.ReviewedAt)
	}
	if got.Verdict != "stale" || got.ProposedSummary != "new summary" {
		t.Errorf("verdict/proposed mismatch: %+v", got)
	}

	// UNIQUE(repo, reviewed_at): re-recording the same row is a no-op.
	if err := s.RecordReview(r1); err != nil {
		t.Errorf("re-record same row should be no-op, got error: %v", err)
	}
}

func TestEntriesSinceForRepo(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "demo", "sess-r41", []map[string]any{
		metaMsg("user", "first", "2026-04-25T10:00:00Z",
			"/Users/dev/work/github.com/acme/demo", "main"),
		msg("assistant", "second", "2026-04-25T11:00:00Z"),
		msg("user", "third", "2026-04-26T08:00:00Z"),
		msg("assistant", "fourth", "2026-04-26T09:00:00Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Find the auto-derived repo string.
	repos, err := s.ListRepos("")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) == 0 {
		t.Fatal("no repos after ingest")
	}
	repo := repos[0].Repo

	cases := []struct {
		name    string
		since   time.Time
		wantMin int // entries returned should be >= wantMin
	}{
		{"all entries", time.Time{}, 4},
		{"after 2026-04-25 noon: 2 entries", time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), 2},
		{"after 2026-04-26 09:00:01: 0 entries", time.Date(2026, 4, 26, 9, 0, 1, 0, time.UTC), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := s.EntriesSinceForRepo(repo, c.since)
			if err != nil {
				t.Fatalf("EntriesSinceForRepo: %v", err)
			}
			if n < c.wantMin {
				t.Errorf("got %d entries, want >= %d", n, c.wantMin)
			}
			// "0 entries" case is exact-match
			if c.wantMin == 0 && n != 0 {
				t.Errorf("got %d entries, want exactly 0", n)
			}
		})
	}
}
