// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeJSONL writes transcript entries as a JSONL file and returns the path.
func writeJSONL(t *testing.T, dir, project, sessionID string, entries []map[string]any) string {
	t.Helper()
	projDir := filepath.Join(dir, project)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projDir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func msg(typ, text, ts string) map[string]any {
	return map[string]any{
		"type":      typ,
		"timestamp": ts,
		"message":   map[string]any{"content": text},
	}
}

func metaMsg(typ, text, ts, cwd, branch string) map[string]any {
	m := msg(typ, text, ts)
	if cwd != "" {
		m["cwd"] = cwd
	}
	if branch != "" {
		m["gitBranch"] = branch
	}
	return m
}

func noiseMsg(ts string) map[string]any {
	return msg("user", "Tool loaded.", ts)
}

func newTestStore(t *testing.T, projectDir string) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIngestAndSearch(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-abc123", []map[string]any{
		metaMsg("user", "How do I fix the authentication bug?", "2026-04-01T10:00:00Z",
			"/Users/dev/work/github.com/acme/webapp", "fix/auth-bug"),
		msg("assistant", "The authentication bug is in the login handler. You need to check the session token expiry.", "2026-04-01T10:00:05Z"),
		msg("user", "That fixed it, thanks!", "2026-04-01T10:01:00Z"),
		noiseMsg("2026-04-01T10:01:05Z"),
		msg("assistant", "Glad it worked. The root cause was an off-by-one in the expiry check.", "2026-04-01T10:01:10Z"),
	})

	writeJSONL(t, projectDir, "otherproject", "sess-def456", []map[string]any{
		metaMsg("user", "Let's refactor the database layer", "2026-04-02T09:00:00Z",
			"/Users/dev/work/github.com/acme/backend", "refactor/db-layer"),
		msg("assistant", "I'll start by extracting the query builder into its own package.", "2026-04-02T09:00:05Z"),
		msg("user", "Sounds good, go ahead", "2026-04-02T09:01:00Z"),
		msg("assistant", "Done. The database queries are now in pkg/querybuilder.", "2026-04-02T09:02:00Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Search for "authentication" should find the first session.
	results, err := s.Search("authentication", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results for 'authentication'")
	}
	if results[0].SessionID != "sess-abc123" {
		t.Errorf("expected session sess-abc123, got %s", results[0].SessionID)
	}

	// Search for "database" should find the second session.
	results, err = s.Search("database", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results for 'database'")
	}
	found := false
	for _, r := range results {
		if r.SessionID == "sess-def456" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sess-def456 in search results for 'database'")
	}

	// "Tool loaded." is noise — should not appear in search.
	results, err = s.Search(`"Tool loaded"`, 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for noise search, got %d", len(results))
	}

	// Search with repo filter — bare name.
	results, err = s.Search("authentication", 10, "all", "webapp", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected results for repo filter 'webapp'")
	}

	// Search with repo filter — org/repo.
	results, err = s.Search("authentication", 10, "all", "acme/webapp", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected results for repo filter 'acme/webapp'")
	}

	// Search with repo filter — no match.
	results, err = s.Search("authentication", 10, "all", "nonexistent", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for repo filter 'nonexistent', got %d", len(results))
	}

	// Search with context.
	results, err = s.Search("authentication", 10, "all", "", 1, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results with context")
	}
	r := results[0]
	if r.MessageID == 0 {
		t.Error("expected non-zero message ID")
	}
	// The hit is "authentication bug" from the user. There should be
	// context after (the assistant response).
	if len(r.After) == 0 {
		t.Error("expected at least 1 context message after hit")
	}
}

func TestListSessions(t *testing.T) {
	projectDir := t.TempDir()

	// Create a session with enough messages to pass the default min_messages filter.
	entries := []map[string]any{
		metaMsg("user", "Working on the API endpoint", "2026-04-01T10:00:00Z",
			"/Users/dev/work/github.com/acme/webapp", "feature/api"),
	}
	for i := range 8 {
		ts := "2026-04-01T10:0" + string(rune('1'+i)) + ":00Z"
		if i%2 == 0 {
			entries = append(entries, msg("assistant", "Here is the implementation for step "+string(rune('1'+i)), ts))
		} else {
			entries = append(entries, msg("user", "Looks good, continue with next step "+string(rune('1'+i)), ts))
		}
	}
	writeJSONL(t, projectDir, "myproject", "sess-big", entries)

	// Create a small session (should be filtered by min_messages=6).
	writeJSONL(t, projectDir, "myproject", "sess-tiny", []map[string]any{
		msg("user", "hello", "2026-04-02T10:00:00Z"),
		msg("assistant", "hi there", "2026-04-02T10:00:05Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Default min_messages=6 should only return the big session.
	sessions, err := s.ListSessions("all", 6, 30, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session with >=6 messages, got %d", len(sessions))
	}
	if sessions[0].SessionID != "sess-big" {
		t.Errorf("expected sess-big, got %s", sessions[0].SessionID)
	}

	// min_messages=1 should return both.
	sessions, err = s.ListSessions("all", 1, 30, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions with min_messages=1, got %d", len(sessions))
	}

	// Filter by repo.
	sessions, err = s.ListSessions("all", 1, 30, "", "acme/webapp", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for repo acme/webapp, got %d", len(sessions))
	}

	// Filter by work type.
	sessions, err = s.ListSessions("all", 1, 30, "", "", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for work_type=feature, got %d", len(sessions))
	}
}

func TestReadSession(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-read-test", []map[string]any{
		msg("user", "first message", "2026-04-01T10:00:00Z"),
		msg("assistant", "second message", "2026-04-01T10:00:05Z"),
		noiseMsg("2026-04-01T10:00:06Z"),
		msg("user", "third message", "2026-04-01T10:00:10Z"),
		msg("assistant", "fourth message", "2026-04-01T10:00:15Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Read all messages (including noise).
	msgs, err := s.ReadSession("sess-read-test", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(msgs))
	}

	// Noise message should be flagged.
	if !msgs[2].IsNoise {
		t.Error("expected message 3 to be noise")
	}
	if msgs[0].IsNoise {
		t.Error("expected message 1 to not be noise")
	}

	// Filter by role.
	msgs, err = s.ReadSession("sess-read-test", "user", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 { // 2 real user + 1 noise user
		t.Errorf("expected 3 user messages, got %d", len(msgs))
	}

	// Pagination: offset=1, limit=2.
	msgs, err = s.ReadSession("sess-read-test", "", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages with offset=1 limit=2, got %d", len(msgs))
	}
	if msgs[0].Text != "second message" {
		t.Errorf("expected 'second message', got %q", msgs[0].Text)
	}

	// Prefix resolution.
	msgs, err = s.ReadSession("sess-read", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected prefix resolution to work, got %d messages", len(msgs))
	}
}

func TestStats(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-stats", []map[string]any{
		msg("user", "a real message", "2026-04-01T10:00:00Z"),
		msg("assistant", "a real response", "2026-04-01T10:00:05Z"),
		noiseMsg("2026-04-01T10:00:06Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalSessions != 1 {
		t.Errorf("expected 1 session, got %d", stats.TotalSessions)
	}
	if stats.TotalMessages != 3 {
		t.Errorf("expected 3 total messages, got %d", stats.TotalMessages)
	}
	if len(stats.ByType) != 1 {
		t.Fatalf("expected 1 session type, got %d", len(stats.ByType))
	}
	if stats.ByType[0].SubstantiveMsgs != 2 {
		t.Errorf("expected 2 substantive messages, got %d", stats.ByType[0].SubstantiveMsgs)
	}
	if stats.ByType[0].NoiseMsgs != 1 {
		t.Errorf("expected 1 noise message, got %d", stats.ByType[0].NoiseMsgs)
	}
}

func TestQuery(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-query", []map[string]any{
		msg("user", "test query message", "2026-04-01T10:00:00Z"),
		msg("assistant", "test query response", "2026-04-01T10:00:05Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT COUNT(*) AS cnt FROM messages WHERE session_id = 'sess-query'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	cnt, ok := rows[0]["cnt"].(int64)
	if !ok || cnt != 2 {
		t.Errorf("expected count=2, got %v", rows[0]["cnt"])
	}

	// Write queries must be rejected.
	_, err = s.Query("DELETE FROM messages")
	if err == nil {
		t.Error("expected error for DELETE query")
	}
	_, err = s.Query("DROP TABLE messages")
	if err == nil {
		t.Error("expected error for DROP query")
	}

	// WITH (CTE) queries should work.
	rows, err = s.Query("WITH s AS (SELECT COUNT(*) AS c FROM messages) SELECT c FROM s")
	if err != nil {
		t.Fatalf("expected WITH query to work, got %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row from CTE, got %d", len(rows))
	}
}

func TestSessionMetadata(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-meta", []map[string]any{
		metaMsg("user", "Implementing the new caching layer for the API",
			"2026-04-01T10:00:00Z",
			"/Users/dev/work/github.com/acme/service", "feature/caching"),
		msg("assistant", "Starting with the cache invalidation strategy.", "2026-04-01T10:00:05Z"),
		msg("user", "Good approach, proceed", "2026-04-01T10:01:00Z"),
		msg("assistant", "Cache layer is implemented.", "2026-04-01T10:02:00Z"),
		msg("user", "Let's add some tests", "2026-04-01T10:03:00Z"),
		msg("assistant", "Tests added and passing.", "2026-04-01T10:04:00Z"),
		msg("user", "Ship it", "2026-04-01T10:05:00Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	sessions, err := s.ListSessions("all", 1, 10, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	si := sessions[0]
	if si.Repo != "acme/service" {
		t.Errorf("expected repo acme/service, got %q", si.Repo)
	}
	if si.WorkType != "feature" {
		t.Errorf("expected work_type feature, got %q", si.WorkType)
	}
	if si.Topic != "Implementing the new caching layer for the API" {
		t.Errorf("unexpected topic: %q", si.Topic)
	}
}

func TestSessionTypeFiltering(t *testing.T) {
	projectDir := t.TempDir()

	// Interactive session (project name doesn't match subagent/worktree/ephemeral patterns).
	writeJSONL(t, projectDir, "myproject", "sess-interactive", []map[string]any{
		msg("user", "interactive message one", "2026-04-01T10:00:00Z"),
		msg("assistant", "interactive response one", "2026-04-01T10:00:05Z"),
	})

	// Subagent session (project = "subagents").
	writeJSONL(t, projectDir, "subagents", "sess-sub", []map[string]any{
		msg("user", "subagent task message", "2026-04-01T11:00:00Z"),
		msg("assistant", "subagent task response", "2026-04-01T11:00:05Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Default (interactive) should only return the interactive session.
	sessions, err := s.ListSessions("interactive", 1, 30, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 interactive session, got %d", len(sessions))
	}
	if sessions[0].SessionID != "sess-interactive" {
		t.Errorf("expected sess-interactive, got %s", sessions[0].SessionID)
	}

	// "all" should return both.
	sessions, err = s.ListSessions("all", 1, 30, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions with type=all, got %d", len(sessions))
	}

	// Search defaults to interactive.
	results, err := s.Search("subagent", 10, "", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for subagent search in interactive mode, got %d", len(results))
	}

	// Search with "all" should find it.
	results, err = s.Search("subagent", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected results for subagent search with type=all")
	}
}

func TestIncrementalIngest(t *testing.T) {
	projectDir := t.TempDir()

	// Write initial file with 2 messages.
	path := writeJSONL(t, projectDir, "myproject", "sess-incr", []map[string]any{
		msg("user", "first message", "2026-04-01T10:00:00Z"),
		msg("assistant", "first response", "2026-04-01T10:00:05Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.ReadSession("sess-incr", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages after first ingest, got %d", len(msgs))
	}

	// Append more messages to the same file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	enc.Encode(msg("user", "second question", "2026-04-01T10:01:00Z"))
	enc.Encode(msg("assistant", "second answer", "2026-04-01T10:01:05Z"))
	f.Close()

	// Re-ingest — should only pick up new messages.
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	msgs, err = s.ReadSession("sess-incr", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages after incremental ingest, got %d", len(msgs))
	}
}

func TestToolUseIngest(t *testing.T) {
	projectDir := t.TempDir()

	// Build a transcript with tool_use and tool_result content blocks.
	writeJSONL(t, projectDir, "myproject", "sess-tools", []map[string]any{
		{
			"type":      "user",
			"timestamp": "2026-04-01T10:00:00Z",
			"message": map[string]any{
				"role":    "user",
				"content": "Edit the store.go file",
			},
		},
		{
			"type":      "assistant",
			"timestamp": "2026-04-01T10:00:05Z",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":    "thinking",
						"thinking": "I need to edit store.go to fix the bug.",
					},
					map[string]any{
						"type": "tool_use",
						"id":   "toolu_abc123",
						"name": "Edit",
						"input": map[string]any{
							"file_path":  "/Users/dev/store.go",
							"old_string": "foo",
							"new_string": "bar",
						},
					},
				},
			},
		},
		{
			"type":      "user",
			"timestamp": "2026-04-01T10:00:10Z",
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_abc123",
						"content":     "File edited successfully.",
						"is_error":    false,
					},
				},
			},
		},
		{
			"type":      "assistant",
			"timestamp": "2026-04-01T10:00:15Z",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "tool_use",
						"id":   "toolu_def456",
						"name": "Bash",
						"input": map[string]any{
							"command":     "go test ./...",
							"description": "Run tests",
						},
					},
				},
			},
		},
		{
			"type":      "user",
			"timestamp": "2026-04-01T10:00:20Z",
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_def456",
						"content":     "FAIL: tests failed",
						"is_error":    true,
					},
				},
			},
		},
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Query for Edit tool uses via the computed file_path column.
	rows, err := s.Query("SELECT tool_name, tool_file_path FROM messages WHERE tool_name = 'Edit'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 Edit tool_use, got %d", len(rows))
	}
	if rows[0]["tool_file_path"] != "/Users/dev/store.go" {
		t.Errorf("expected file_path '/Users/dev/store.go', got %v", rows[0]["tool_file_path"])
	}

	// Query for Bash commands via the computed command column.
	rows, err = s.Query("SELECT tool_command FROM messages WHERE tool_name = 'Bash'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 Bash tool_use, got %d", len(rows))
	}
	if rows[0]["tool_command"] != "go test ./..." {
		t.Errorf("expected command 'go test ./...', got %v", rows[0]["tool_command"])
	}

	// Query for failed tool results.
	rows, err = s.Query("SELECT text, is_error FROM messages WHERE content_type = 'tool_result' AND is_error = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 failed tool_result, got %d", len(rows))
	}

	// Query for thinking blocks.
	rows, err = s.Query("SELECT text FROM messages WHERE content_type = 'thinking'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 thinking block, got %d", len(rows))
	}
	if rows[0]["text"] != "I need to edit store.go to fix the bug." {
		t.Errorf("unexpected thinking text: %v", rows[0]["text"])
	}

	// Join tool_use to tool_result via tool_use_id.
	rows, err = s.Query(`
		SELECT tu.tool_name, tr.text AS result, tr.is_error
		FROM messages tu
		JOIN messages tr ON tr.tool_use_id = tu.tool_use_id AND tr.content_type = 'tool_result'
		WHERE tu.content_type = 'tool_use'
		ORDER BY tu.id
	`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 tool_use/result pairs, got %d", len(rows))
	}
}
