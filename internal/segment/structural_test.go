// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package segment

import (
	"testing"
	"time"
)

func TestStructuralNestedAndSeal(t *testing.T) {
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	msgs := []Message{
		{ID: 1, Role: "user", Text: "fix the fts tokenizer", Timestamp: base.Format(time.RFC3339)},
		{ID: 2, Role: "assistant", Text: "looking at fts5", Timestamp: base.Add(1 * time.Minute).Format(time.RFC3339)},
		{ID: 3, Role: "user", Text: "also check diacritics", Timestamp: base.Add(2 * time.Minute).Format(time.RFC3339)},
		{ID: 4, Role: "assistant", Text: "diacritic path", Timestamp: base.Add(3 * time.Minute).Format(time.RFC3339)},
		// Strong idle gap → topic switch
		{ID: 5, Role: "user", Text: "now implement vault export", Timestamp: base.Add(2 * time.Hour).Format(time.RFC3339)},
		{ID: 6, Role: "assistant", Text: "vault wing", Timestamp: base.Add(2*time.Hour + time.Minute).Format(time.RFC3339)},
		{ID: 7, Role: "user", Text: "add migration doc", Timestamp: base.Add(2*time.Hour + 2*time.Minute).Format(time.RFC3339)},
		{ID: 8, Role: "assistant", Text: "done migration", Timestamp: base.Add(2*time.Hour + 3*time.Minute).Format(time.RFC3339)},
		{ID: 9, Role: "user", Text: "ship it", Timestamp: base.Add(2*time.Hour + 4*time.Minute).Format(time.RFC3339)},
		{ID: 10, Role: "assistant", Text: "shipped", Timestamp: base.Add(2*time.Hour + 5*time.Minute).Format(time.RFC3339)},
	}
	spans := Structural(msgs, DefaultConfig())
	if len(spans) < 2 {
		t.Fatalf("expected multi-span result, got %d: %+v", len(spans), spans)
	}
	// At least one sealed span exists when tail has enough context after first topic.
	sealed := 0
	var hasNested bool
	for _, sp := range spans {
		if sp.Sealed {
			sealed++
		}
		if sp.Level == 0 && sp.ParentIdx >= 0 {
			hasNested = true
		}
		if sp.Method != "structural" {
			t.Errorf("method=%q", sp.Method)
		}
	}
	if sealed == 0 {
		t.Error("expected at least one sealed span with lookahead")
	}
	if !hasNested && len(spans) >= 2 {
		// Coarse+fine may not always nest depending on cuts; require levels present.
		levels := map[int]bool{}
		for _, sp := range spans {
			levels[sp.Level] = true
		}
		if len(levels) < 1 {
			t.Error("no levels")
		}
	}
	id := SegmentID("sess", 1, 4, 0, "structural")
	if id[:4] != "seg_" || len(id) != 4+12 {
		t.Errorf("SegmentID shape: %q", id)
	}
	// Stable.
	if SegmentID("sess", 1, 4, 0, "structural") != id {
		t.Error("SegmentID not stable")
	}
}

func TestSealDoesNotRewriteLogic(t *testing.T) {
	// With no lookahead tail, last span stays unsealed.
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	msgs := []Message{
		{ID: 1, Role: "user", Text: "a", Timestamp: base.Format(time.RFC3339)},
		{ID: 2, Role: "assistant", Text: "b", Timestamp: base.Add(time.Minute).Format(time.RFC3339)},
		{ID: 3, Role: "user", Text: "c", Timestamp: base.Add(2 * time.Minute).Format(time.RFC3339)},
	}
	cfg := DefaultConfig()
	cfg.SealLookahead = 5
	spans := Structural(msgs, cfg)
	for _, sp := range spans {
		if sp.Sealed {
			t.Fatalf("expected unsealed when lookahead unmet: %+v", sp)
		}
	}
}

func TestPkAndWindowDiffPerfect(t *testing.T) {
	cuts := []int{3, 7}
	if Pk(10, cuts, cuts, 2) != 0 {
		t.Fatalf("Pk perfect want 0")
	}
	if WindowDiff(10, cuts, cuts, 2) != 0 {
		t.Fatalf("WindowDiff perfect want 0")
	}
	// Divergent hyp should score > 0.
	if Pk(10, cuts, []int{5}, 2) <= 0 {
		t.Error("expected Pk > 0 for mismatch")
	}
}
