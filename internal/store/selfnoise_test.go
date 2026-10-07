// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for 🎯T190: mnemo's own tool traffic stays out of the index.
//
// Sentinels are nonsense words so a hit can only come from the fixture
// row that carries them.

func searchHits(t *testing.T, s *Store, q string) int {
	t.Helper()
	res, err := s.Search(q, 50, "all", "", 0, 0, false)
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	return len(res)
}

func noiseOf(t *testing.T, s *Store, where string, args ...any) []int {
	t.Helper()
	rows, err := s.readDB.Query(`SELECT is_noise FROM messages WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestSelfToolTrafficIsNotIndexed: a sentinel inside a mnemo tool_use
// input, or inside the tool_result paired with it, is not searchable;
// the same word in an ordinary message is. Both spellings of the tool
// name are covered — server-prefixed as Claude Code writes it, and bare
// as the Codex, Grok and Cursor parsers do.
func TestSelfToolTrafficIsNotIndexed(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "proj", "sess-self", []map[string]any{
		msg("user", "an ordinary message mentioning zqxnormal here", "2026-04-01T10:00:00Z"),
		toolUseMsg("2026-04-01T10:00:01Z", "mcp__mnemo__mnemo_search", "tu-1", map[string]any{"query": "zqxprefixed sentinel query"}),
		toolResultMsg("2026-04-01T10:00:02Z", "tu-1", "mnemo answered with zqxprefixedresult in the body"),
		toolUseMsg("2026-04-01T10:00:03Z", "mnemo_query", "tu-2", map[string]any{"query": "zqxbare sentinel query"}),
		toolResultMsg("2026-04-01T10:00:04Z", "tu-2", "mnemo answered with zqxbareresult in the body"),
		toolUseMsg("2026-04-01T10:00:05Z", "Grep", "tu-3", map[string]any{"query": "zqxothertool pattern"}),
		toolResultMsg("2026-04-01T10:00:06Z", "tu-3", "grep found zqxotherresult"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"zqxprefixed", "zqxprefixedresult", "zqxbare", "zqxbareresult"} {
		if n := searchHits(t, s, q); n != 0 {
			t.Errorf("%q: %d hits, want 0 — mnemo's own traffic reached the index", q, n)
		}
	}
	for _, q := range []string{"zqxnormal", "zqxothertool", "zqxotherresult"} {
		if n := searchHits(t, s, q); n != 1 {
			t.Errorf("%q: %d hits, want 1", q, n)
		}
	}
	if got := noiseOf(t, s, `tool_use_id IN ('tu-1','tu-2')`); fmtInts(got) != "[1 1 1 1]" {
		t.Errorf("is_noise on mnemo rows = %v, want all 1", got)
	}
	if got := noiseOf(t, s, `tool_use_id = 'tu-3'`); fmtInts(got) != "[0 0]" {
		t.Errorf("is_noise on a foreign tool's rows = %v, want all 0", got)
	}
}

func fmtInts(v []int) string {
	b, _ := json.Marshal(v)
	return strings.ReplaceAll(string(b), ",", " ")
}

// TestSelfToolResultInLaterBatchIsFlagged: the tool_use lands in one
// writer transaction and its tool_result in the next, so the in-memory
// set cannot answer and the indexed lookup must.
func TestSelfToolResultInLaterBatchIsFlagged(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	path := filepath.Join(t.TempDir(), "sess-later.jsonl")

	write := func(pf parsedFile) {
		t.Helper()
		ws, err := s.newWriterState()
		if err != nil {
			t.Fatal(err)
		}
		pf.path = path
		pf.sessionID = "sess-later"
		pf.project = "proj"
		s.writeParsedFile(ws, pf)
		ws.Close()
		if err := ws.tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	rawEntry := func(uuid string) parsedRawEntry {
		raw, _ := json.Marshal(map[string]any{"uuid": uuid, "type": "assistant"})
		return parsedRawEntry{entryType: "assistant", timestamp: "2026-04-01T10:00:00Z", raw: raw}
	}

	write(parsedFile{
		entries: []parsedRawEntry{rawEntry("u1")},
		messages: []parsedMessage{{
			entryIdx: 0, role: "assistant", typ: "assistant", contentType: "tool_use",
			toolName: "mnemo_sessions", toolUseID: "tu-later", text: "mnemo_sessions {}",
			timestamp: "2026-04-01T10:00:00Z",
		}},
	})
	write(parsedFile{
		entries: []parsedRawEntry{rawEntry("u2")},
		messages: []parsedMessage{{
			entryIdx: 0, role: "user", typ: "user", contentType: "tool_result",
			toolUseID: "tu-later", text: "zqxlaterresult listed three sessions",
			timestamp: "2026-04-01T10:00:01Z",
		}},
	})

	if got := noiseOf(t, s, `tool_use_id = 'tu-later'`); fmtInts(got) != "[1 1]" {
		t.Fatalf("is_noise = %v, want [1 1]", got)
	}
	if n := searchHits(t, s, "zqxlaterresult"); n != 0 {
		t.Errorf("tool_result from a later batch reached the index (%d hits)", n)
	}
}

// TestSelfNoiseBackfillFlipsAndUnindexes: rows written before 🎯T190
// are flipped, leave the FTS index, and the pass is idempotent. The
// store is uncompressed (plain INSERTs), so the FTS 'delete' is handed
// exactly the text that was indexed.
func TestSelfNoiseBackfillFlipsAndUnindexes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const ins = `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type, tool_name, tool_use_id)
		VALUES (?, 'p', ?, ?, '2026-01-01T00:00:00Z', ?, 0, ?, ?, ?)`
	mustExec(t, s, ins, "sess-bf", "assistant", `mcp__mnemo__mnemo_search {"query":"zqxoldprefixed"}`, "assistant", "tool_use", "mcp__mnemo__mnemo_search", "old-1")
	mustExec(t, s, ins, "sess-bf", "user", "result zqxoldprefixedresult", "user", "tool_result", "", "old-1")
	mustExec(t, s, ins, "sess-bf", "assistant", `mnemo_read_session {"session_id":"zqxoldbare"}`, "assistant", "tool_use", "mnemo_read_session", "old-2")
	mustExec(t, s, ins, "sess-bf", "user", "result zqxoldbareresult", "user", "tool_result", "", "old-2")
	mustExec(t, s, ins, "sess-bf", "assistant", `Read {"file_path":"zqxoldother"}`, "assistant", "tool_use", "Read", "old-3")
	mustExec(t, s, ins, "sess-bf", "user", "contents zqxoldotherresult", "user", "tool_result", "", "old-3")
	mustExec(t, s, ins, "sess-bf", "user", "a plain message with zqxoldnormal", "user", "text", "", "")

	// Before: everything is indexed, as it was on a pre-fix store.
	for _, q := range []string{"zqxoldprefixed", "zqxoldprefixedresult", "zqxoldbare", "zqxoldbareresult"} {
		if n := searchHits(t, s, q); n != 1 {
			t.Fatalf("precondition: %q has %d hits, want 1", q, n)
		}
	}
	var substantiveBefore int
	if err := s.readDB.QueryRow(`SELECT substantive_msgs FROM session_summary WHERE session_id = 'sess-bf'`).Scan(&substantiveBefore); err != nil {
		t.Fatal(err)
	}

	res, err := s.SelfNoiseBackfill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ToolUses != 2 || res.ToolResults != 2 {
		t.Errorf("flipped %+v, want 2 tool_uses and 2 tool_results", res)
	}

	for _, q := range []string{"zqxoldprefixed", "zqxoldprefixedresult", "zqxoldbare", "zqxoldbareresult"} {
		if n := searchHits(t, s, q); n != 0 {
			t.Errorf("%q: %d hits after backfill, want 0", q, n)
		}
	}
	for _, q := range []string{"zqxoldother", "zqxoldotherresult", "zqxoldnormal"} {
		if n := searchHits(t, s, q); n != 1 {
			t.Errorf("%q: %d hits after backfill, want 1 — backfill took a row it should not have", q, n)
		}
	}
	if got := noiseOf(t, s, `tool_use_id IN ('old-1','old-2')`); fmtInts(got) != "[1 1 1 1]" {
		t.Errorf("is_noise on flipped rows = %v, want all 1", got)
	}
	if got := noiseOf(t, s, `tool_use_id = 'old-3' OR content_type = 'text'`); fmtInts(got) != "[0 0 0]" {
		t.Errorf("is_noise on untouched rows = %v, want all 0", got)
	}

	var substantiveAfter int
	if err := s.readDB.QueryRow(`SELECT substantive_msgs FROM session_summary WHERE session_id = 'sess-bf'`).Scan(&substantiveAfter); err != nil {
		t.Fatal(err)
	}
	if substantiveAfter != substantiveBefore-4 {
		t.Errorf("substantive_msgs %d -> %d, want a decrement of 4", substantiveBefore, substantiveAfter)
	}

	// The index structure is still sound after the 'delete' commands.
	// This is the index-only check (rank=0): the external-content
	// comparison fails on every mnemo store by design, since noise rows
	// are never indexed. It does not detect a stale index row — the
	// zero-hit assertions above are what prove the rows actually left.
	if _, err := s.writeDB.Exec(`INSERT INTO messages_fts(messages_fts, rank) VALUES('integrity-check', 0)`); err != nil {
		t.Errorf("FTS integrity after backfill: %v", err)
	}

	// Idempotent.
	again, err := s.SelfNoiseBackfill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.ToolUses != 0 || again.ToolResults != 0 {
		t.Errorf("second pass flipped %+v, want nothing", again)
	}
}

// TestSelfNoiseBackfillRunsAtStartup: the phase is registered, so a
// store opened on pre-fix rows converges without anyone calling it.
func TestSelfNoiseBackfillRunsAtStartup(t *testing.T) {
	installTestRateCard(t, testRateCard())
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := New(dbPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type, tool_name, tool_use_id)
		VALUES ('sess-boot', 'p', 'assistant', 'mnemo_status {}', '2026-01-01T00:00:00Z', 'assistant', 0, 'tool_use', 'mnemo_status', 'boot-1')`)
	s.Close()

	s2, err := New(dbPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := noiseOf(t, s2, `tool_use_id = 'boot-1'`)
		if fmtInts(got) == "[1]" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup backfill did not flip the row: is_noise = %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestQuotedPhraseMatchesPlural is the corpus half of 🎯T191: the
// singular phrase finds the row that holds the plural.
func TestQuotedPhraseMatchesPlural(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('s-pl', 'p', 'user', 'we restarted the zqxbackground zqxworkers after lunch', '2026-01-01T00:00:00Z', 'user', 0, 'text')`)
	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('s-pl', 'p', 'user', 'zqxworkers in the zqxbackground, not adjacent', '2026-01-01T00:00:01Z', 'user', 0, 'text')`)

	if n := searchHits(t, s, `"zqxbackground zqxworker"`); n != 1 {
		t.Errorf(`"zqxbackground zqxworker": %d hits, want 1 (the plural row, and only the adjacent one)`, n)
	}
	// Through the unified path too, since that is what the tool calls.
	res, err := s.UnifiedSearch(`"zqxbackground zqxworker"`, []string{"message"}, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Errorf("UnifiedSearch: %d hits, want 1", len(res.Hits))
	}
}
