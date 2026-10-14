// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The filters under test used to run after a fixed candidate window
// (200 for messages, 50 for tool_output). A window full of the wrong
// session type dropped the row the caller asked for. These tests put
// more wrong rows than that window holds, keep them better BM25 matches
// than the one right row, and require the right row back at a small limit.

func insertPlain(t *testing.T, s *Store, session, project, text string) int64 {
	t.Helper()
	res, err := s.writeDB.Exec(`INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES (?, ?, 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`, session, project, text)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSessionTypeFilterBeforeTruncation(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const token = "zqxfiltertoken"
	pad := strings.Repeat("padding word ", 40)
	for i := 0; i < 60; i++ {
		insertPlain(t, s, fmt.Sprintf("sub-session-%02d", i), "subagents", token)
	}
	insertPlain(t, s, "main-session-aaa", "workbench", token+" "+pad)

	got, err := s.Search(token, 10, "interactive", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSession(got, "main-session-aaa") {
		t.Fatalf("interactive hit missing from top 10 (got %d hits); the session-type filter ran after truncation", len(got))
	}
	for _, r := range got {
		if strings.HasPrefix(r.SessionID, "sub-session-") {
			t.Fatalf("subagent session %s survived an interactive filter", r.SessionID)
		}
	}

	res, err := s.UnifiedSearchOpts(token, UnifiedOpts{
		Kinds: []string{"message"}, Limit: 10, SessionType: "interactive",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range res.Hits {
		if h.Message != nil && h.Message.SessionID == "main-session-aaa" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unified message search dropped the interactive hit: %+v", res.Hits)
	}
}

func TestRepoFilterBeforeTruncation(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const token = "zqxrepotoken"
	pad := strings.Repeat("padding word ", 40)
	for i := 0; i < 60; i++ {
		sid := fmt.Sprintf("other-session-%02d", i)
		insertPlain(t, s, sid, "workbench", token)
		mustExec(t, s, `INSERT INTO session_meta (session_id, repo, cwd) VALUES (?, 'other/place', '/tmp/other/place')`, sid)
	}
	insertPlain(t, s, "right-session-aaa", "workbench", token+" "+pad)
	mustExec(t, s, `INSERT INTO session_meta (session_id, repo, cwd) VALUES ('right-session-aaa', 'sample/app', '/tmp/sample/app')`)

	got, err := s.Search(token, 10, "interactive", "sample/app", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSession(got, "right-session-aaa") {
		t.Fatalf("repo hit missing from top 10 (got %d); the repo filter ran after truncation", len(got))
	}
}

func TestToolOutputFilterBeforeTruncation(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const token = "zqxoutputtoken"
	pad := strings.Repeat("padding word ", 80)
	for i := 0; i < 60; i++ {
		insertToolBody(t, s, fmt.Sprintf("sub-out-%02d", i), "subagents", token)
	}
	insertToolBody(t, s, "main-out-aaa", "workbench", token+" "+pad)

	res, err := s.UnifiedSearchOpts(token, UnifiedOpts{
		Kinds: []string{"tool_output"}, Limit: 10, SessionType: "interactive",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range res.Hits {
		if strings.Contains(h.Body, "padding word") {
			found = true
		}
		if strings.Contains(h.Meta, "sub-out-") {
			t.Fatalf("subagent tool output survived the interactive filter: %s", h.Meta)
		}
	}
	if !found {
		t.Fatalf("interactive tool output missing from top 10 (%d hits); the filter ran after the corpus limit", len(res.Hits))
	}

	// Same shape for the repo filter: 60 outputs in the wrong repo.
	s2 := newTestStore(t, t.TempDir())
	for i := 0; i < 60; i++ {
		sid := fmt.Sprintf("repo-other-%02d", i)
		insertToolBody(t, s2, sid, "workbench", token)
		mustExec(t, s2, `INSERT INTO session_meta (session_id, repo, cwd) VALUES (?, 'other/place', '/tmp/other/place')`, sid)
	}
	insertToolBody(t, s2, "repo-right-aaa", "workbench", token+" "+pad)
	mustExec(t, s2, `INSERT INTO session_meta (session_id, repo, cwd) VALUES ('repo-right-aaa', 'sample/app', '/tmp/sample/app')`)
	res, err = s2.UnifiedSearchOpts(token, UnifiedOpts{
		Kinds: []string{"tool_output"}, Limit: 10, SessionType: "all", Repo: "sample/app",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, h := range res.Hits {
		if strings.Contains(h.Body, "padding word") {
			found = true
		}
	}
	if !found {
		t.Fatalf("repo-matching tool output missing (%d hits)", len(res.Hits))
	}
}

func TestCallerSessionExcludedBeforeTruncation(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const token = "zqxcallertoken"
	const caller = "caller-session-aaa"
	const other = "other-session-bbb"
	pad := strings.Repeat("padding word ", 40)
	for i := 0; i < 60; i++ {
		// Distinct sessions would each be one hit. The failure mode is
		// sixty hits in the caller's own session outranking the one
		// real hit. One session, sixty short messages.
		insertPlain(t, s, caller, "workbench", token+" "+fmt.Sprintf("n%d", i))
	}
	insertPlain(t, s, other, "workbench", token+" "+pad)

	// Without exclusion the caller's short messages take the window.
	open, err := s.Search(token, 10, "interactive", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if hasSession(open, other) {
		t.Fatal("the other session outranked the caller; the truncation test is not loaded")
	}
	if !hasSession(open, caller) {
		t.Fatal("expected the caller's own hits when exclusion is off")
	}

	got, err := s.SearchExcluding(token, 10, "interactive", "", 0, 0, false, caller)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSession(got, other) {
		t.Fatalf("other session missing after exclusion (got %d); the caller was filtered after truncation", len(got))
	}
	if hasSession(got, caller) {
		t.Fatal("caller session was returned despite ExcludeSession")
	}

	// Opt-in is an empty exclusion. Already covered by `open` above.
}

func insertToolBody(t *testing.T, s *Store, session, project, body string) {
	t.Helper()
	res, err := s.writeDB.Exec(`INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES (?, ?, 'user', 'preview without the token', '2026-01-01T00:00:00Z', 'user', 0, 'tool_result')`, session, project)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`INSERT INTO tool_outputs (message_id, session_id, project, source, text)
		VALUES (?, ?, ?, 'tool_use_result', ?)`, mid, session, project, body); err != nil {
		t.Fatal(err)
	}
}

func hasSession(hits []SearchResult, sid string) bool {
	for _, h := range hits {
		if h.SessionID == sid {
			return true
		}
	}
	return false
}
