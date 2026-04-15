// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func TestCompactionsRoundTrip(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// No compaction yet.
	if got, err := s.LatestCompaction("sess-1"); err != nil {
		t.Fatalf("LatestCompaction on empty: %v", err)
	} else if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}

	c1 := Compaction{
		SessionID:    "sess-1",
		Model:        "claude-sonnet-4-6",
		PromptTokens: 1200,
		OutputTokens: 350,
		CostUSD:      0.012,
		EntryIDFrom:  0,
		EntryIDTo:    42,
		PayloadJSON:  `{"targets":["T10"],"summary":"first pass"}`,
		Summary:      "first pass",
	}
	id, err := s.PutCompaction(c1)
	if err != nil {
		t.Fatalf("PutCompaction: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero id")
	}

	// Second compaction for the same session, later.
	c2 := c1
	c2.GeneratedAt = time.Now().UTC().Add(time.Second)
	c2.EntryIDFrom = 42
	c2.EntryIDTo = 90
	c2.Summary = "second pass"
	c2.PayloadJSON = `{"targets":["T10"],"summary":"second pass"}`
	if _, err := s.PutCompaction(c2); err != nil {
		t.Fatalf("PutCompaction (second): %v", err)
	}

	latest, err := s.LatestCompaction("sess-1")
	if err != nil {
		t.Fatalf("LatestCompaction: %v", err)
	}
	if latest == nil || latest.Summary != "second pass" {
		t.Fatalf("expected second pass, got %+v", latest)
	}
	if latest.Model != "claude-sonnet-4-6" || latest.PromptTokens != 1200 {
		t.Fatalf("metadata not preserved: %+v", latest)
	}

	list, err := s.ListCompactions("sess-1", 0)
	if err != nil {
		t.Fatalf("ListCompactions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 compactions, got %d", len(list))
	}
	if list[0].Summary != "first pass" || list[1].Summary != "second pass" {
		t.Fatalf("wrong order: %v / %v", list[0].Summary, list[1].Summary)
	}
}

func TestChainCompactionsNoChain(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	if _, err := s.PutCompaction(Compaction{
		SessionID: "solo",
		Summary:   "only",
	}); err != nil {
		t.Fatalf("PutCompaction: %v", err)
	}

	got, err := s.ChainCompactions("solo")
	if err != nil {
		t.Fatalf("ChainCompactions: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "only" {
		t.Fatalf("expected one compaction with summary 'only', got %+v", got)
	}
}

func TestChainCompactionsWalksChain(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Manually wire a chain A -> B -> C (A oldest).
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO session_chains (successor_id, predecessor_id, gap_ms, confidence)
		VALUES (?, ?, ?, ?)`, "B", "A", 500, "high")
	mustExec(`INSERT INTO session_chains (successor_id, predecessor_id, gap_ms, confidence)
		VALUES (?, ?, ?, ?)`, "C", "B", 500, "high")

	// Compactions for A and C only (B has none).
	if _, err := s.PutCompaction(Compaction{SessionID: "A", Summary: "a-span"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutCompaction(Compaction{SessionID: "C", Summary: "c-span"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ChainCompactions("C")
	if err != nil {
		t.Fatalf("ChainCompactions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 compactions, got %d: %+v", len(got), got)
	}
	if got[0].Summary != "a-span" || got[1].Summary != "c-span" {
		t.Fatalf("expected oldest-first a-span, c-span; got %v / %v",
			got[0].Summary, got[1].Summary)
	}
}
