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

	cands, _, err := s.SelectCompactionCandidates(
		3, now.Add(-15*time.Minute), 0.10, 100)
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

	cands, _, err := s.SelectCompactionCandidates(
		2, now.Add(-15*time.Minute), 0.10, 100)
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

// TestSelectCompactionCandidatesFilters verifies the convergence
// predicate (🎯T68.1): the delta/idle triggers still reject a session
// that is not yet owed, but there is NO recency floor — a stale,
// never-compacted session is owed and selected, not abandoned.
func TestSelectCompactionCandidatesFilters(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// "tiny" — recent and below minMsgs, so not owed yet (neither
	// trigger fires: delta < min, and last_msg is newer than the idle
	// cutoff).
	writeJSONL(t, dir, "p", "sess-tiny", []map[string]any{
		msg("user", "lonely", now.Add(-1*time.Minute).Format(time.RFC3339)),
	})
	// "stale" — last_msg a full day ago. Under the old recency window
	// this was dropped; under the convergence predicate it is idle and
	// has new substantive content, so it is OWED and must be selected.
	staleStart := now.Add(-24 * time.Hour)
	writeJSONL(t, dir, "p", "sess-stale", []map[string]any{
		msg("user", "x1", staleStart.Format(time.RFC3339)),
		msg("assistant", "x2", staleStart.Add(time.Minute).Format(time.RFC3339)),
		msg("user", "x3", staleStart.Add(2*time.Minute).Format(time.RFC3339)),
	})
	// "good" — recent and over the delta threshold.
	writeJSONL(t, dir, "p", "sess-good", []map[string]any{
		msg("user", "y1", now.Add(-5*time.Minute).Format(time.RFC3339)),
		msg("assistant", "y2", now.Add(-4*time.Minute).Format(time.RFC3339)),
		msg("user", "y3", now.Add(-3*time.Minute).Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	cands, _, err := s.SelectCompactionCandidates(
		3, now.Add(-15*time.Minute), 0.10, 100)
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
		t.Errorf("sess-tiny should not be owed yet (below minMsgs and not idle)")
	}
	if !ids["sess-stale"] {
		t.Errorf("sess-stale should be owed via the idle trigger — there is no recency floor")
	}
}

// TestSelectCompactionCandidatesHistoricalBacklog covers the core
// 🎯T68.1 convergence guarantee: a never-compacted session whose
// last message is far outside any former recency window is still
// owed a compaction and selected, and the returned backlog counts it.
func TestSelectCompactionCandidatesHistoricalBacklog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// A month-old session, never compacted — the kind the old 24h
	// recency floor abandoned permanently.
	old := now.Add(-30 * 24 * time.Hour)
	writeJSONL(t, dir, "p", "sess-historical", []map[string]any{
		msg("user", "h1", old.Format(time.RFC3339)),
		msg("assistant", "h2", old.Add(time.Minute).Format(time.RFC3339)),
		msg("user", "h3", old.Add(2*time.Minute).Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	cands, backlog, err := s.SelectCompactionCandidates(
		3, now.Add(-15*time.Minute), 0.10, 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	found := false
	for _, c := range cands {
		if c.SessionID == "sess-historical" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("month-old never-compacted session should be owed; got %+v", cands)
	}
	if backlog < 1 {
		t.Errorf("backlog should count the owed session, got %d", backlog)
	}
}

// TestSelectCompactionCandidatesDeltaTrigger verifies 🎯T67's
// trigger A: candidate qualifies when delta messages SINCE the
// session's latest compaction.entry_id_to clear the threshold —
// even if the lifetime substantive_msgs count is much higher
// (i.e. the session has already been compacted past most of its
// history).
func TestSelectCompactionCandidatesDeltaTrigger(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// 6 substantive messages, all recent.
	entries := []map[string]any{
		msg("user", "m1", now.Add(-6*time.Minute).Format(time.RFC3339)),
		msg("assistant", "m2", now.Add(-5*time.Minute).Format(time.RFC3339)),
		msg("user", "m3", now.Add(-4*time.Minute).Format(time.RFC3339)),
		msg("assistant", "m4", now.Add(-3*time.Minute).Format(time.RFC3339)),
		msg("user", "m5", now.Add(-2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "m6", now.Add(-1*time.Minute).Format(time.RFC3339)),
	}
	writeJSONL(t, dir, "p", "sess-delta", entries)

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Plant a compaction that covers ALL six messages so the delta
	// drops to zero. Even though lifetime substantive_msgs is 6
	// (which would qualify under the pre-T67 filter), the session
	// must NOT be selected because there's nothing new since the
	// compaction.
	var maxMsgID int64
	if err := s.db.QueryRow(`SELECT MAX(id) FROM messages WHERE session_id = 'sess-delta'`).Scan(&maxMsgID); err != nil {
		t.Fatalf("max msg id: %v", err)
	}
	var maxEntryID int64
	if err := s.db.QueryRow(`SELECT MAX(entry_id) FROM messages WHERE session_id = 'sess-delta'`).Scan(&maxEntryID); err != nil {
		t.Fatalf("max entry id: %v", err)
	}
	if _, err := s.PutCompaction(Compaction{
		SessionID:    "sess-delta",
		EntryIDFrom:  0,
		EntryIDTo:    maxEntryID,
		PromptTokens: 100,
		OutputTokens: 20,
		Summary:      "covers everything",
	}); err != nil {
		t.Fatalf("PutCompaction: %v", err)
	}

	cands, _, err := s.SelectCompactionCandidates(
		3, now.Add(-15*time.Minute), 0.10, 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	for _, c := range cands {
		if c.SessionID == "sess-delta" {
			t.Errorf("sess-delta should be filtered: prior compaction covers all messages")
		}
	}
}

// TestSelectCompactionCandidatesIdleTrigger verifies 🎯T67's
// trigger B: a session with fewer than MinDeltaMessages new
// substantive messages still qualifies when last_msg is older than
// idleCutoff. Captures small one-shot sessions that would otherwise
// never be compacted.
func TestSelectCompactionCandidatesIdleTrigger(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// 2 substantive messages, both posted ~30 min ago — below the
	// MinDeltaMessages=10 threshold but past the idleCutoff=15min mark.
	writeJSONL(t, dir, "p", "sess-idle", []map[string]any{
		msg("user", "small q", now.Add(-30*time.Minute).Format(time.RFC3339)),
		msg("assistant", "small a", now.Add(-29*time.Minute).Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	cands, _, err := s.SelectCompactionCandidates(
		10,                       // minDeltaMsgs — well above the 2 we have
		now.Add(-15*time.Minute), // idleCutoff: last_msg must be older than this
		0.10,
		100,
	)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	found := false
	for _, c := range cands {
		if c.SessionID == "sess-idle" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sess-idle should qualify via the idle trigger; got %+v", cands)
	}
}

// TestSelectCompactionCandidatesBudgetFilter verifies 🎯T67's
// candidate-level budget filter: a session whose cumulative
// compaction tokens already meet maxBudgetRatio of its assistant
// token cost is dropped from the candidate set, so the watcher
// never wakes up just to log budget_exceeded.
func TestSelectCompactionCandidatesBudgetFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// Build a session with assistant entries carrying real token
	// counts so the budget ratio resolves to something meaningful.
	asst := func(text string, ts time.Time, in, out int) map[string]any {
		return map[string]any{
			"type":      "assistant",
			"timestamp": ts.Format(time.RFC3339),
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
				"usage": map[string]any{
					"input_tokens":  in,
					"output_tokens": out,
				},
			},
		}
	}
	writeJSONL(t, dir, "p", "sess-broke", []map[string]any{
		asst("a1", now.Add(-10*time.Minute), 100, 50),
		msg("user", "u1", now.Add(-9*time.Minute).Format(time.RFC3339)),
		asst("a2", now.Add(-8*time.Minute), 100, 50),
		msg("user", "u2", now.Add(-7*time.Minute).Format(time.RFC3339)),
		asst("a3", now.Add(-6*time.Minute), 100, 50),
		msg("user", "u3", now.Add(-5*time.Minute).Format(time.RFC3339)),
		msg("user", "u4", now.Add(-4*time.Minute).Format(time.RFC3339)),
		msg("user", "u5", now.Add(-3*time.Minute).Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Plant a compaction whose prompt+output already exceed 50% of
	// the session's 450-token cost (3 × (100+50)).
	if _, err := s.PutCompaction(Compaction{
		SessionID:    "sess-broke",
		EntryIDFrom:  0,
		EntryIDTo:    1,
		PromptTokens: 300,
		OutputTokens: 100,
		Summary:      "spent",
	}); err != nil {
		t.Fatalf("PutCompaction: %v", err)
	}

	// 50% budget — already exceeded.
	cands, _, err := s.SelectCompactionCandidates(
		3, now.Add(-15*time.Minute), 0.50, 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	for _, c := range cands {
		if c.SessionID == "sess-broke" {
			t.Errorf("sess-broke should be filtered by the budget ratio; got %+v", cands)
		}
	}

	// Raising the budget to 200% lets it through (now well under).
	cands, _, err = s.SelectCompactionCandidates(
		3, now.Add(-15*time.Minute), 2.0, 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	found := false
	for _, c := range cands {
		if c.SessionID == "sess-broke" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sess-broke should pass when budget ratio raised; got %+v", cands)
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
