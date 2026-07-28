// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

func TestSourceFromPath(t *testing.T) {
	tests := []struct {
		path       string
		wantSource string
		wantID     string
	}{
		{"/Users/a/.codex/sessions/2026/06/20/rollout-2026-06-20T20-10-47-019ee482-f152-7141-9b36-4ae6705019b1.jsonl",
			"codex", "019ee482-f152-7141-9b36-4ae6705019b1"},
		// The archived copy is the same session: Codex moves the file, and
		// mnemo must not treat the move as a second conversation.
		{"/Users/a/.codex/archived_sessions/rollout-2026-06-20T20-10-47-019ee482-f152-7141-9b36-4ae6705019b1.jsonl",
			"codex", "019ee482-f152-7141-9b36-4ae6705019b1"},
		{"/Users/a/.grok/sessions/%2FUsers%2Fa%2Fwork/019f4f4a-6237-7241-8431-d54cbcbbbcf4/updates.jsonl",
			"grok", "019f4f4a-6237-7241-8431-d54cbcbbbcf4"},
		{"/Users/a/.claude/projects/-Users-a-work/84369401-74e5-4d5e-834d-6732e8988328.jsonl",
			"claude", "84369401-74e5-4d5e-834d-6732e8988328"},
	}
	for _, tc := range tests {
		src, id, ok := sourceFromPath(tc.path)
		if !ok {
			t.Errorf("sourceFromPath(%q) not recognised", tc.path)
			continue
		}
		if src != tc.wantSource || id != tc.wantID {
			t.Errorf("sourceFromPath(%q) = (%s, %s), want (%s, %s)",
				tc.path, src, id, tc.wantSource, tc.wantID)
		}
	}

	if _, _, ok := sourceFromPath("/tmp/somewhere/else.jsonl"); ok {
		t.Error("a path under no known root must not be assigned a source")
	}
}

// TestRepairSessionSources reproduces the exact live defect: a Codex
// session tagged claude, plus a phantom row keyed by the rollout FILENAME
// holding no messages (🎯T127).
func TestRepairSessionSources(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	const (
		uuid = "019ee482-f152-7141-9b36-4ae6705019b1"
		stem = "rollout-2026-06-20T20-10-47-019ee482-f152-7141-9b36-4ae6705019b1"
		path = "/Users/a/.codex/sessions/2026/06/20/" + stem + ".jsonl"
	)

	// The real session, mis-tagged claude, with messages.
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, source) VALUES (?, 'claude')`, uuid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO messages (session_id, project, role, text, timestamp, type)
		 VALUES (?, 'p', 'user', 'hi', '2026-06-20T00:00:00Z', 'text')`, uuid); err != nil {
		t.Fatal(err)
	}
	// The phantom: filename-keyed, no messages.
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, source) VALUES (?, 'claude')`, stem); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO ingest_state (path, offset) VALUES (?, 0)`, path); err != nil {
		t.Fatal(err)
	}

	retagged, removed, err := s.RepairSessionSources()
	if err != nil {
		t.Fatalf("RepairSessionSources: %v", err)
	}
	if retagged != 1 {
		t.Errorf("retagged = %d, want 1", retagged)
	}
	if removed != 1 {
		t.Errorf("phantoms removed = %d, want 1", removed)
	}

	var src string
	if err := s.readDB.QueryRow(
		`SELECT source FROM session_meta WHERE session_id = ?`, uuid).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "codex" {
		t.Errorf("source = %q, want codex", src)
	}
	var phantoms int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM session_meta WHERE session_id = ?`, stem).Scan(&phantoms); err != nil {
		t.Fatal(err)
	}
	if phantoms != 0 {
		t.Error("phantom row survived the repair")
	}

	// Idempotent: the second run must find nothing left to do.
	retagged, removed, err = s.RepairSessionSources()
	if err != nil {
		t.Fatal(err)
	}
	if retagged != 0 || removed != 0 {
		t.Errorf("second run changed %d/%d rows; repair is not idempotent", retagged, removed)
	}
}

// TestSourceFromPathIgnoresEmbeddedIds guards against the corruption a
// substring match would cause.
//
// Grok url-encodes the working directory into its session directory
// name. When that cwd happens to be a Claude scratchpad, another
// session's uuid ends up INSIDE a grok path:
//
//	~/.grok/sessions/%2F…%2Fclaude-501%2F…%2Fa359b7fb-…%2Fscratchpad/<grok-id>/updates.jsonl
//
// A repair that located sessions by "path contains this id" would find
// the Claude session there and retag it grok — silently corrupting a
// perfectly good row. Observed for real: a359b7fb-83a5-42e2-8aaf-
// 738d1826a58b is a Claude session that a naive query flagged as grok.
// Provenance must come from path STRUCTURE, never substring.
func TestSourceFromPathIgnoresEmbeddedIds(t *testing.T) {
	const (
		claudeID = "a359b7fb-83a5-42e2-8aaf-738d1826a58b"
		grokID   = "019f4f4a-6237-7241-8431-d54cbcbbbcf4"
	)
	path := "/Users/a/.grok/sessions/%2Fprivate%2Ftmp%2Fclaude-501%2F-Users-a-jevons%2F" +
		claudeID + "%2Fscratchpad/" + grokID + "/updates.jsonl"

	src, id, ok := sourceFromPath(path)
	if !ok {
		t.Fatal("grok path not recognised")
	}
	if src != "grok" {
		t.Errorf("source = %q, want grok", src)
	}
	if id == claudeID {
		t.Fatal("resolved to the Claude session id embedded in the cwd — this would retag a Claude session as grok")
	}
	if id != grokID {
		t.Errorf("session id = %q, want the directory id %q", id, grokID)
	}
}

// TestRepairKeepsSessionsThatHaveMessages: a stem-keyed row is only a
// phantom when it is empty. Every Claude session is legitimately named by
// its filename stem, and deleting those would be catastrophic.
func TestRepairKeepsSessionsThatHaveMessages(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const id = "84369401-74e5-4d5e-834d-6732e8988328"
	path := "/Users/a/.claude/projects/-Users-a-work/" + id + ".jsonl"

	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, source) VALUES (?, 'claude')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO messages (session_id, project, role, text, timestamp, type)
		 VALUES (?, 'p', 'user', 'hi', '2026-07-01T00:00:00Z', 'text')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO ingest_state (path, offset) VALUES (?, 0)`, path); err != nil {
		t.Fatal(err)
	}

	if _, removed, err := s.RepairSessionSources(); err != nil {
		t.Fatal(err)
	} else if removed != 0 {
		t.Fatal("a Claude session named by its own stem must never be removed")
	}
	var n int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM session_meta WHERE session_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the Claude session was deleted")
	}
}
