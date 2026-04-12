// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
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

func TestFuzzySearch(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-fuzzy1", []map[string]any{
		msg("user", "How does the QR transfer work?", "2026-04-01T10:00:00Z"),
		msg("assistant", "The QR transfer uses a token-based handoff.", "2026-04-01T10:00:05Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// "QR pairing protocol" — no message contains "pairing" or "protocol",
	// but with OR semantics, "QR" alone should match.
	results, err := s.Search("QR pairing protocol", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected fuzzy search to find partial matches via OR")
	}
	found := false
	for _, r := range results {
		if r.SessionID == "sess-fuzzy1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sess-fuzzy1 in fuzzy search results")
	}

	// Explicit AND should still require all terms — this should find nothing.
	results, err = s.Search("QR AND pairing AND protocol", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for explicit AND with missing terms, got %d", len(results))
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
						"type":     "thinking",
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

func TestSnapshotFiles(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-snap", []map[string]any{
		msg("user", "working on files", "2026-04-01T10:00:00Z"),
		{
			"type": "file-history-snapshot",
			"snapshot": map[string]any{
				"messageId": "msg-123",
				"trackedFileBackups": map[string]any{
					"internal/store/store.go": map[string]any{
						"backupFileName": "abc123@v1",
						"version":        1,
						"backupTime":     "2026-04-01T10:00:05Z",
					},
					"main.go": map[string]any{
						"backupFileName": "def456@v1",
						"version":        1,
						"backupTime":     "2026-04-01T10:00:06Z",
					},
				},
				"timestamp": "2026-04-01T10:00:05Z",
			},
			"isSnapshotUpdate": false,
		},
		// Empty snapshot (no tracked files).
		{
			"type": "file-history-snapshot",
			"snapshot": map[string]any{
				"messageId":          "msg-456",
				"trackedFileBackups": map[string]any{},
				"timestamp":          "2026-04-01T10:01:00Z",
			},
		},
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Two files should be extracted from the first snapshot.
	rows, err := s.Query("SELECT COUNT(*) AS cnt FROM snapshot_files")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, ok := rows[0]["cnt"].(int64); !ok || cnt != 2 {
		t.Fatalf("expected 2 snapshot_files, got %v", rows[0]["cnt"])
	}

	// Check file paths.
	rows, err = s.Query("SELECT file_path, backup_time FROM snapshot_files ORDER BY file_path")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0]["file_path"] != "internal/store/store.go" {
		t.Errorf("expected internal/store/store.go, got %v", rows[0]["file_path"])
	}
	if rows[1]["file_path"] != "main.go" {
		t.Errorf("expected main.go, got %v", rows[1]["file_path"])
	}
	if rows[0]["backup_time"] != "2026-04-01T10:00:05Z" {
		t.Errorf("expected backup_time, got %v", rows[0]["backup_time"])
	}

	// FTS search for store.go.
	rows, err = s.Query("SELECT file_path FROM snapshot_files WHERE id IN (SELECT rowid FROM snapshot_files_fts WHERE snapshot_files_fts MATCH 'store')")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 FTS match, got %d", len(rows))
	}
	if rows[0]["file_path"] != "internal/store/store.go" {
		t.Errorf("expected store.go from FTS, got %v", rows[0]["file_path"])
	}

	// Join with session_meta.
	rows, err = s.Query(`
		SELECT sf.file_path, sf.session_id
		FROM snapshot_files sf
		WHERE sf.file_path LIKE '%main.go'
	`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for main.go, got %d", len(rows))
	}
	if rows[0]["session_id"] != "sess-snap" {
		t.Errorf("expected session sess-snap, got %v", rows[0]["session_id"])
	}
}

func TestEntriesTable(t *testing.T) {
	projectDir := t.TempDir()

	// Build a transcript with multiple entry types: user, assistant (with model/usage),
	// progress, and system.
	writeJSONL(t, projectDir, "myproject", "sess-entries", []map[string]any{
		// System entry (gitStatus).
		{
			"type":      "system",
			"timestamp": "2026-04-01T10:00:00Z",
			"cwd":       "/Users/dev/work/github.com/acme/webapp",
			"gitBranch": "feature/entries",
			"version":   "2.1.81",
			"message":   map[string]any{"content": "system init"},
		},
		// User message.
		{
			"type":      "user",
			"timestamp": "2026-04-01T10:00:01Z",
			"cwd":       "/Users/dev/work/github.com/acme/webapp",
			"gitBranch": "feature/entries",
			"version":   "2.1.81",
			"message":   map[string]any{"content": "Build the new feature"},
		},
		// Assistant with model and usage.
		{
			"type":      "assistant",
			"timestamp": "2026-04-01T10:00:05Z",
			"cwd":       "/Users/dev/work/github.com/acme/webapp",
			"gitBranch": "feature/entries",
			"version":   "2.1.81",
			"slug":      "myproject-sess-entries",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "I'll build the feature now.",
					},
				},
				"model":       "claude-opus-4-6",
				"stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":                50000,
					"output_tokens":               500,
					"cache_read_input_tokens":     45000,
					"cache_creation_input_tokens": 1000,
				},
			},
		},
		// Progress event (bash).
		{
			"type":            "progress",
			"timestamp":       "2026-04-01T10:00:10Z",
			"parentToolUseID": "toolu_abc123",
			"agentId":         "agent-xyz",
			"data": map[string]any{
				"type":    "bash_progress",
				"command": "make build",
				"output":  "Building...",
			},
		},
		// Progress event (hook).
		{
			"type":            "progress",
			"timestamp":       "2026-04-01T10:00:15Z",
			"parentToolUseID": "toolu_abc123",
			"data": map[string]any{
				"type":      "hook_progress",
				"hookEvent": "PostToolUse",
				"command":   "python3 hook.py",
			},
		},
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// All 5 JSONL lines should be in entries.
	rows, err := s.Query("SELECT COUNT(*) AS cnt FROM entries")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, ok := rows[0]["cnt"].(int64); !ok || cnt != 5 {
		t.Fatalf("expected 5 entries, got %v", rows[0]["cnt"])
	}

	// Verify entry types.
	rows, err = s.Query("SELECT type, COUNT(*) AS cnt FROM entries GROUP BY type ORDER BY type")
	if err != nil {
		t.Fatal(err)
	}
	typeCounts := map[string]int64{}
	for _, r := range rows {
		typeCounts[r["type"].(string)] = r["cnt"].(int64)
	}
	if typeCounts["assistant"] != 1 {
		t.Errorf("expected 1 assistant entry, got %d", typeCounts["assistant"])
	}
	if typeCounts["progress"] != 2 {
		t.Errorf("expected 2 progress entries, got %d", typeCounts["progress"])
	}
	if typeCounts["system"] != 1 {
		t.Errorf("expected 1 system entry, got %d", typeCounts["system"])
	}

	// Verify virtual columns on assistant entry.
	rows, err = s.Query("SELECT model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, version, slug FROM entries WHERE type = 'assistant'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 assistant entry, got %d", len(rows))
	}
	r := rows[0]
	if r["model"] != "claude-opus-4-6" {
		t.Errorf("expected model claude-opus-4-6, got %v", r["model"])
	}
	if r["stop_reason"] != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %v", r["stop_reason"])
	}
	if r["input_tokens"] != int64(50000) {
		t.Errorf("expected input_tokens 50000, got %v", r["input_tokens"])
	}
	if r["output_tokens"] != int64(500) {
		t.Errorf("expected output_tokens 500, got %v", r["output_tokens"])
	}
	if r["cache_read_tokens"] != int64(45000) {
		t.Errorf("expected cache_read_tokens 45000, got %v", r["cache_read_tokens"])
	}
	if r["cache_creation_tokens"] != int64(1000) {
		t.Errorf("expected cache_creation_tokens 1000, got %v", r["cache_creation_tokens"])
	}
	if r["version"] != "2.1.81" {
		t.Errorf("expected version 2.1.81, got %v", r["version"])
	}
	if r["slug"] != "myproject-sess-entries" {
		t.Errorf("expected slug myproject-sess-entries, got %v", r["slug"])
	}

	// Verify progress event virtual columns.
	rows, err = s.Query("SELECT data_type, agent_id, parent_tool_use_id FROM entries WHERE type = 'progress' AND data_type = 'bash_progress'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 bash_progress entry, got %d", len(rows))
	}
	if rows[0]["agent_id"] != "agent-xyz" {
		t.Errorf("expected agent_id agent-xyz, got %v", rows[0]["agent_id"])
	}
	if rows[0]["parent_tool_use_id"] != "toolu_abc123" {
		t.Errorf("expected parent_tool_use_id toolu_abc123, got %v", rows[0]["parent_tool_use_id"])
	}

	// Verify hook progress.
	rows, err = s.Query("SELECT data_type, data_hook_event, data_command FROM entries WHERE data_type = 'hook_progress'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 hook_progress entry, got %d", len(rows))
	}
	if rows[0]["data_hook_event"] != "PostToolUse" {
		t.Errorf("expected hookEvent PostToolUse, got %v", rows[0]["data_hook_event"])
	}
	if rows[0]["data_command"] != "python3 hook.py" {
		t.Errorf("expected command 'python3 hook.py', got %v", rows[0]["data_command"])
	}

	// Only user/assistant should produce messages (2 entries → content blocks).
	rows, err = s.Query("SELECT COUNT(*) AS cnt FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, ok := rows[0]["cnt"].(int64); !ok || cnt != 2 {
		t.Fatalf("expected 2 messages (user+assistant), got %v", rows[0]["cnt"])
	}

	// Messages should be linked to entries via entry_id.
	rows, err = s.Query(`
		SELECT m.text, e.model, e.version
		FROM messages m
		JOIN entries e ON e.id = m.entry_id
		WHERE m.role = 'assistant'
	`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 joined row, got %d", len(rows))
	}
	if rows[0]["model"] != "claude-opus-4-6" {
		t.Errorf("expected model from join, got %v", rows[0]["model"])
	}

	// Verify raw JSON access via json_extract.
	rows, err = s.Query("SELECT json_extract(raw, '$.data.output') AS output FROM entries WHERE data_type = 'bash_progress'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for raw json_extract, got %d", len(rows))
	}
	if rows[0]["output"] != "Building..." {
		t.Errorf("expected raw output 'Building...', got %v", rows[0]["output"])
	}
}

func TestSchemaVersionRebuild(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-rebuild", []map[string]any{
		msg("user", "hello from the first database", "2026-04-01T10:00:00Z"),
		msg("assistant", "hi there", "2026-04-01T10:00:05Z"),
	})

	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Create a store — this sets the current schema version.
	s, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	results, err := s.Search("hello", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results before rebuild")
	}
	s.Close()

	// Manually set a stale schema version to simulate an old database.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("PRAGMA user_version = 1")
	db.Close()

	// Re-open — should detect mismatch, blow away database, rebuild.
	s, err = New(dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Database was rebuilt — no data until we re-ingest.
	stats, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalMessages != 0 {
		t.Errorf("expected 0 messages after schema rebuild, got %d", stats.TotalMessages)
	}

	// Re-ingest and verify data is back.
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	results, err = s.Search("hello", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results after re-ingest")
	}
}

func TestMemoryIngest(t *testing.T) {
	projectDir := t.TempDir()

	// Create a memory directory structure mimicking ~/.claude/projects/<project>/memory/
	memDir := filepath.Join(projectDir, "myproject", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a memory file with frontmatter.
	memContent := `---
name: QR transfer protocol
description: Design decisions for QR-based session transfer in HMS
type: project
---

The QR transfer uses a token-based handoff. The phone scans the QR code
which contains a transfer token. The server validates the token and
creates a new session for the mobile device.
`
	if err := os.WriteFile(filepath.Join(memDir, "qr_transfer.md"), []byte(memContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write a MEMORY.md index file.
	indexContent := "- [QR transfer](qr_transfer.md) — QR-based session transfer design\n"
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(indexContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Also need a JSONL file so the store has a valid project.
	writeJSONL(t, projectDir, "myproject", "sess-mem1", []map[string]any{
		msg("user", "hello", "2026-04-01T10:00:00Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestMemories(); err != nil {
		t.Fatal(err)
	}

	// Search for "QR transfer" should find the memory.
	results, err := s.SearchMemories("QR transfer", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected memory search results for 'QR transfer'")
	}
	for i, r := range results {
		t.Logf("result[%d]: name=%q type=%q project=%q path=%q content=%.60q", i, r.Name, r.MemoryType, r.Project, r.FilePath, r.Content)
	}
	// Find the actual qr_transfer.md result (MEMORY.md index may also match).
	found := false
	for _, r := range results {
		if r.Name == "QR transfer protocol" {
			found = true
			if r.MemoryType != "project" {
				t.Errorf("expected type 'project', got %q", r.MemoryType)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find memory 'QR transfer protocol'")
	}

	// Search for "pairing" — not in the memory, but "transfer" is.
	// With OR semantics, "pairing transfer" should still find it via "transfer".
	results, err = s.SearchMemories("pairing transfer", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected fuzzy memory search to find partial match")
	}

	// Filter by type.
	results, err = s.SearchMemories("", "project", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for type filter 'project'")
	}

	// Filter by type — no match.
	results, err = s.SearchMemories("", "reference", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for type 'reference', got %d", len(results))
	}

	// Update the memory file and re-ingest.
	updatedContent := `---
name: QR transfer protocol (revised)
description: Updated design for QR-based session transfer
type: project
---

The revised protocol uses ECDH key exchange after QR scan.
`
	if err := os.WriteFile(filepath.Join(memDir, "qr_transfer.md"), []byte(updatedContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestMemories(); err != nil {
		t.Fatal(err)
	}

	results, err = s.SearchMemories("ECDH", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for updated content 'ECDH'")
	}
	if results[0].Name != "QR transfer protocol (revised)" {
		t.Errorf("expected updated name, got %q", results[0].Name)
	}
}

func TestRelaxQuery(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		// Single word — unchanged.
		{"hello", "hello"},
		// Multiple words — OR-joined.
		{"QR code pairing", "QR OR code OR pairing"},
		// Explicit OR — unchanged.
		{"QR OR pairing", "QR OR pairing"},
		// Explicit AND — unchanged.
		{"QR AND pairing", "QR AND pairing"},
		// Explicit NOT — unchanged.
		{"QR NOT test", "QR NOT test"},
		// Quoted phrase — unchanged.
		{`"QR transfer"`, `"QR transfer"`},
		// NEAR — unchanged.
		{"NEAR(QR transfer)", "NEAR(QR transfer)"},
		// Case-insensitive operator detection.
		{"hello or world", "hello or world"},
		// Empty — unchanged.
		{"", ""},
		// Whitespace only.
		{"   ", ""},
	}
	for _, tt := range tests {
		got := relaxQuery(tt.input)
		if got != tt.want {
			t.Errorf("relaxQuery(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSkillIngest(t *testing.T) {
	projectDir := t.TempDir()
	skillsDir := t.TempDir()

	// Write a skill file with YAML frontmatter.
	skillContent := `---
name: release workflow
description: Steps to publish a new release with CI and Homebrew tap
---

1. Bump the version constant in main.go.
2. Run the release CI workflow via GitHub Actions.
3. Update the Homebrew tap formula with the new sha256 checksums.
`
	if err := os.WriteFile(filepath.Join(skillsDir, "release.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write a skill file without frontmatter — name derived from filename.
	plainContent := `Use this skill to audit the codebase for code quality,
security issues, and documentation gaps.`
	if err := os.WriteFile(filepath.Join(skillsDir, "audit-codebase.md"), []byte(plainContent), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t, projectDir)

	// Directly ingest from the temp skills dir using ingestSkillFileLocked.
	s.rwmu.Lock()
	for _, name := range []string{"release.md", "audit-codebase.md"} {
		if err := s.ingestSkillFileLocked(filepath.Join(skillsDir, name)); err != nil {
			s.rwmu.Unlock()
			t.Fatalf("ingest skill %s: %v", name, err)
		}
	}
	s.rwmu.Unlock()

	// Search for "release" should find the release skill.
	results, err := s.SearchSkills("release", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'release'")
	}
	found := false
	for _, r := range results {
		t.Logf("result: name=%q description=%.60q", r.Name, r.Description)
		if r.Name == "release workflow" {
			found = true
			if r.Description != "Steps to publish a new release with CI and Homebrew tap" {
				t.Errorf("unexpected description: %q", r.Description)
			}
		}
	}
	if !found {
		t.Error("expected to find skill 'release workflow'")
	}

	// Filename-derived name for the plain skill.
	results, err = s.SearchSkills("audit codebase", 10)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range results {
		if r.Name == "audit codebase" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to find skill 'audit codebase'")
	}

	// List all (empty query).
	results, err = s.SearchSkills("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Errorf("expected at least 2 skills, got %d", len(results))
	}

	// Update a skill file and re-ingest.
	updatedContent := `---
name: release workflow
description: Updated release steps including signing
---

Updated content.
`
	if err := os.WriteFile(filepath.Join(skillsDir, "release.md"), []byte(updatedContent), 0o644); err != nil {
		t.Fatal(err)
	}
	s.rwmu.Lock()
	if err := s.ingestSkillFileLocked(filepath.Join(skillsDir, "release.md")); err != nil {
		s.rwmu.Unlock()
		t.Fatal(err)
	}
	s.rwmu.Unlock()

	results, err = s.SearchSkills("signing", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected updated skill to be searchable by new description")
	}

	// Delete the skill.
	s.deleteSkillFile(filepath.Join(skillsDir, "release.md"))
	results, err = s.SearchSkills("release workflow", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Name == "release workflow" {
			t.Error("expected deleted skill to be removed from index")
		}
	}
}

func TestClaudeConfigIngest(t *testing.T) {
	projectDir := t.TempDir()

	// Create a fake repo directory with a .git marker and CLAUDE.md.
	repoDir := filepath.Join(t.TempDir(), "work", "github.com", "acme", "myrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeContent := `# myrepo

## Build & Run

` + "```bash\nmake build\n```" + `

## Delivery

Merged to master via squash PR.
`
	if err := os.WriteFile(filepath.Join(repoDir, "CLAUDE.md"), []byte(claudeContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write a session_meta row pointing cwd into the repo.
	writeJSONL(t, projectDir, "myproject", "sess-cfg1", []map[string]any{
		metaMsg("user", "let's build this", "2026-04-01T10:00:00Z",
			filepath.Join(repoDir, "internal", "store"), "master"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestClaudeConfigs(); err != nil {
		t.Fatal(err)
	}

	// Search for "squash" should find the CLAUDE.md.
	results, err := s.SearchClaudeConfigs("squash", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'squash' in CLAUDE.md")
	}
	found := false
	for _, r := range results {
		if r.FilePath == filepath.Join(repoDir, "CLAUDE.md") {
			found = true
			if r.Repo == "" {
				t.Error("expected non-empty repo in ClaudeConfigInfo")
			}
			t.Logf("found config: repo=%q path=%q content=%.80q", r.Repo, r.FilePath, r.Content)
			break
		}
	}
	if !found {
		t.Errorf("expected to find config at %s", filepath.Join(repoDir, "CLAUDE.md"))
	}

	// List all (no query).
	all, err := s.SearchClaudeConfigs("", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("expected at least one config in list-all mode")
	}

	// Repo filter — should find by fragment.
	filtered, err := s.SearchClaudeConfigs("", "myrepo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) == 0 {
		t.Fatal("expected results for repo filter 'myrepo'")
	}

	// Repo filter — no match.
	none, err := s.SearchClaudeConfigs("", "doesnotexist", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("expected no results for repo filter 'doesnotexist', got %d", len(none))
	}

	// Update the CLAUDE.md and re-ingest.
	updatedContent := claudeContent + "\n## Gates\nprofile: library\n"
	if err := os.WriteFile(filepath.Join(repoDir, "CLAUDE.md"), []byte(updatedContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestClaudeConfigs(); err != nil {
		t.Fatal(err)
	}

	results, err = s.SearchClaudeConfigs("library", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for updated content 'library'")
	}
}

func TestAuditLogIngest(t *testing.T) {
	projectDir := t.TempDir()

	// Create a fake repo with a .git marker and docs/audit-log.md.
	repoDir := filepath.Join(t.TempDir(), "myorg", "myrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	docsDir := filepath.Join(repoDir, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	auditContent := `# Audit Log

Chronological record of maintenance activities.

## 2026-04-06 — /release v0.1.0

- **Commit**: ` + "`" + `abc123` + "`" + `
- **Outcome**: Released v0.1.0 with initial features.

## 2026-04-07 — /audit

- Reviewed code quality and security.
- No critical issues found.

## 2026-04-08 — /release v0.2.0

- **Commit**: ` + "`" + `def456` + "`" + `
- **Outcome**: Released v0.2.0 with bug fixes.
`
	auditPath := filepath.Join(docsDir, "audit-log.md")
	if err := os.WriteFile(auditPath, []byte(auditContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a session with cwd pointing to the repo so IngestAuditLogs can discover it.
	writeJSONL(t, projectDir, "myproject", "sess-audit", []map[string]any{
		metaMsg("user", "working on repo", "2026-04-08T10:00:00Z",
			repoDir, "master"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestAuditLogs(); err != nil {
		t.Fatal(err)
	}

	// Should have 3 entries.
	rows, err := s.Query("SELECT COUNT(*) AS cnt FROM audit_entries")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, ok := rows[0]["cnt"].(int64); !ok || cnt != 3 {
		t.Fatalf("expected 3 audit entries, got %v", rows[0]["cnt"])
	}

	// Verify parsed fields for the first release entry.
	results, err := s.SearchAuditLogs("", "", "release", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 release entries, got %d", len(results))
	}
	// Results are ordered by date DESC.
	first := results[0]
	if first.Date != "2026-04-08" {
		t.Errorf("expected date 2026-04-08, got %q", first.Date)
	}
	if first.Skill != "release" {
		t.Errorf("expected skill 'release', got %q", first.Skill)
	}
	if first.Version != "v0.2.0" {
		t.Errorf("expected version v0.2.0, got %q", first.Version)
	}

	// Verify the audit entry (no version).
	auditResults, err := s.SearchAuditLogs("", "", "audit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditResults) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(auditResults))
	}
	if auditResults[0].Version != "" {
		t.Errorf("expected empty version for audit entry, got %q", auditResults[0].Version)
	}
	if auditResults[0].Date != "2026-04-07" {
		t.Errorf("expected date 2026-04-07, got %q", auditResults[0].Date)
	}

	// FTS search.
	ftsResults, err := s.SearchAuditLogs("bug fixes", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ftsResults) == 0 {
		t.Fatal("expected FTS search to find 'bug fixes'")
	}
	found := false
	for _, r := range ftsResults {
		if r.Version == "v0.2.0" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to find v0.2.0 entry via FTS 'bug fixes'")
	}

	// Filter by repo.
	repoResults, err := s.SearchAuditLogs("", "myrepo", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(repoResults) != 3 {
		t.Fatalf("expected 3 results for repo filter 'myrepo', got %d", len(repoResults))
	}

	// Re-ingest should replace all entries (not duplicate them).
	if err := s.IngestAuditLogs(); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Query("SELECT COUNT(*) AS cnt FROM audit_entries")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, ok := rows[0]["cnt"].(int64); !ok || cnt != 3 {
		t.Fatalf("expected 3 audit entries after re-ingest (no duplicates), got %v", rows[0]["cnt"])
	}
}

func TestTargetIngest(t *testing.T) {
	// Create a fake repo with .git and docs/targets.md.
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	targetsContent := `# Convergence Targets

### 🎯T1 All tests pass on CI

- **Status**: converging
- **Weight**: 8

The CI pipeline should run all unit and integration tests and report green
on every pull request before merge.

### 🎯T2 Documentation is complete

- **Status**: identified
- **Weight**: 5

All public APIs and user-facing features must have documentation.

### 🎯T3 Achieved target example

- **Status**: achieved
- **Weight**: 3

This target has already been completed.
`
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "targets.md"), []byte(targetsContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a session that uses this repo as cwd.
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "myproject", "sess-targets", []map[string]any{
		metaMsg("user", "working on CI", "2026-04-01T10:00:00Z", repoDir, "master"),
		msg("assistant", "OK, I'll look at CI.", "2026-04-01T10:00:05Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestTargets(); err != nil {
		t.Fatal(err)
	}

	// List all targets.
	results, err := s.SearchTargets("", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(results))
	}

	// Find T1 and verify parsed fields.
	var t1 *TargetInfo
	for i := range results {
		if results[i].TargetID == "🎯T1" {
			t1 = &results[i]
			break
		}
	}
	if t1 == nil {
		t.Fatal("expected to find 🎯T1")
	}
	if t1.Name != "All tests pass on CI" {
		t.Errorf("T1 name = %q, want %q", t1.Name, "All tests pass on CI")
	}
	if t1.Status != "converging" {
		t.Errorf("T1 status = %q, want %q", t1.Status, "converging")
	}
	if t1.Weight != 8 {
		t.Errorf("T1 weight = %v, want 8", t1.Weight)
	}
	if t1.Description == "" {
		t.Error("T1 description should not be empty")
	}
	if t1.RawText == "" {
		t.Error("T1 raw_text should not be empty")
	}
	if t1.Repo == "" {
		t.Error("T1 repo should not be empty")
	}

	// Filter by status.
	results, err = s.SearchTargets("", "", "identified", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 target with status=identified, got %d", len(results))
	}
	if results[0].TargetID != "🎯T2" {
		t.Errorf("expected 🎯T2, got %s", results[0].TargetID)
	}

	// Filter by status=achieved.
	results, err = s.SearchTargets("", "", "achieved", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].TargetID != "🎯T3" {
		t.Errorf("expected 🎯T3 with status=achieved, got %v", results)
	}

	// FTS search.
	results, err = s.SearchTargets("documentation APIs", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected FTS results for 'documentation APIs'")
	}
}

func TestPlanIngest(t *testing.T) {
	projectDir := t.TempDir()

	// Create a fake repo with a .git directory to make findRepoRoot work.
	repoRoot := filepath.Join(projectDir, "work", "github.com", "testorg", "myrepo")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create .planning/ structure:
	//   .planning/phase-1/PLAN.md
	//   .planning/milestone-v2/phase-1/PLAN.md
	//   .planning/OVERVIEW.md
	planDir := filepath.Join(repoRoot, ".planning")
	if err := os.MkdirAll(filepath.Join(planDir, "phase-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(planDir, "milestone-v2", "phase-1"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(planDir, "phase-1", "PLAN.md"),
		"# Phase 1\n\nImplement the widget factory using dependency injection.\n")
	writeFile(filepath.Join(planDir, "milestone-v2", "phase-1", "PLAN.md"),
		"# Milestone v2 Phase 1\n\nRefactor widget factory for performance.\n")
	writeFile(filepath.Join(planDir, "OVERVIEW.md"),
		"# Overview\n\nHigh-level architecture for the widget system.\n")

	// Seed session_meta so IngestPlans can discover the repo root.
	writeJSONL(t, projectDir, "testorg-myrepo", "sess-plan1", []map[string]any{
		msg("user", "hello", "2026-04-01T10:00:00Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Manually insert a session_meta row pointing at the repo.
	if _, err := s.db.Exec(
		"INSERT OR IGNORE INTO session_meta (session_id, cwd) VALUES (?, ?)",
		"sess-plan1", repoRoot,
	); err != nil {
		t.Fatal(err)
	}

	if err := s.IngestPlans(); err != nil {
		t.Fatal(err)
	}

	// Search for "widget factory" — should find phase-1 PLAN.md.
	results, err := s.SearchPlans("widget factory", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected plan search results for 'widget factory'")
	}
	t.Logf("found %d results", len(results))

	// Verify phase parsing.
	phaseMap := map[string]string{}
	for _, r := range results {
		t.Logf("  file=%q phase=%q repo=%q", r.FilePath, r.Phase, r.Repo)
		phaseMap[r.Phase] = r.FilePath
	}

	// phase-1/PLAN.md → phase "1"
	if _, ok := phaseMap["1"]; !ok {
		t.Errorf("expected phase '1' in results, got: %v", phaseMap)
	}

	// List all plans (no query).
	all, err := s.SearchPlans("", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Errorf("expected at least 3 plans (3 files), got %d", len(all))
	}

	// Check milestone-v2/phase-1 → phase "v2/1"
	foundMilestone := false
	for _, r := range all {
		if r.Phase == "v2/1" {
			foundMilestone = true
		}
	}
	if !foundMilestone {
		t.Error("expected phase 'v2/1' for milestone-v2/phase-1/PLAN.md")
	}

	// Filter by repo.
	filtered, err := s.SearchPlans("", "testorg/myrepo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) == 0 {
		t.Error("expected results when filtering by repo 'testorg/myrepo'")
	}

	// Fuzzy OR search — "performance architecture" should match via OR.
	fuzzy, err := s.SearchPlans("performance architecture", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fuzzy) == 0 {
		t.Error("expected fuzzy OR results for 'performance architecture'")
	}
}

func TestUsageHourlyRate(t *testing.T) {
	projectDir := t.TempDir()

	// Create a session with multiple assistant messages spread over time.
	// Messages at T+0, T+10min, T+20min → 20 minutes of active time.
	writeJSONL(t, projectDir, "myproject", "sess-usage-rate", []map[string]any{
		{
			"type":      "system",
			"timestamp": now(),
			"cwd":       "/Users/dev/work/github.com/acme/app",
			"version":   "2.1.81",
			"message":   map[string]any{"content": "system init"},
		},
		assistantWithUsage(now(), "claude-opus-4-6", 50000, 500, 45000, 1000),
		assistantWithUsage(nowPlus(10), "claude-opus-4-6", 60000, 600, 50000, 2000),
		assistantWithUsage(nowPlus(20), "claude-opus-4-6", 40000, 400, 35000, 500),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	result, err := s.Usage(30, "", "", "day")
	if err != nil {
		t.Fatal(err)
	}

	if result.HourlyRate == nil {
		t.Fatal("expected HourlyRate to be populated")
	}

	hr := result.HourlyRate
	// 20 minutes ≈ 0.333 hours of active time.
	if hr.ActiveHours < 0.3 || hr.ActiveHours > 0.4 {
		t.Errorf("expected ~0.333 active hours, got %f", hr.ActiveHours)
	}

	// Total input: 150000 tokens over ~0.333h → ~450000/h
	if hr.InputPerHour < 400000 || hr.InputPerHour > 500000 {
		t.Errorf("expected input/hour ~450000, got %f", hr.InputPerHour)
	}

	if hr.CostPerHour <= 0 {
		t.Error("expected positive cost per hour")
	}

	if hr.MessagesPerHour <= 0 {
		t.Error("expected positive messages per hour")
	}
}

// assistantWithUsage creates an assistant entry with usage fields at the given timestamp.
func assistantWithUsage(ts string, model string, input, output, cacheRead, cacheCreate int) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"cwd":       "/Users/dev/work/github.com/acme/app",
		"version":   "2.1.81",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "Working on it."},
			},
			"model":       model,
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":                input,
				"output_tokens":               output,
				"cache_read_input_tokens":     cacheRead,
				"cache_creation_input_tokens": cacheCreate,
			},
		},
	}
}

// now returns the current time formatted for JSONL timestamps.
func now() string {
	return "2026-04-09T10:00:00Z"
}

// nowPlus returns a timestamp offset by the given minutes.
func nowPlus(minutes int) string {
	return "2026-04-09T10:" + padMinutes(minutes) + ":00Z"
}

func padMinutes(m int) string {
	if m < 10 {
		return "0" + string(rune('0'+m))
	}
	return string(rune('0'+m/10)) + string(rune('0'+m%10))
}
