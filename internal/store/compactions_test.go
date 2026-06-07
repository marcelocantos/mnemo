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

// asstTok builds an assistant JSONL entry carrying the per-turn token
// usage the token-volume model reads: output_tokens and
// cache_creation_input_tokens feed the addenda metric; input_tokens
// feeds the (cumulative) session cost the ratio guard divides against.
func asstTok(text, ts string, outTokens, cacheCreation, inputTokens int) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"message": map[string]any{
			"role":    "assistant",
			"content": text,
			"usage": map[string]any{
				"input_tokens":                inputTokens,
				"output_tokens":               outTokens,
				"cache_creation_input_tokens": cacheCreation,
			},
		},
	}
}

func candidateIDs(t *testing.T, s *Store, budget int64, ratio float64) map[string]bool {
	t.Helper()
	cands, _, err := s.SelectCompactionCandidates(budget, ratio, 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range cands {
		ids[c.SessionID] = true
	}
	return ids
}

// TestAddendaTokens checks the metric directly: with no cursor the sum
// covers the whole session (output + cache_creation over assistant
// entries); past a cursor it covers only entries after the cursor's
// owning entry, and user-entry tokens never count.
func TestAddendaTokens(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeJSONL(t, dir, "p", "sess-at", []map[string]any{
		asstTok("a1", now.Add(-5*time.Minute).Format(time.RFC3339), 100, 20, 500),
		msg("user", "u1 long enough to be substantive", now.Add(-4*time.Minute).Format(time.RFC3339)),
		asstTok("a2", now.Add(-3*time.Minute).Format(time.RFC3339), 200, 30, 600),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Whole session: (100+20) + (200+30) = 350.
	if got, err := s.AddendaTokens("sess-at", 0); err != nil {
		t.Fatalf("AddendaTokens: %v", err)
	} else if got != 350 {
		t.Errorf("whole-session addenda = %d, want 350", got)
	}

	// Cursor at the first assistant message: only a2 counts = 230.
	var firstAsstMsgID int64
	if err := s.writeDB.QueryRow(
		`SELECT MIN(id) FROM messages WHERE session_id='sess-at' AND role='assistant'`).Scan(&firstAsstMsgID); err != nil {
		t.Fatalf("first asst msg id: %v", err)
	}
	if got, err := s.AddendaTokens("sess-at", firstAsstMsgID); err != nil {
		t.Fatalf("AddendaTokens past cursor: %v", err)
	} else if got != 230 {
		t.Errorf("addenda past first assistant = %d, want 230", got)
	}
}

// TestSelectCompactionCandidatesSizeFloor verifies the unified floor:
// a session whose whole-session token volume is below the budget is
// never owed (its raw entries are its retrieval form), while one above
// the budget is owed. Connection attribution still flows through.
func TestSelectCompactionCandidatesSizeFloor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// Below floor: 50 < budget 100.
	writeJSONL(t, dir, "p", "sess-tiny", []map[string]any{
		msg("user", "a small question here", now.Add(-2*time.Minute).Format(time.RFC3339)),
		asstTok("tiny reply", now.Add(-1*time.Minute).Format(time.RFC3339), 50, 0, 200),
	})
	// Above floor: 120 + 40 = 160 >= budget 100.
	writeJSONL(t, dir, "p", "sess-big", []map[string]any{
		msg("user", "a much bigger question", now.Add(-3*time.Minute).Format(time.RFC3339)),
		asstTok("big reply one", now.Add(-2*time.Minute).Format(time.RFC3339), 120, 0, 400),
		asstTok("big reply two", now.Add(-1*time.Minute).Format(time.RFC3339), 0, 40, 400),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}
	// Bind sess-big so we also confirm attribution survives.
	s.RecordConnectionSessionAt("conn-big", "sess-big", now)

	cands, backlog, err := s.SelectCompactionCandidates(100, 0.10, 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidates: %v", err)
	}
	var big *CompactionCandidate
	ids := map[string]bool{}
	for i, c := range cands {
		ids[c.SessionID] = true
		if c.SessionID == "sess-big" {
			big = &cands[i]
		}
	}
	if ids["sess-tiny"] {
		t.Errorf("sess-tiny is below the floor and must not be owed")
	}
	if big == nil {
		t.Fatalf("sess-big is above the floor and must be owed; got %+v", cands)
	}
	if big.ConnectionID != "conn-big" {
		t.Errorf("expected ConnectionID=conn-big, got %q", big.ConnectionID)
	}
	if backlog < 1 {
		t.Errorf("backlog should count sess-big, got %d", backlog)
	}
}

// TestSelectCompactionCandidatesCursor verifies the re-compaction
// trigger and its convergence. The owed-predicate measures addenda past
// MAX(entry_id_to): a compaction at the phase-one cursor still leaves
// phase-two's tokens as addenda (owed); a compaction covering every
// message drops addenda to zero (not owed). No recency floor — the
// session is a month old throughout.
func TestSelectCompactionCandidatesCursor(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	writeJSONL(t, dir, "p", "sess-cur", []map[string]any{
		msg("user", "kickoff message text here", old.Format(time.RFC3339)),
		asstTok("phase one", old.Add(time.Minute).Format(time.RFC3339), 200, 0, 500),
		msg("user", "follow-up question text", old.Add(2*time.Minute).Format(time.RFC3339)),
		asstTok("phase two", old.Add(3*time.Minute).Format(time.RFC3339), 150, 0, 800),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Never compacted, above the floor, old → owed (no recency floor).
	if !candidateIDs(t, s, 100, 0.10)["sess-cur"] {
		t.Fatalf("old above-floor session should be owed")
	}

	// Cursor at the first (phase-one) assistant message: phase two's 150
	// tokens remain as addenda ≥ budget → still owed.
	var phaseOneMsgID, maxMsgID int64
	if err := s.writeDB.QueryRow(
		`SELECT MIN(id) FROM messages WHERE session_id='sess-cur' AND role='assistant'`).Scan(&phaseOneMsgID); err != nil {
		t.Fatalf("phase-one msg id: %v", err)
	}
	if err := s.writeDB.QueryRow(
		`SELECT MAX(id) FROM messages WHERE session_id='sess-cur'`).Scan(&maxMsgID); err != nil {
		t.Fatalf("max msg id: %v", err)
	}
	if _, err := s.PutCompaction(Compaction{
		SessionID: "sess-cur", EntryIDFrom: 0, EntryIDTo: phaseOneMsgID,
		PromptTokens: 80, OutputTokens: 20, Summary: "covers phase one",
	}); err != nil {
		t.Fatalf("PutCompaction (phase one): %v", err)
	}
	if !candidateIDs(t, s, 100, 0.10)["sess-cur"] {
		t.Errorf("addenda past the phase-one cursor (150) exceed the budget — should be owed")
	}

	// Advance the cursor to cover everything → addenda 0 → not owed.
	if _, err := s.PutCompaction(Compaction{
		SessionID: "sess-cur", EntryIDFrom: phaseOneMsgID, EntryIDTo: maxMsgID,
		PromptTokens: 80, OutputTokens: 20, Summary: "covers everything",
	}); err != nil {
		t.Fatalf("PutCompaction (all): %v", err)
	}
	if candidateIDs(t, s, 100, 0.10)["sess-cur"] {
		t.Errorf("a fully-covered session has zero addenda and must not be owed")
	}
}

// TestSelectCompactionCandidatesExcludesCompactorInternal is the
// precise recursion guard: a claudia-spawned summariser session
// (flagged via the marker on its first message) is excluded even when
// above the floor, while a genuine dev session in the same project is
// still selected. The legacy signature is detected too.
func TestSelectCompactionCandidatesExcludesCompactorInternal(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	writeJSONL(t, dir, "p", "sess-internal", []map[string]any{
		msg("user", CompactorMarker+" compact the following transcript span", now.Add(-3*time.Minute).Format(time.RFC3339)),
		asstTok("summary json here", now.Add(-2*time.Minute).Format(time.RFC3339), 300, 0, 500),
	})
	writeJSONL(t, dir, "p", "sess-legacy", []map[string]any{
		msg("user", LegacyCompactorSignature+" Given transcript messages...", now.Add(-3*time.Minute).Format(time.RFC3339)),
		asstTok("legacy summary", now.Add(-2*time.Minute).Format(time.RFC3339), 300, 0, 500),
	})
	writeJSONL(t, dir, "p", "sess-dev", []map[string]any{
		msg("user", "a real dev session in the mnemo repo", now.Add(-3*time.Minute).Format(time.RFC3339)),
		asstTok("dev reply", now.Add(-2*time.Minute).Format(time.RFC3339), 300, 0, 500),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	ids := candidateIDs(t, s, 100, 0.10)
	if ids["sess-internal"] {
		t.Errorf("marker-flagged compactor session must be excluded")
	}
	if ids["sess-legacy"] {
		t.Errorf("legacy-signature compactor session must be excluded")
	}
	if !ids["sess-dev"] {
		t.Errorf("genuine dev session must remain eligible (no cwd-prefix exclusion)")
	}

	// Confirm the flag actually landed on the right rows.
	var internal, dev int
	s.readDB.QueryRow(`SELECT compactor_internal FROM session_meta WHERE session_id='sess-internal'`).Scan(&internal)
	s.readDB.QueryRow(`SELECT COALESCE((SELECT compactor_internal FROM session_meta WHERE session_id='sess-dev'),0)`).Scan(&dev)
	if internal != 1 {
		t.Errorf("sess-internal should be flagged compactor_internal=1, got %d", internal)
	}
	if dev != 0 {
		t.Errorf("sess-dev should be compactor_internal=0, got %d", dev)
	}
}

// TestSelectCompactionCandidatesBudgetRatioGuard verifies the runaway
// backstop: a session whose cumulative summariser cost already meets
// maxBudgetRatio of its (input+output) session cost is dropped even
// though its addenda exceed the budget; raising the ratio lets it
// through.
func TestSelectCompactionCandidatesBudgetRatioGuard(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// Four assistant turns; cursor will sit at the first so addenda
	// past it (turns 2–4) stay above the budget.
	writeJSONL(t, dir, "p", "sess-broke", []map[string]any{
		asstTok("a1", now.Add(-10*time.Minute).Format(time.RFC3339), 200, 0, 100),
		asstTok("a2", now.Add(-8*time.Minute).Format(time.RFC3339), 200, 0, 100),
		asstTok("a3", now.Add(-6*time.Minute).Format(time.RFC3339), 200, 0, 100),
		asstTok("a4", now.Add(-4*time.Minute).Format(time.RFC3339), 200, 0, 100),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Cursor at the first assistant message; comp_tokens high relative
	// to sess_tokens (sum of input+output = 4×300 = 1200).
	var firstMsgID int64
	if err := s.writeDB.QueryRow(
		`SELECT MIN(id) FROM messages WHERE session_id='sess-broke'`).Scan(&firstMsgID); err != nil {
		t.Fatalf("first msg id: %v", err)
	}
	if _, err := s.PutCompaction(Compaction{
		SessionID: "sess-broke", EntryIDFrom: 0, EntryIDTo: firstMsgID,
		PromptTokens: 400, OutputTokens: 100, Summary: "spent", // comp_tokens=500
	}); err != nil {
		t.Fatalf("PutCompaction: %v", err)
	}

	// ratio 500/1200 ≈ 0.42 ≥ 0.10 → filtered.
	if candidateIDs(t, s, 100, 0.10)["sess-broke"] {
		t.Errorf("sess-broke should be filtered by the ratio guard")
	}
	// Raise the ratio cap above 0.42 → passes (addenda past cursor ≥ 100).
	if !candidateIDs(t, s, 100, 2.0)["sess-broke"] {
		t.Errorf("sess-broke should pass when the ratio cap is raised")
	}
}

// TestSearchWeightsCompactions verifies the 🎯T72 search integration:
// a matching compaction summary ranks above raw message hits, raw hits
// the compaction covers are suppressed (the summary stands in for them),
// and an uncovered addenda hit past the cursor still flows through.
func TestSearchWeightsCompactions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeJSONL(t, dir, "p", "sess-search", []map[string]any{
		msg("user", "early zorptastic discussion point", now.Add(-5*time.Minute).Format(time.RFC3339)),
		msg("assistant", "more zorptastic content in the middle", now.Add(-4*time.Minute).Format(time.RFC3339)),
		msg("user", "a later zorptastic tail message", now.Add(-1*time.Minute).Format(time.RFC3339)),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Cursor at the second message: messages 1–2 are covered, 3 is addenda.
	var coverTo, tailID int64
	if err := s.writeDB.QueryRow(
		`SELECT id FROM messages WHERE session_id='sess-search' AND text LIKE '%middle%'`).Scan(&coverTo); err != nil {
		t.Fatalf("cover-to id: %v", err)
	}
	if err := s.writeDB.QueryRow(
		`SELECT id FROM messages WHERE session_id='sess-search' AND text LIKE '%tail%'`).Scan(&tailID); err != nil {
		t.Fatalf("tail id: %v", err)
	}
	if _, err := s.PutCompaction(Compaction{
		SessionID: "sess-search", EntryIDFrom: 0, EntryIDTo: coverTo,
		Summary: "a zorptastic summary of the opening span",
	}); err != nil {
		t.Fatalf("PutCompaction: %v", err)
	}

	results, err := s.Search("zorptastic", 20, "all", "", 0, 0, true)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected results for 'zorptastic'")
	}
	if results[0].Role != "compaction" {
		t.Errorf("compaction summary should rank first, got role %q", results[0].Role)
	}
	for _, r := range results {
		if r.Role == "compaction" {
			continue
		}
		if r.MessageID > 0 && int64(r.MessageID) <= coverTo {
			t.Errorf("covered raw hit msg:%d should be suppressed by the compaction", r.MessageID)
		}
	}
	tailFound := false
	for _, r := range results {
		if int64(r.MessageID) == tailID {
			tailFound = true
		}
	}
	if !tailFound {
		t.Errorf("uncovered addenda hit (msg:%d) should flow through unmodified", tailID)
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
		if _, err := s.writeDB.Exec(q, args...); err != nil {
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
