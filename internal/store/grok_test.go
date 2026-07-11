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
const grokParentUUID = "019f4b93-0000-0000-0000-000000000001"

// grokFixtureLines returns a representative Grok updates.jsonl covering
// conversation core plus 🎯T111 extensions (recap, plan, goal, subagent).
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
		// read_file with Grok target_file alias → normalize to file_path
		line("session/update", 1783679373, map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "call-2",
			"title":         "read_file",
			"rawInput":      map[string]any{"target_file": "/tmp/auth.go", "limit": 50},
			"_meta": map[string]any{
				"x.ai/tool": map[string]any{"name": "read_file", "kind": "read"},
			},
		}),
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
		line("session/update", 1783679381, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "call-2",
			"status":        "completed",
			"content": []map[string]any{
				{"type": "content", "content": map[string]any{"type": "text", "text": "package auth"}},
			},
		}),
		line("session/update", 1783679385, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "Fixed the authentication bug in the login handler."},
		}),
		line("session/update", 1783679386, map[string]any{
			"sessionUpdate": "plan",
			"entries": []map[string]any{
				{"content": "Inspect login handler", "priority": "high", "status": "completed"},
				{"content": "Add regression test", "priority": "medium", "status": "pending"},
			},
		}),
		line("_x.ai/session/update", 1783679387, map[string]any{
			"sessionUpdate": "session_recap",
			"summary":       "Fixed authentication bug and outlined a regression test.",
			"auto":          true,
		}),
		line("_x.ai/session/update", 1783679388, map[string]any{
			"sessionUpdate": "goal_updated",
			"goal_id":       "goal-1",
			"objective":     "T99",
			"status":        "active",
			"phase":         "executing",
		}),
		line("_x.ai/session/update", 1783679389, map[string]any{
			"sessionUpdate":     "subagent_spawned",
			"subagent_id":       "child-1",
			"child_session_id":  "019f4b93-child-0000-0000-000000000002",
			"parent_session_id": sessionID,
			"subagent_type":     "general-purpose",
			"description":       "verify fix",
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
		"generated_title":   "Auth bug triage",
		"session_summary":   "Auth bug triage",
		"current_model_id":  "grok-4.5",
		"head_branch":       "main",
		"git_remotes":       []string{"https://github.com/acme/webapp.git"},
		"parent_session_id": grokParentUUID,
		"session_kind":      "subagent",
		"agent_name":        "general-purpose",
	}
	sumRaw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), sumRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	signals := map[string]any{
		"contextTokensUsed":   12345,
		"contextWindowTokens": 500000,
		"primaryModelId":      "grok-4.5",
		"toolCallCount":       2,
		"turnCount":           1,
		"compactionCount":     0,
		"gitCommitCount":      0,
	}
	sigRaw, _ := json.Marshal(signals)
	if err := os.WriteFile(filepath.Join(dir, "signals.json"), sigRaw, 0o644); err != nil {
		t.Fatal(err)
	}
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
		t.Errorf("sessionID = %q", pf.sessionID)
	}
	if pf.cwd != cwd {
		t.Errorf("cwd = %q", pf.cwd)
	}
	if pf.branch != "main" {
		t.Errorf("branch = %q", pf.branch)
	}
	// Subagent → project "subagents" for session_type trigger.
	if pf.project != "subagents" {
		t.Errorf("project = %q, want subagents", pf.project)
	}
	if pf.parentSessionID != grokParentUUID {
		t.Errorf("parentSessionID = %q", pf.parentSessionID)
	}
	if pf.model != "grok-4.5" {
		t.Errorf("model = %q", pf.model)
	}
	if !strings.Contains(pf.topic, "authentication bug") {
		t.Errorf("topic = %q", pf.topic)
	}

	// Model stamped on entry raw for generated columns.
	var sawModel bool
	for _, e := range pf.entries {
		var raw map[string]any
		if json.Unmarshal(e.raw, &raw) != nil {
			continue
		}
		if msg, ok := raw["message"].(map[string]any); ok {
			if msg["model"] == "grok-4.5" {
				sawModel = true
				break
			}
		}
	}
	if !sawModel {
		t.Error("expected message.model=grok-4.5 on at least one entry")
	}

	// Signals usage entry present.
	var sawUsage bool
	for _, m := range pf.messages {
		if strings.HasPrefix(m.text, "[grok signals]") {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Error("expected [grok signals] usage message")
	}

	// Tool input normalised: target_file → file_path
	var sawReadFilePath bool
	for _, m := range pf.messages {
		if m.contentType == "tool_use" && m.toolName == "read_file" {
			var in map[string]any
			if json.Unmarshal(m.toolInput, &in) == nil && in["file_path"] == "/tmp/auth.go" {
				sawReadFilePath = true
			}
		}
	}
	if !sawReadFilePath {
		t.Error("read_file tool_input missing normalised file_path")
	}

	// Recap / plan / goal indexed
	var sawRecap, sawPlan, sawGoal, sawSpawn bool
	for _, m := range pf.messages {
		if strings.Contains(m.text, "[grok recap") {
			sawRecap = true
		}
		if strings.Contains(m.text, "[grok plan]") {
			sawPlan = true
		}
		if strings.Contains(m.text, "[grok goal]") {
			sawGoal = true
		}
		if strings.Contains(m.text, "[grok subagent spawned]") {
			sawSpawn = true
		}
	}
	if !sawRecap || !sawPlan || !sawGoal || !sawSpawn {
		t.Errorf("extensions missing recap=%v plan=%v goal=%v spawn=%v", sawRecap, sawPlan, sawGoal, sawSpawn)
	}
}

func TestIsGrokUpdates(t *testing.T) {
	if !isGrokUpdates("/x/sessions/y/updates.jsonl") {
		t.Error("expected updates.jsonl to match")
	}
	if isGrokUpdates("/x/sessions/y/events.jsonl") {
		t.Error("events.jsonl must not match")
	}
}

func TestGrokIngestEndToEnd(t *testing.T) {
	grokHome := t.TempDir()
	cwd := "/Users/dev/work/github.com/acme/webapp"
	writeGrokSession(t, grokHome, cwd)

	s := newTestStore(t, t.TempDir())
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

	// Recap searchable
	if rec, err := s.Search("regression test", 5, "all", "", 0, 0, false); err != nil || len(rec) == 0 {
		t.Fatalf("recap/plan not searchable: %v %+v", err, rec)
	}

	var source, repo, stype string
	if err := s.readDB.QueryRow(
		`SELECT sm.source, sm.repo, ss.session_type
		 FROM session_meta sm
		 JOIN session_summary ss ON ss.session_id = sm.session_id
		 WHERE sm.session_id = ?`, grokSessUUID,
	).Scan(&source, &repo, &stype); err != nil {
		t.Fatalf("session_meta: %v", err)
	}
	if source != "grok" {
		t.Errorf("source = %q", source)
	}
	if repo != "acme/webapp" {
		t.Errorf("repo = %q, want acme/webapp", repo)
	}
	if stype != "subagent" {
		t.Errorf("session_type = %q, want subagent", stype)
	}

	// Model on assistant entries
	var model string
	if err := s.readDB.QueryRow(
		`SELECT model FROM entries WHERE session_id = ? AND type = 'assistant' AND model IS NOT NULL AND model != '' LIMIT 1`,
		grokSessUUID,
	).Scan(&model); err != nil || model != "grok-4.5" {
		t.Errorf("model = %q err=%v", model, err)
	}

	// Usage tokens from signals
	var inTok int
	if err := s.readDB.QueryRow(
		`SELECT input_tokens FROM entries WHERE session_id = ? AND input_tokens > 0 LIMIT 1`,
		grokSessUUID,
	).Scan(&inTok); err != nil || inTok != 12345 {
		t.Errorf("input_tokens = %d err=%v, want 12345", inTok, err)
	}

	// tool_file_path from normalised target_file
	var nPath int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages WHERE session_id = ? AND tool_file_path = '/tmp/auth.go'`,
		grokSessUUID,
	).Scan(&nPath); err != nil || nPath < 1 {
		t.Errorf("tool_file_path count = %d err=%v", nPath, err)
	}

	// Parent chain edge
	var pred string
	if err := s.readDB.QueryRow(
		`SELECT predecessor_id FROM session_chains WHERE successor_id = ?`, grokSessUUID,
	).Scan(&pred); err != nil || pred != grokParentUUID {
		t.Errorf("chain predecessor = %q err=%v", pred, err)
	}

	// Subagent spawn chain
	var spawnPred string
	if err := s.readDB.QueryRow(
		`SELECT predecessor_id FROM session_chains WHERE successor_id = ? AND mechanism = 'grok_subagent'`,
		"019f4b93-child-0000-0000-000000000002",
	).Scan(&spawnPred); err != nil || spawnPred != grokSessUUID {
		t.Errorf("subagent chain pred = %q err=%v", spawnPred, err)
	}

	// Idempotency
	entryCount := func() int {
		var n int
		_ = s.readDB.QueryRow(`SELECT count(*) FROM entries WHERE session_id = ?`, grokSessUUID).Scan(&n)
		return n
	}
	before := entryCount()
	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	path := filepath.Join(grokHome, "sessions", "%2FUsers%2Fdev%2Fwork%2Fgithub.com%2Facme%2Fwebapp", grokSessUUID, "updates.jsonl")
	if err := s.ingestGrokFile(path); err != nil {
		t.Fatal(err)
	}
	if got := entryCount(); got != before {
		t.Errorf("entries after re-ingest = %d, want %d", got, before)
	}
}

func TestRepoFromRemote(t *testing.T) {
	if g := repoFromRemote("git@github.com:acme/webapp.git"); g != "acme/webapp" {
		t.Errorf("ssh remote = %q", g)
	}
	if g := repoFromRemote("https://github.com/acme/webapp.git"); g != "acme/webapp" {
		t.Errorf("https remote = %q", g)
	}
}
