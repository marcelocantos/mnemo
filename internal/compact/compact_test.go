// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

type fakeStore struct {
	mu       sync.Mutex
	session  string
	msgs     []store.SessionMessage
	compacts []store.Compaction
	nextID   int64
}

func (f *fakeStore) ReadSession(sessionID, role string, offset, limit int) ([]store.SessionMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID != f.session {
		return nil, nil
	}
	out := make([]store.SessionMessage, 0, len(f.msgs))
	for _, m := range f.msgs {
		if role != "" && m.Role != role {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeStore) PutCompaction(c store.Compaction) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	c.ID = f.nextID
	f.compacts = append(f.compacts, c)
	return c.ID, nil
}

func (f *fakeStore) LatestCompaction(sessionID string) (*store.Compaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.compacts) == 0 {
		return nil, nil
	}
	last := f.compacts[len(f.compacts)-1]
	return &last, nil
}

type stubLLM struct {
	response LLMResult
	err      error
	calls    int
	lastUser string
}

func (s *stubLLM) Call(ctx context.Context, sys, user string) (LLMResult, error) {
	s.calls++
	s.lastUser = user
	if s.err != nil {
		return LLMResult{}, s.err
	}
	return s.response, nil
}

func TestCompactRoundTrip(t *testing.T) {
	store := &fakeStore{
		session: "sess-1",
		msgs: []store.SessionMessage{
			{ID: 1, Role: "user", Text: "Work on T10"},
			{ID: 2, Role: "assistant", Text: "OK, let's design the schema"},
			{ID: 3, Role: "user", Text: "Tool loaded.", IsNoise: true},
			{ID: 4, Role: "user", Text: "Looks good, commit it"},
		},
	}
	llm := &stubLLM{response: LLMResult{
		Text: `{
  "targets": ["T10"],
  "decisions": [{"what": "schema design", "why": "needed before compactor"}],
  "files": ["internal/store/compactions.go"],
  "open_threads": [],
  "summary": "designed compaction schema"
}`,
		Model:        "claude-sonnet-4-6",
		PromptTokens: 400,
		OutputTokens: 120,
		CostUSD:      0.005,
	}}

	c := New(store, llm, Config{})
	got, err := c.Compact(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got.SessionID != "sess-1" {
		t.Fatalf("wrong session id: %q", got.SessionID)
	}
	if got.Summary != "designed compaction schema" {
		t.Fatalf("wrong summary: %q", got.Summary)
	}
	if got.EntryIDFrom != 0 || got.EntryIDTo != 4 {
		t.Fatalf("wrong range: %d..%d", got.EntryIDFrom, got.EntryIDTo)
	}
	if got.Model != "claude-sonnet-4-6" || got.PromptTokens != 400 || got.CostUSD != 0.005 {
		t.Fatalf("accounting not propagated: %+v", got)
	}

	// The user prompt should include the non-noise messages and exclude
	// the noise one.
	if !strings.Contains(llm.lastUser, "Work on T10") {
		t.Fatalf("prompt missing user message: %q", llm.lastUser)
	}
	if strings.Contains(llm.lastUser, "Tool loaded.") {
		t.Fatalf("prompt should have dropped noise message: %q", llm.lastUser)
	}
}

func TestCompactPicksUpAfterLatest(t *testing.T) {
	s := &fakeStore{
		session: "sess-1",
		msgs: []store.SessionMessage{
			{ID: 1, Role: "user", Text: "old 1"},
			{ID: 2, Role: "assistant", Text: "old 2"},
			{ID: 3, Role: "user", Text: "new 1"},
			{ID: 4, Role: "assistant", Text: "new 2"},
		},
	}
	// Seed an existing compaction covering 1..2.
	if err := insertSeed(s, 2); err != nil {
		t.Fatal(err)
	}

	llm := &stubLLM{response: LLMResult{
		Text: `{"targets":[],"decisions":[],"files":[],"open_threads":[],"summary":"second span"}`,
	}}
	c := New(s, llm, Config{})
	got, err := c.Compact(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got.EntryIDFrom != 2 || got.EntryIDTo != 4 {
		t.Fatalf("wrong range: %d..%d", got.EntryIDFrom, got.EntryIDTo)
	}
	// Prompt should only contain the new messages.
	if strings.Contains(llm.lastUser, "old 1") {
		t.Fatalf("prompt leaked already-compacted message: %q", llm.lastUser)
	}
	if !strings.Contains(llm.lastUser, "new 1") {
		t.Fatalf("prompt missing new message: %q", llm.lastUser)
	}
}

func insertSeed(s *fakeStore, to int64) error {
	_, err := s.PutCompaction(store.Compaction{
		SessionID:   "sess-1",
		EntryIDFrom: 0,
		EntryIDTo:   to,
		Summary:     "first span",
	})
	return err
}

func TestCompactNothingToDo(t *testing.T) {
	s := &fakeStore{session: "sess-1", msgs: []store.SessionMessage{{ID: 1, Role: "user", Text: "hi"}}}
	if err := insertSeed(s, 1); err != nil {
		t.Fatal(err)
	}
	llm := &stubLLM{}

	c := New(s, llm, Config{})
	_, err := c.Compact(context.Background(), "sess-1")
	if !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("expected ErrNothingToCompact, got %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM should not have been called, was: %d", llm.calls)
	}
}

func TestParsePayloadTolerantOfFences(t *testing.T) {
	body := "```json\n" + `{"targets":["T10"],"decisions":[],"files":[],"open_threads":[],"summary":"ok"}` + "\n```"
	p, _, err := parsePayload(body)
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if len(p.Targets) != 1 || p.Targets[0] != "T10" {
		t.Fatalf("bad parse: %+v", p)
	}
}

func TestParsePayloadRejectsGarbage(t *testing.T) {
	_, _, err := parsePayload("I cannot help with that")
	if err == nil {
		t.Fatalf("expected error for non-JSON input")
	}
}

func TestRenderTranscriptTruncates(t *testing.T) {
	msgs := []store.SessionMessage{
		{Role: "user", Text: strings.Repeat("a", 100)},
		{Role: "user", Text: strings.Repeat("b", 100)},
	}
	got := renderTranscript(msgs, 50)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker, got: %q", got)
	}
}
