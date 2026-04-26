// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// stubLLM lets the test pre-stage the response and assert that
// Reviewer.Tick called Call exactly the expected number of times.
type stubLLM struct {
	response string
	calls    atomic.Int64
	err      error
}

func (s *stubLLM) Call(_ context.Context, sys, user string) (LLMResult, error) {
	s.calls.Add(1)
	if s.err != nil {
		return LLMResult{}, s.err
	}
	return LLMResult{Text: s.response, Model: "stub"}, nil
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name, in, wantVerdict, wantSummary string
		wantErr                            bool
	}{
		{
			name:        "current verdict, no proposal",
			in:          `{"verdict":"current"}`,
			wantVerdict: "current",
		},
		{
			name:        "stale with proposed_summary",
			in:          `{"verdict":"stale","proposed_summary":"better"}`,
			wantVerdict: "stale",
			wantSummary: "better",
		},
		{
			name:        "tolerates ```json fence wrapper",
			in:          "```json\n{\"verdict\":\"current\"}\n```",
			wantVerdict: "current",
		},
		{
			name:        "tolerates plain ``` fence",
			in:          "```\n{\"verdict\":\"current\"}\n```",
			wantVerdict: "current",
		},
		{
			name:    "invalid verdict rejected",
			in:      `{"verdict":"unknown"}`,
			wantErr: true,
		},
		{
			name:    "non-JSON rejected",
			in:      "not even close",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseVerdict(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if got.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", got.Verdict, c.wantVerdict)
			}
			if got.ProposedSummary != c.wantSummary {
				t.Errorf("summary = %q, want %q", got.ProposedSummary, c.wantSummary)
			}
		})
	}
}

func TestTickFiresLLMWhenTriggered(t *testing.T) {
	s, repo := setupRepoWithSummary(t, 100) // 100 entries since (no prior) review

	llm := &stubLLM{response: `{"verdict":"current"}`}
	r := NewWithConfig(s, llm, store.ReviewTriggerConfig{
		HighEntryThreshold: 50,
		LowEntryThreshold:  10,
		LowMinAge:          24 * time.Hour,
	})

	r.Tick(context.Background())

	if got := llm.calls.Load(); got != 1 {
		t.Fatalf("LLM called %d times, want 1", got)
	}
	last, err := s.LatestReview(repo)
	if err != nil {
		t.Fatalf("LatestReview: %v", err)
	}
	if last == nil || last.Verdict != "current" {
		t.Fatalf("expected current verdict, got %+v", last)
	}
}

func TestTickSkipsWhenNotTriggered(t *testing.T) {
	s, repo := setupRepoWithSummary(t, 4) // only a handful of entries

	llm := &stubLLM{response: `{"verdict":"current"}`}
	r := NewWithConfig(s, llm, store.ReviewTriggerConfig{
		HighEntryThreshold: 500,
		LowEntryThreshold:  50,
		LowMinAge:          24 * time.Hour,
	})

	r.Tick(context.Background())

	if got := llm.calls.Load(); got != 0 {
		t.Errorf("LLM called %d times when no trigger expected", got)
	}
	last, err := s.LatestReview(repo)
	if err != nil {
		t.Fatalf("LatestReview: %v", err)
	}
	if last != nil {
		t.Errorf("expected no review recorded, got %+v", last)
	}
}

func TestTickSkipsRepoWithoutClaudeMD(t *testing.T) {
	// setupRepoWithSummary inserts CLAUDE.md; here we don't, by
	// running the same fixture with no claude_configs row.
	s, _ := setupRepoNoSummary(t, 100)

	llm := &stubLLM{response: `{"verdict":"current"}`}
	r := NewWithConfig(s, llm, store.ReviewTriggerConfig{
		HighEntryThreshold: 1, // would trigger if not for skip
		LowEntryThreshold:  1,
		LowMinAge:          1 * time.Nanosecond,
	})

	r.Tick(context.Background())

	if got := llm.calls.Load(); got != 0 {
		t.Errorf("LLM called %d times for repo without CLAUDE.md", got)
	}
}

func TestTickRecordsStaleVerdictWithProposal(t *testing.T) {
	s, repo := setupRepoWithSummary(t, 600)

	resp := store.ClaudeMDReview{
		Verdict:         "stale",
		ProposedSummary: "fresher one-liner",
	}
	body, _ := json.Marshal(struct {
		Verdict         string `json:"verdict"`
		ProposedSummary string `json:"proposed_summary"`
	}{resp.Verdict, resp.ProposedSummary})

	llm := &stubLLM{response: string(body)}
	r := NewWithConfig(s, llm, store.DefaultReviewTriggerConfig)

	r.Tick(context.Background())

	last, err := s.LatestReview(repo)
	if err != nil {
		t.Fatalf("LatestReview: %v", err)
	}
	if last == nil {
		t.Fatal("expected review recorded")
	}
	if last.Verdict != "stale" || last.ProposedSummary != "fresher one-liner" {
		t.Errorf("unexpected review: %+v", last)
	}
}

func TestTickContinuesAfterLLMError(t *testing.T) {
	s, _ := setupRepoWithSummary(t, 100)

	llm := &stubLLM{err: errors.New("LLM is down")}
	r := NewWithConfig(s, llm, store.ReviewTriggerConfig{
		HighEntryThreshold: 50,
		LowEntryThreshold:  10,
		LowMinAge:          1 * time.Hour,
	})

	// Should not panic, should not record a review.
	r.Tick(context.Background())

	if got := llm.calls.Load(); got != 1 {
		t.Errorf("LLM should have been attempted once, got %d", got)
	}
}

// setupRepoWithSummary creates a temp store with one repo, n entries,
// and a CLAUDE.md row so ListRepos returns a non-empty Summary.
// Returns the store and the auto-derived repo identifier.
func setupRepoWithSummary(t *testing.T, n int) (*store.Store, string) {
	t.Helper()
	s, repo := setupRepoNoSummary(t, n)
	if err := s.RecordClaudeConfig(repo, "/path/CLAUDE.md",
		"# x\n\nA repo for testing.\n"); err != nil {
		t.Fatalf("RecordClaudeConfig: %v", err)
	}
	return s, repo
}

// setupRepoNoSummary creates a temp store with one repo and n
// transcript entries, but no CLAUDE.md row. Used to exercise the
// "no summary, no review" skip.
func setupRepoNoSummary(t *testing.T, n int) (*store.Store, string) {
	t.Helper()

	projectDir := t.TempDir()
	entries := make([]map[string]any, 0, n+1)
	entries = append(entries, storetest.MetaMsg("user", "first",
		"2026-04-25T10:00:00Z",
		"/Users/dev/work/github.com/acme/demo", "main"))
	for i := 0; i < n; i++ {
		entries = append(entries,
			storetest.Msg("assistant", "x", "2026-04-25T10:00:00Z"))
	}
	storetest.WriteJSONL(t, projectDir, "demo", "sess-r41", entries)

	s := storetest.NewStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	repos, err := s.ListRepos("")
	if err != nil || len(repos) == 0 {
		t.Fatalf("ListRepos: %v (got %d)", err, len(repos))
	}
	return s, repos[0].Repo
}
