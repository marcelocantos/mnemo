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

const codexSessUUID = "019ee2a8-189f-7540-a035-1cc6ee7bd02f"
const codexParentUUID = "019ee2a8-0000-0000-0000-0000000000aa"

// codexFixtureLines returns a representative Codex rollout: header,
// per-turn context, user/assistant messages, a function call + output,
// an apply_patch custom tool call, encrypted reasoning, token_count usage
// (🎯T112), plus records we must skip (a developer instruction message).
func codexFixtureLines(t *testing.T, cwd string) []byte {
	t.Helper()
	line := func(typ string, payload map[string]any) map[string]any {
		return map[string]any{"timestamp": "2026-06-30T10:00:00.000Z", "type": typ, "payload": payload}
	}
	records := []map[string]any{
		line("session_meta", map[string]any{
			"id":               codexSessUUID,
			"cwd":              cwd,
			"parent_thread_id": codexParentUUID,
			"git":              map[string]any{"branch": "main", "repository_url": "https://github.com/acme/webapp"},
		}),
		line("turn_context", map[string]any{"cwd": cwd, "model": "gpt-5.5"}),
		line("response_item", map[string]any{
			"type": "message", "role": "developer",
			"content": []map[string]any{{"type": "input_text", "text": "<permissions instructions> ... long system blob ..."}},
		}),
		line("response_item", map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "How do I fix the authentication bug?"}},
		}),
		line("response_item", map[string]any{
			"type": "reasoning", "summary": []map[string]any{}, "encrypted_content": "gAAAAABopaque",
		}),
		line("response_item", map[string]any{
			// Real Codex uses exec_command with cmd=string; also cover array form.
			"type": "function_call", "name": "exec_command",
			"arguments": `{"cmd":"go build ./...","workdir":"/tmp/webapp"}`, "call_id": "call_1",
		}),
		line("event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"last_token_usage": map[string]any{
					"input_tokens": 100, "cached_input_tokens": 40,
					"output_tokens": 20, "reasoning_output_tokens": 5, "total_tokens": 125,
				},
				"total_token_usage": map[string]any{
					"input_tokens": 100, "output_tokens": 25, "total_tokens": 125,
				},
				"model_context_window": 258400,
			},
		}),
		line("response_item", map[string]any{
			"type": "function_call_output", "call_id": "call_1", "output": "build succeeded",
		}),
		line("response_item", map[string]any{
			"type": "custom_tool_call", "name": "apply_patch",
			"input": "*** Begin Patch\n*** Update File: main.go\n*** End Patch\n", "call_id": "call_2",
		}),
		line("response_item", map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "Fixed the authentication bug in the login handler."}},
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

func shellToolInput(m *parsedMessage) string {
	if m == nil {
		return ""
	}
	return string(m.toolInput)
}

func writeCodexRollout(t *testing.T, dir, cwd string) string {
	t.Helper()
	day := filepath.Join(dir, "sessions", "2026", "06", "30")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(day, "rollout-2026-06-30T10-00-00-"+codexSessUUID+".jsonl")
	if err := os.WriteFile(path, codexFixtureLines(t, cwd), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseCodexFile(t *testing.T) {
	cwd := "/Users/dev/work/github.com/acme/webapp"
	dir := t.TempDir()
	path := writeCodexRollout(t, dir, cwd)

	pf, err := parseCodexFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}

	if pf.source != "codex" {
		t.Errorf("source = %q, want codex", pf.source)
	}
	if pf.sessionID != codexSessUUID {
		t.Errorf("sessionID = %q, want %q", pf.sessionID, codexSessUUID)
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
		t.Errorf("topic = %q, want first user message", pf.topic)
	}

	// 6 response_items + 1 token_count usage entry; developer message,
	// session_meta and turn_context are not conversation entries.
	if len(pf.entries) != 7 {
		t.Fatalf("entries = %d, want 7", len(pf.entries))
	}
	if len(pf.messages) != 7 {
		t.Fatalf("messages = %d, want 7", len(pf.messages))
	}
	if pf.parentSessionID != codexParentUUID {
		t.Errorf("parentSessionID = %q, want %q", pf.parentSessionID, codexParentUUID)
	}
	if pf.chainMechanism != "codex_parent" {
		t.Errorf("chainMechanism = %q, want codex_parent", pf.chainMechanism)
	}
	if pf.model != "gpt-5.5" {
		t.Errorf("model = %q, want gpt-5.5", pf.model)
	}
	// Model stamped on conversation entries.
	var sawModel bool
	for _, e := range pf.entries {
		var raw map[string]any
		if json.Unmarshal(e.raw, &raw) != nil {
			continue
		}
		if msg, _ := raw["message"].(map[string]any); msg != nil && msg["model"] == "gpt-5.5" {
			sawModel = true
			break
		}
	}
	if !sawModel {
		t.Error("expected message.model=gpt-5.5 on at least one entry")
	}
	// token_count → searchable usage note
	var sawUsage bool
	for _, m := range pf.messages {
		if strings.Contains(m.text, "[codex tokens]") && strings.Contains(m.text, "input=100") {
			sawUsage = true
			break
		}
	}
	if !sawUsage {
		t.Error("expected [codex tokens] usage message from token_count")
	}

	// The first entry's raw must carry the injected synthetic uuid so
	// (session_id, uuid) dedup works; Codex records have none natively.
	var first map[string]any
	if err := json.Unmarshal(pf.entries[0].raw, &first); err != nil {
		t.Fatalf("entry raw not valid JSON: %v", err)
	}
	if uuid, _ := first["uuid"].(string); !strings.HasPrefix(uuid, "codex-"+codexSessUUID+"-") {
		t.Errorf("entry uuid = %v, want codex-<session>-<offset> prefix", first["uuid"])
	}

	// Index the messages by a (contentType, role) probe for assertions.
	byType := map[string][]parsedMessage{}
	for _, m := range pf.messages {
		byType[m.contentType] = append(byType[m.contentType], m)
	}

	// user/assistant text.
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
		t.Errorf("missing user/assistant text messages (user=%v assistant=%v)", sawUser, sawAssistant)
	}

	// tool_use: exec_command with cmd normalised to command, apply_patch text.
	var shell, patch *parsedMessage
	for i := range byType["tool_use"] {
		m := &byType["tool_use"][i]
		switch m.toolName {
		case "exec_command", "shell":
			shell = m
		case "apply_patch":
			patch = m
		}
	}
	if shell == nil || shell.toolUseID != "call_1" || !json.Valid(shell.toolInput) ||
		!strings.Contains(string(shell.toolInput), "command") ||
		!strings.Contains(string(shell.toolInput), "go build") {
		t.Errorf("shell tool_use malformed: %+v input=%s", shell, shellToolInput(shell))
	}
	if patch == nil || patch.toolUseID != "call_2" || patch.toolInput != nil ||
		!strings.Contains(patch.text, "Begin Patch") {
		t.Errorf("apply_patch tool_use malformed: %+v", patch)
	}

	// tool_result paired to the shell call by call_id.
	if rs := byType["tool_result"]; len(rs) != 1 || rs[0].toolUseID != "call_1" || rs[0].text != "build succeeded" {
		t.Errorf("tool_result = %+v, want call_1 / build succeeded", byType["tool_result"])
	}

	// reasoning: encrypted chain → placeholder.
	if th := byType["thinking"]; len(th) != 1 || th[0].text != "[encrypted reasoning]" {
		t.Errorf("thinking = %+v, want [encrypted reasoning]", byType["thinking"])
	}
}

func TestCodexIngestEndToEnd(t *testing.T) {
	codexDir := t.TempDir()
	cwd := "/Users/dev/work/github.com/acme/webapp"
	writeCodexRollout(t, codexDir, cwd)

	s := newTestStore(t, t.TempDir()) // empty Claude project dir
	s.SetCodexRoots([]string{filepath.Join(codexDir, "sessions")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Codex content is searchable.
	results, err := s.Search("authentication", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].SessionID != codexSessUUID {
		t.Fatalf("search did not surface the Codex session: %+v", results)
	}

	// session_meta records the source and repo attribution.
	var source, repo string
	if err := s.readDB.QueryRow(
		`SELECT source, repo FROM session_meta WHERE session_id = ?`, codexSessUUID,
	).Scan(&source, &repo); err != nil {
		t.Fatalf("session_meta query: %v", err)
	}
	if source != "codex" {
		t.Errorf("session_meta.source = %q, want codex", source)
	}
	if repo != "acme/webapp" {
		t.Errorf("session_meta.repo = %q, want acme/webapp", repo)
	}

	entryCount := func() int {
		var n int
		if err := s.readDB.QueryRow(`SELECT count(*) FROM entries WHERE session_id = ?`, codexSessUUID).Scan(&n); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}
	if got := entryCount(); got != 7 {
		t.Fatalf("entries after ingest = %d, want 7 (incl. token_count)", got)
	}

	// Model + usage + parent chain (🎯T112).
	var model string
	if err := s.readDB.QueryRow(
		`SELECT model FROM entries WHERE session_id = ? AND type = 'assistant' AND model IS NOT NULL AND model != '' LIMIT 1`,
		codexSessUUID,
	).Scan(&model); err != nil || model != "gpt-5.5" {
		t.Errorf("model = %q err=%v", model, err)
	}
	var usageN int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages WHERE session_id = ? AND text LIKE '%[codex tokens]%'`,
		codexSessUUID,
	).Scan(&usageN); err != nil || usageN < 1 {
		t.Errorf("usage messages = %d err=%v", usageN, err)
	}
	var pred, mech string
	if err := s.readDB.QueryRow(
		`SELECT predecessor_id, mechanism FROM session_chains WHERE successor_id = ?`,
		codexSessUUID,
	).Scan(&pred, &mech); err != nil || pred != codexParentUUID || mech != "codex_parent" {
		t.Errorf("chain pred=%q mech=%q err=%v", pred, mech, err)
	}
	// cmd → command normalisation surfaces in tool_command.
	var cmd string
	if err := s.readDB.QueryRow(
		`SELECT tool_command FROM messages WHERE session_id = ? AND tool_name = 'exec_command' LIMIT 1`,
		codexSessUUID,
	).Scan(&cmd); err != nil || !strings.Contains(cmd, "go build") {
		t.Errorf("tool_command = %q err=%v", cmd, err)
	}

	// Idempotency: re-ingesting the same rollout from offset 0 must not
	// duplicate rows — the synthetic (session_id, uuid) dedup holds.
	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	path := writeCodexRollout(t, codexDir, cwd)
	if err := s.ingestCodexFile(path); err != nil {
		t.Fatal(err)
	}
	if got := entryCount(); got != 7 {
		t.Errorf("entries after re-ingest = %d, want 7 (dedup)", got)
	}
}
