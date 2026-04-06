// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// writeJSONL writes transcript entries as a JSONL file.
func writeJSONL(t *testing.T, dir, project, sessionID string, entries []map[string]any) {
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

// setupTestServer creates a store with test data, starts an RPC server
// on a temporary socket, and returns a connected client.
func setupTestServer(t *testing.T) *Client {
	t.Helper()

	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "myproject", "sess-rpc-test", []map[string]any{
		metaMsg("user", "How do I fix the authentication bug?", "2026-04-01T10:00:00Z",
			"/Users/dev/work/github.com/acme/webapp", "fix/auth-bug"),
		msg("assistant", "The bug is in the login handler.", "2026-04-01T10:00:05Z"),
		msg("user", "That fixed it, thanks!", "2026-04-01T10:01:00Z"),
		msg("assistant", "Glad it worked.", "2026-04-01T10:01:10Z"),
		msg("user", "One more question about the API", "2026-04-01T10:02:00Z"),
		msg("assistant", "Sure, what is it?", "2026-04-01T10:02:05Z"),
		msg("user", "How do I add rate limiting?", "2026-04-01T10:03:00Z"),
		msg("assistant", "Use middleware to throttle requests.", "2026-04-01T10:03:05Z"),
	})

	// Add a session with tool_use blocks.
	writeJSONL(t, projectDir, "myproject", "sess-rpc-tools", []map[string]any{
		{
			"type":      "user",
			"timestamp": "2026-04-02T10:00:00Z",
			"message":   map[string]any{"role": "user", "content": "Edit store.go"},
		},
		{
			"type":      "assistant",
			"timestamp": "2026-04-02T10:00:05Z",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_rpc1",
						"name":  "Edit",
						"input": map[string]any{"file_path": "/Users/dev/store.go", "old_string": "a", "new_string": "b"},
					},
				},
			},
		},
		{
			"type":      "user",
			"timestamp": "2026-04-02T10:00:10Z",
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_rpc1",
						"content":     "File edited.",
						"is_error":    false,
					},
				},
			},
		},
	})

	dbPath := filepath.Join(t.TempDir(), "test.db")
	mem, err := store.New(dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Use a temp socket path.
	sockPath := filepath.Join(t.TempDir(), "mnemo.sock")
	t.Setenv("MNEMO_SOCKET", sockPath)

	srv, err := NewServerAt(mem, sockPath)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		mem.Close()
	})

	client, err := DialAt(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	return client
}

func TestRPCSearch(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	results, err := proxy.Search("authentication", 10, "all", "", 1, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results")
	}
	if results[0].SessionID != "sess-rpc-test" {
		t.Errorf("expected sess-rpc-test, got %s", results[0].SessionID)
	}
	// Should have context.
	if len(results[0].After) == 0 {
		t.Error("expected context after hit")
	}
}

func TestRPCListSessions(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	sessions, err := proxy.ListSessions("all", 1, 30, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestRPCReadSession(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	msgs, err := proxy.ReadSession("sess-rpc-test", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 8 {
		t.Fatalf("expected 8 messages, got %d", len(msgs))
	}

	// Prefix resolution.
	msgs, err = proxy.ReadSession("sess-rpc-te", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected prefix resolution to work, got %d", len(msgs))
	}
}

func TestRPCQuery(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	rows, err := proxy.Query("SELECT tool_name, tool_file_path FROM messages WHERE tool_name = 'Edit'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 Edit tool_use, got %d", len(rows))
	}

	// Write queries should be rejected.
	_, err = proxy.Query("DELETE FROM messages")
	if err == nil {
		t.Error("expected error for DELETE query")
	}
}

func TestRPCStats(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	stats, err := proxy.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalSessions != 2 {
		t.Errorf("expected 2 sessions, got %d", stats.TotalSessions)
	}
	if stats.TotalMessages == 0 {
		t.Error("expected non-zero message count")
	}
}

func TestRPCSearchRepoFilter(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	// Search with repo filter — should find results.
	results, err := proxy.Search("authentication", 10, "all", "acme/webapp", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected results for repo filter 'acme/webapp'")
	}

	// Non-matching repo — should find nothing.
	results, err = proxy.Search("authentication", 10, "all", "nonexistent", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for repo 'nonexistent', got %d", len(results))
	}
}

func TestRPCSearchContextFilter(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	// Substantive-only context (default).
	results, err := proxy.Search("authentication", 10, "all", "", 3, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// All context messages should be substantive (user/assistant, non-noise).
	for _, cm := range results[0].Before {
		if cm.Role != "user" && cm.Role != "assistant" {
			t.Errorf("expected substantive context, got role %q", cm.Role)
		}
	}
	for _, cm := range results[0].After {
		if cm.Role != "user" && cm.Role != "assistant" {
			t.Errorf("expected substantive context, got role %q", cm.Role)
		}
	}
}

func TestRPCSessionMetadata(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	// Filter by repo.
	sessions, err := proxy.ListSessions("all", 1, 30, "", "acme/webapp", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for repo acme/webapp, got %d", len(sessions))
	}
	if sessions[0].Repo != "acme/webapp" {
		t.Errorf("expected repo acme/webapp, got %q", sessions[0].Repo)
	}
	if sessions[0].WorkType != "bugfix" {
		t.Errorf("expected work_type bugfix, got %q", sessions[0].WorkType)
	}
}

func TestRPCToolUseQueries(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	// Query tool_use/tool_result join.
	rows, err := proxy.Query(`
		SELECT tu.tool_name, tr.text AS result
		FROM messages tu
		JOIN messages tr ON tr.tool_use_id = tu.tool_use_id AND tr.content_type = 'tool_result'
		WHERE tu.content_type = 'tool_use'
	`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 tool_use/result pair, got %d", len(rows))
	}
	if rows[0]["tool_name"] != "Edit" {
		t.Errorf("expected tool_name Edit, got %v", rows[0]["tool_name"])
	}

	// Query via computed column.
	rows, err = proxy.Query("SELECT tool_file_path FROM messages WHERE tool_file_path IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row with file_path, got %d", len(rows))
	}
	if rows[0]["tool_file_path"] != "/Users/dev/store.go" {
		t.Errorf("expected /Users/dev/store.go, got %v", rows[0]["tool_file_path"])
	}
}

func TestRPCResolveNonceNotFound(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	_, err := proxy.ResolveNonce("mnemo:self:nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent nonce")
	}
}

func TestRPCReadSessionNotFound(t *testing.T) {
	client := setupTestServer(t)
	proxy := NewProxy(client)

	_, err := proxy.ReadSession("nonexistent-session-id", "", 0, 10)
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}
