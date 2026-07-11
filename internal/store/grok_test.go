// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const grokSessUUID = "019f4b93-3082-75f2-9c15-1aff0d5f2c3f"

// grokFixtureLines returns a representative Grok updates.jsonl: user,
// thought, assistant text, tool_call + completed tool_call_update,
// plus records we must skip (_x.ai hooks, intermediate tool updates).
func grokFixtureLines(t *testing.T, sessionID string) []byte {
	t.Helper()
	line := func(method string, ts int64, update map[string]any) map[string]any {
		return map[string]any{
			"timestamp": ts,
			"method":    method,
			"params": map[string]any{
				"sessionId": sessionID,
				"update":    update,
			},
		}
	}
	records := []map[string]any{
		line("_x.ai/session/update", 1783679365, map[string]any{
			"sessionUpdate": "hook_execution",
			"event_name":    "session_start",
		}),
		line("session/update", 1783679368, map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "text", "text": "How do I fix the authentication bug?"},
		}),
		line("session/update", 1783679370, map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": "User wants help with auth; inspect login handler."},
		}),
		line("session/update", 1783679371, map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "call-1",
			"title":         "run_terminal_command",
			"rawInput":      map[string]any{"command": "go test ./...", "description": "run tests"},
			"_meta": map[string]any{
				"x.ai/tool": map[string]any{"name": "run_terminal_command", "kind": "execute"},
			},
		}),
		// Intermediate update without status — must be skipped.
		line("session/update", 1783679372, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "call-1",
			"title":         "Execute go test",
			"rawInput":      map[string]any{"command": "go test ./..."},
		}),
		line("session/update", 1783679380, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "call-1",
			"status":        "completed",
			"content": []map[string]any{
				{"type": "content", "content": map[string]any{"type": "text", "text": "PASS ok package"}},
			},
			"rawOutput": map[string]any{"type": "Bash", "content": "PASS ok package"},
		}),
		line("session/update", 1783679385, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Fixed the authentication bug in the login handler."},
		}),
		line("_x.ai/session/update", 1783679390, map[string]any{
			"sessionUpdate": "turn_completed",
			"stop_reason":   "end_turn",
		}),
	}
	var b strings.Builder
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func writeGrokSession(t *testing.T, root, cwd string) string {
	t.Helper()
	// Mirror layout: sessions/<encoded-cwd>/<session-id>/updates.jsonl
	encoded := "%2FUsers%2Fdev%2Fwork%2Fgithub.com%2Facme%2Fwebapp"
	dir := filepath.Join(root, "sessions", encoded, grokSessUUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := map[string]any{
		"info": map[string]any{
			"id":  grokSessUUID,
			"cwd": cwd,
		},
		"generated_title":  "Auth bug triage",
		"session_summary":  "Auth bug triage",
		"current_model_id": "grok-4.5",
		"head_branch":      "main",
		"git_remotes":      []string{"https://github.com/acme/webapp"},
	}
	sumRaw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), sumRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Sidecar that must NOT be ingested as Claude JSONL.
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"noise"}\n`), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "updates.jsonl")
	if err := os.WriteFile(path, grokFixtureLines(t, grokSessUUID), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseGrokFile(t *testing.T) {
	cwd := "/Users/dev/work/github.com/acme/webapp"
	root := t.TempDir()
	path := writeGrokSession(t, root, cwd)

	pf, err := parseGrokFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}

	if pf.source != "grok" {
		t.Errorf("source = %q, want grok", pf.source)
	}
	if pf.sessionID != grokSessUUID {
		t.Errorf("sessionID = %q, want %q", pf.sessionID, grokSessUUID)
	}
	if pf.cwd != cwd {
		t.Errorf("cwd = %q, want %q", pf.cwd, cwd)
	}
	if pf.branch != "main" {
		t.Errorf("branch = %q, want main", pf.branch)
	}
	if pf.project != "acme/webapp" {
		t.Errorf("project = %q, want acme/webapp", pf.project)
	}
	if !strings.Contains(pf.topic, "authentication bug") {
		t.Errorf("topic = %q, want first user message (not summary title)", pf.topic)
	}

	// user, thought, tool_call, tool_result, assistant = 5 entries.
	// hook_execution, intermediate tool_call_update, turn_completed skipped.
	if len(pf.entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(pf.entries))
	}
	if len(pf.messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(pf.messages))
	}

	var first map[string]any
	if err := json.Unmarshal(pf.entries[0].raw, &first); err != nil {
		t.Fatalf("entry raw not valid JSON: %v", err)
	}
	if uuid, _ := first["uuid"].(string); !strings.HasPrefix(uuid, "grok-"+grokSessUUID+"-") {
		t.Errorf("entry uuid = %v, want grok-<session>-<offset> prefix", first["uuid"])
	}

	byType := map[string][]parsedMessage{}
	for _, m := range pf.messages {
		byType[m.contentType] = append(byType[m.contentType], m)
	}

	var sawUser, sawAssistant bool
	for _, m := range byType["text"] {
		if m.role == "user" && strings.Contains(m.text, "authentication bug") {
			sawUser = true
		}
		if m.role == "assistant" && strings.Contains(m.text, "Fixed the authentication") {
			sawAssistant = true
		}
	}
	if !sawUser || !sawAssistant {
		t.Errorf("missing user/assistant text (user=%v assistant=%v)", sawUser, sawAssistant)
	}

	if th := byType["thinking"]; len(th) != 1 || !strings.Contains(th[0].text, "auth") {
		t.Errorf("thinking = %+v", byType["thinking"])
	}

	if tu := byType["tool_use"]; len(tu) != 1 ||
		tu[0].toolName != "run_terminal_command" ||
		tu[0].toolUseID != "call-1" ||
		!json.Valid(tu[0].toolInput) ||
		!strings.Contains(string(tu[0].toolInput), "go test") {
		t.Errorf("tool_use malformed: %+v", byType["tool_use"])
	}

	if rs := byType["tool_result"]; len(rs) != 1 ||
		rs[0].toolUseID != "call-1" ||
		!strings.Contains(rs[0].text, "PASS ok package") {
		t.Errorf("tool_result = %+v", byType["tool_result"])
	}
}

func TestIsGrokUpdates(t *testing.T) {
	if !isGrokUpdates("/x/sessions/y/updates.jsonl") {
		t.Error("expected updates.jsonl to match")
	}
	if isGrokUpdates("/x/sessions/y/events.jsonl") {
		t.Error("events.jsonl must not match")
	}
	if isGrokUpdates("/x/rollout-foo.jsonl") {
		t.Error("rollout must not match")
	}
}

func TestGrokIngestEndToEnd(t *testing.T) {
	grokHome := t.TempDir()
	cwd := "/Users/dev/work/github.com/acme/webapp"
	writeGrokSession(t, grokHome, cwd)

	s := newTestStore(t, t.TempDir()) // empty Claude project dir
	s.SetGrokRoots([]string{filepath.Join(grokHome, "sessions")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search("authentication", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].SessionID != grokSessUUID {
		t.Fatalf("search did not surface the Grok session: %+v", results)
	}

	var source, repo string
	if err := s.readDB.QueryRow(
		`SELECT source, repo FROM session_meta WHERE session_id = ?`, grokSessUUID,
	).Scan(&source, &repo); err != nil {
		t.Fatalf("session_meta query: %v", err)
	}
	if source != "grok" {
		t.Errorf("session_meta.source = %q, want grok", source)
	}
	if repo != "acme/webapp" {
		t.Errorf("session_meta.repo = %q, want acme/webapp", repo)
	}

	entryCount := func() int {
		var n int
		if err := s.readDB.QueryRow(`SELECT count(*) FROM entries WHERE session_id = ?`, grokSessUUID).Scan(&n); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}
	if got := entryCount(); got != 5 {
		t.Fatalf("entries after ingest = %d, want 5", got)
	}

	// Idempotency: re-ingest must not duplicate.
	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	path := filepath.Join(grokHome, "sessions", "%2FUsers%2Fdev%2Fwork%2Fgithub.com%2Facme%2Fwebapp", grokSessUUID, "updates.jsonl")
	if err := s.ingestGrokFile(path); err != nil {
		t.Fatal(err)
	}
	if got := entryCount(); got != 5 {
		t.Errorf("entries after re-ingest = %d, want 5 (dedup)", got)
	}

	// Sidecar events.jsonl must not create a second session or garbage.
	var nSessions int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM session_meta WHERE source = 'grok'`).Scan(&nSessions); err != nil {
		t.Fatal(err)
	}
	if nSessions != 1 {
		t.Errorf("grok sessions = %d, want 1 (sidecars skipped)", nSessions)
	}
}

func TestGrokTimestamp(t *testing.T) {
	ts := grokTimestamp(json.RawMessage(`1783679368`))
	if !strings.HasPrefix(ts, "2026-") {
		t.Errorf("unix timestamp = %q, want 2026-… RFC3339", ts)
	}
	ts2 := grokTimestamp(json.RawMessage(`"2026-07-10T12:00:00Z"`))
	if ts2 != "2026-07-10T12:00:00Z" {
		t.Errorf("rfc3339 passthrough = %q", ts2)
	}
}
