// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// TestCompactWritesSpansToRealStore closes the gap the fake-store tests
// leave open (🎯T64.11): they prove the compactor *calls*
// PutCompactionSegments, not that a real *store.Store satisfies the
// interface and lands rows. This runs the actual compactor against a
// real store with a stubbed summariser, and asserts the spans arrive in
// topic_segments as sealed llm rows nested under their window.
func TestCompactWritesSpansToRealStore(t *testing.T) {
	proj := t.TempDir()
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	sid := "cccccccc-dddd-eeee-ffff-000000000001"
	entries := []map[string]any{
		storetest.MetaMsg("user", "fix the fts tokenizer bug", base.Format(time.RFC3339), "/Users/a/work/mnemo", "master"),
		storetest.Msg("assistant", "investigating fts5 diacritics", base.Add(time.Minute).Format(time.RFC3339)),
		storetest.Msg("user", "now build the vault exporter", base.Add(2*time.Minute).Format(time.RFC3339)),
		storetest.Msg("assistant", "vault wing scaffolded", base.Add(3*time.Minute).Format(time.RFC3339)),
	}
	storetest.WriteJSONL(t, proj, "-Users-a-work-mnemo", sid, entries)
	s := storetest.NewStore(t, proj)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Anchor the stub's span ids to the real messages.id values.
	bounds, err := s.Query(
		`SELECT MIN(id) AS lo, MAX(id) AS hi FROM messages WHERE session_id = '` + sid + `'`)
	if err != nil || len(bounds) != 1 {
		t.Fatalf("message bounds: %v", err)
	}
	lo, hi := toInt64(t, bounds[0]["lo"]), toInt64(t, bounds[0]["hi"])
	if lo == 0 || hi <= lo {
		t.Fatalf("unusable message range %d..%d", lo, hi)
	}
	mid := lo + (hi-lo)/2

	llm := &stubLLM{response: LLMResult{Model: "stub", Text: `{
		"targets":[],"decisions":[],"files":[],"open_threads":[],
		"summary":"tokenizer then exporter",
		"spans":[
			{"from":` + itoa(lo) + `,"to":` + itoa(mid) + `,"label":"fts tokenizer","summary":"diacritics in fts5"},
			{"from":` + itoa(mid+1) + `,"to":` + itoa(hi) + `,"label":"vault exporter","summary":"scaffolded the wing"}
		]}`}}

	c := New(s, llm, Config{})
	if _, err := c.Compact(context.Background(), "conn-1", sid, nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	segs, err := s.QuerySegments(store.SegmentQuery{SessionID: sid, Limit: 50})
	if err != nil {
		t.Fatalf("QuerySegments: %v", err)
	}
	var llmSpans, windowSpans int
	for _, sg := range segs {
		switch sg.Method {
		case store.SegmentMethodLLM:
			llmSpans++
			if !sg.Sealed {
				t.Errorf("llm span %q not sealed", sg.Label)
			}
			if sg.ParentID == "" {
				t.Errorf("llm span %q not nested under its window", sg.Label)
			}
		case store.SegmentMethodCompaction:
			windowSpans++
			if sg.Summary != "tokenizer then exporter" {
				t.Errorf("window span summary = %q", sg.Summary)
			}
		}
	}
	if windowSpans != 1 {
		t.Errorf("got %d window spans, want 1 (of %d segments)", windowSpans, len(segs))
	}
	if llmSpans != 2 {
		t.Errorf("got %d llm spans, want 2 (of %d segments)", llmSpans, len(segs))
	}

	// The spans are findable by their text, which is the whole point.
	hits, err := s.QuerySegments(store.SegmentQuery{FTSQuery: "diacritics", Limit: 10})
	if err != nil {
		t.Fatalf("span FTS: %v", err)
	}
	if len(hits) == 0 {
		t.Error("summariser-drawn span is not searchable")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func toInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		t.Fatalf("unexpected numeric type %T", v)
		return 0
	}
}
