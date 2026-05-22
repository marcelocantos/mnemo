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

// TestSelectCompactionCandidatesUnboundSession covers 🎯T59 #6:
// an ingested session with enough substantive messages is returned
// by SelectCompactionCandidates even though no connection_session
// row has ever been recorded for it. Its ConnectionID comes back
// empty so a subsequent PutCompaction stores NULL.
func TestSelectCompactionCandidatesUnboundSession(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// 5 substantive user/assistant messages, all recent.
	entries := []map[string]any{
		msg("user", "hello one", now.Add(-5*time.Minute).Format(time.RFC3339)),
		msg("assistant", "reply one", now.Add(-4*time.Minute).Format(time.RFC3339)),
		msg("user", "hello two", now.Add(-3*time.Minute).Format(time.RFC3339)),
		msg("assistant", "reply two", now.Add(-2*time.Minute).Format(time.RFC3339)),
		msg("user", "hello three", now.Add(-1*time.Minute).Format(time.RFC3339)),
	}
	writeJSONL(t, dir, "unbound-project", "sess-unbound", entries)

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	cands, err := s.SelectCompactionCandidates(3, now.Add(-1*time.Hour), 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	var found *CompactionCandidate
	for i, c := range cands {
		if c.SessionID == "sess-unbound" {
			found = &cands[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected sess-unbound in candidates, got %+v", cands)
	}
	if found.ConnectionID != "" {
		t.Errorf("expected empty ConnectionID for unbound session, got %q", found.ConnectionID)
	}
}

// TestSelectCompactionCandidatesBoundSession covers 🎯T59 #7:
// a session with a recorded connection binding is returned with
// its best-effort ConnectionID populated, so the resulting
// compaction can be tagged for backwards compatibility with the
// pre-T59 connection-based restore path.
func TestSelectCompactionCandidatesBoundSession(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	entries := []map[string]any{
		msg("user", "bound one", now.Add(-3*time.Minute).Format(time.RFC3339)),
		msg("assistant", "bound reply", now.Add(-2*time.Minute).Format(time.RFC3339)),
		msg("user", "bound two", now.Add(-1*time.Minute).Format(time.RFC3339)),
	}
	writeJSONL(t, dir, "bound-project", "sess-bound", entries)

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Simulate a live MCP binding for this session.
	s.RecordConnectionSessionAt("conn-xyz", "sess-bound", now)

	cands, err := s.SelectCompactionCandidates(2, now.Add(-1*time.Hour), 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	var found *CompactionCandidate
	for i, c := range cands {
		if c.SessionID == "sess-bound" {
			found = &cands[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected sess-bound in candidates, got %+v", cands)
	}
	if found.ConnectionID != "conn-xyz" {
		t.Errorf("expected ConnectionID=conn-xyz, got %q", found.ConnectionID)
	}
}

// TestSelectCompactionCandidatesFilters verifies the threshold +
// recency filters reject ineligible sessions.
func TestSelectCompactionCandidatesFilters(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// "tiny" — recent but below minMsgs.
	writeJSONL(t, dir, "p", "sess-tiny", []map[string]any{
		msg("user", "lonely", now.Add(-1*time.Minute).Format(time.RFC3339)),
	})
	// "stale" — enough msgs but last_msg way outside recency window.
	staleStart := now.Add(-24 * time.Hour)
	writeJSONL(t, dir, "p", "sess-stale", []map[string]any{
		msg("user", "x1", staleStart.Format(time.RFC3339)),
		msg("assistant", "x2", staleStart.Add(time.Minute).Format(time.RFC3339)),
		msg("user", "x3", staleStart.Add(2*time.Minute).Format(time.RFC3339)),
	})
	// "good" — meets both criteria.
	writeJSONL(t, dir, "p", "sess-good", []map[string]any{
		msg("user", "y1", now.Add(-5*time.Minute).Format(time.RFC3339)),
		msg("assistant", "y2", now.Add(-4*time.Minute).Format(time.RFC3339)),
		msg("user", "y3", now.Add(-3*time.Minute).Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	cands, err := s.SelectCompactionCandidates(3, now.Add(-1*time.Hour), 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range cands {
		ids[c.SessionID] = true
	}
	if !ids["sess-good"] {
		t.Errorf("expected sess-good in candidates")
	}
	if ids["sess-tiny"] {
		t.Errorf("sess-tiny should be filtered (below minMsgs)")
	}
	if ids["sess-stale"] {
		t.Errorf("sess-stale should be filtered (outside recency)")
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
