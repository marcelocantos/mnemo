// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// windowMsgs builds a contiguous fake window with ids 1..n.
func windowMsgs(n int) []store.SessionMessage {
	msgs := make([]store.SessionMessage, 0, n)
	for i := 1; i <= n; i++ {
		msgs = append(msgs, store.SessionMessage{ID: i, Role: "user", Text: "line"})
	}
	return msgs
}

// TestRenderTranscriptCarriesMessageIDs: the summariser can only name a
// boundary if the ids are in front of it (🎯T64.11).
func TestRenderTranscriptCarriesMessageIDs(t *testing.T) {
	got := renderTranscript([]store.SessionMessage{
		{ID: 42, Role: "user", Text: "hello"},
		{ID: 43, Role: "assistant", Text: "hi"},
	}, 10000)
	if !strings.Contains(got, "#42 [user] hello") {
		t.Errorf("transcript lost its message-id anchors: %q", got)
	}
	if !strings.Contains(got, "#43 [assistant] hi") {
		t.Errorf("transcript lost its message-id anchors: %q", got)
	}
}

func TestValidateSpans(t *testing.T) {
	ids := []int64{10, 11, 12, 13, 14, 15}

	tests := []struct {
		name string
		in   []Span
		want []Span
	}{{
		name: "clean spans pass through",
		in: []Span{
			{From: 10, To: 12, Label: "a"},
			{From: 13, To: 15, Label: "b"},
		},
		want: []Span{
			{From: 10, To: 12, Label: "a"},
			{From: 13, To: 15, Label: "b"},
		},
	}, {
		name: "ids outside the window are clamped to it",
		in:   []Span{{From: 1, To: 9999, Label: "a"}},
		want: []Span{{From: 10, To: 15, Label: "a"}},
	}, {
		name: "inverted interval is righted",
		in:   []Span{{From: 14, To: 11, Label: "a"}},
		want: []Span{{From: 11, To: 14, Label: "a"}},
	}, {
		name: "ids not present in the window snap to the nearest real message",
		in:   []Span{{From: 12, To: 13, Label: "a"}},
		want: []Span{{From: 12, To: 13, Label: "a"}},
	}, {
		name: "out-of-order spans are sorted",
		in: []Span{
			{From: 13, To: 15, Label: "b"},
			{From: 10, To: 12, Label: "a"},
		},
		want: []Span{
			{From: 10, To: 12, Label: "a"},
			{From: 13, To: 15, Label: "b"},
		},
	}, {
		name: "overlaps are trimmed",
		in: []Span{
			{From: 10, To: 13, Label: "a"},
			{From: 12, To: 15, Label: "b"},
		},
		want: []Span{
			{From: 10, To: 13, Label: "a"},
			{From: 14, To: 15, Label: "b"},
		},
	}, {
		name: "a span fully swallowed by its predecessor is dropped",
		in: []Span{
			{From: 10, To: 15, Label: "a"},
			{From: 11, To: 13, Label: "b"},
		},
		want: []Span{{From: 10, To: 15, Label: "a"}},
	}, {
		name: "textless spans are dropped as unindexable",
		in:   []Span{{From: 10, To: 12}},
		want: nil,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := validateSpans(tc.in, ids)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d spans %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i].From != tc.want[i].From || got[i].To != tc.want[i].To {
					t.Errorf("span %d = %d..%d, want %d..%d",
						i, got[i].From, got[i].To, tc.want[i].From, tc.want[i].To)
				}
			}
		})
	}
}

func TestValidateSpansEmptyWindow(t *testing.T) {
	if got := validateSpans([]Span{{From: 1, To: 2, Label: "a"}}, nil); got != nil {
		t.Errorf("no window means no anchorable spans, got %v", got)
	}
}

// TestCompactPersistsSpans covers the fold itself: one summarisation
// pass yields both the summary and the span index for its window.
func TestCompactPersistsSpans(t *testing.T) {
	s := &fakeStore{session: "sess-1", msgs: windowMsgs(6)}
	llm := &stubLLM{response: LLMResult{
		Model: "stub",
		Text: `{"targets":[],"decisions":[],"files":[],"open_threads":[],
			"summary":"two topics",
			"spans":[
				{"from":1,"to":3,"label":"first topic","summary":"the fts bug"},
				{"from":4,"to":6,"label":"second topic","summary":"the exporter"}
			]}`,
	}}
	c := New(s, llm, Config{})

	if _, err := c.Compact(context.Background(), "conn", "sess-1", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(s.segments) != 1 {
		t.Fatalf("got %d span writes, want 1", len(s.segments))
	}
	seg := s.segments[0]
	if seg.FromMsgID != 1 || seg.ToMsgID != 6 {
		t.Errorf("window = %d..%d, want 1..6", seg.FromMsgID, seg.ToMsgID)
	}
	if seg.WindowSummary != "two topics" {
		t.Errorf("window summary = %q", seg.WindowSummary)
	}
	if len(seg.Spans) != 2 {
		t.Fatalf("got %d topic spans, want 2", len(seg.Spans))
	}
	if seg.Spans[0].Label != "first topic" || seg.Spans[1].Label != "second topic" {
		t.Errorf("span labels = %q, %q", seg.Spans[0].Label, seg.Spans[1].Label)
	}
}

// TestCompactWithoutSpansStillIndexesWindow: a summariser that omits
// spans (older prompt, terse model) must still yield a searchable
// window-level span, so the pre-🎯T64.11 payload shape stays valid.
func TestCompactWithoutSpansStillIndexesWindow(t *testing.T) {
	s := &fakeStore{session: "sess-1", msgs: windowMsgs(4)}
	llm := &stubLLM{response: LLMResult{
		Model: "stub",
		Text:  `{"targets":[],"decisions":[],"files":[],"open_threads":[],"summary":"one topic"}`,
	}}
	c := New(s, llm, Config{})

	if _, err := c.Compact(context.Background(), "conn", "sess-1", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(s.segments) != 1 {
		t.Fatalf("got %d span writes, want 1", len(s.segments))
	}
	if len(s.segments[0].Spans) != 0 {
		t.Errorf("got %d topic spans, want 0", len(s.segments[0].Spans))
	}
	if s.segments[0].WindowSummary != "one topic" {
		t.Error("window span must still carry the summary so the window is findable")
	}
}

// TestCompactSpansAreClampedToWindow: the model's ids are external
// input. A hallucinated range must not escape the window it summarised.
func TestCompactSpansAreClampedToWindow(t *testing.T) {
	s := &fakeStore{session: "sess-1", msgs: windowMsgs(5)}
	llm := &stubLLM{response: LLMResult{
		Model: "stub",
		Text: `{"targets":[],"decisions":[],"files":[],"open_threads":[],
			"summary":"s",
			"spans":[{"from":-40,"to":99999,"label":"whole thing","summary":"x"}]}`,
	}}
	c := New(s, llm, Config{})

	if _, err := c.Compact(context.Background(), "conn", "sess-1", nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	sp := s.segments[0].Spans
	if len(sp) != 1 {
		t.Fatalf("got %d spans, want 1", len(sp))
	}
	if sp[0].FromMsgID != 1 || sp[0].ToMsgID != 5 {
		t.Errorf("span = %d..%d, want it clamped to 1..5", sp[0].FromMsgID, sp[0].ToMsgID)
	}
}

// TestCompactSurvivesSpanWriteFailure: spans are an enrichment. Losing
// them must not fail a compaction that already cost an LLM call — the
// watcher would retry and pay again, and the backfill recovers spans
// anyway.
func TestCompactSurvivesSpanWriteFailure(t *testing.T) {
	s := &fakeStore{
		session:     "sess-1",
		msgs:        windowMsgs(3),
		segmentsErr: errors.New("disk on fire"),
	}
	llm := &stubLLM{response: LLMResult{
		Model: "stub",
		Text:  `{"targets":[],"decisions":[],"files":[],"open_threads":[],"summary":"s"}`,
	}}
	c := New(s, llm, Config{})

	comp, err := c.Compact(context.Background(), "conn", "sess-1", nil)
	if err != nil {
		t.Fatalf("span write failure must not fail the compaction: %v", err)
	}
	if comp == nil || comp.ID == 0 {
		t.Error("compaction should still be durable")
	}
}

// TestSystemPromptDocumentsSpans guards the contract the payload
// depends on: if the schema stops asking for spans, the model stops
// emitting them and the fold silently degrades to window-only.
func TestSystemPromptDocumentsSpans(t *testing.T) {
	if !strings.Contains(SystemPrompt, `"spans"`) {
		t.Error("SystemPrompt no longer requests spans")
	}
	if !strings.Contains(SystemPrompt, "#<id>") {
		t.Error("SystemPrompt must explain the #<id> anchors it asks the model to cite")
	}
}
