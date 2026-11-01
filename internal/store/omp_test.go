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

const ompSessID = "01a174ff-8ca8-70bb-bd0f-520fc6d13c43"

func TestIsOmpSession(t *testing.T) {
	ok := "/Users/a/.omp/agent/sessions/-work-proj/2026-10-25T21-37-47-176Z_" + ompSessID + ".jsonl"
	if !isOmpSession(ok) {
		t.Error("expected sessions/<cwd>/<timestamp>_<uuid>.jsonl to match")
	}
	if id := ompSessionID(ok); id != ompSessID {
		t.Errorf("session id = %q", id)
	}
	if isOmpSession("/Users/a/.omp/agent/cache/composer/abc/recent-sessions.jsonl") {
		t.Error("composer recent-sessions index must not match")
	}
	if isOmpSession("/Users/a/.claude/projects/-Users-a-work/" + ompSessID + ".jsonl") {
		t.Error("Claude JSONL must not match")
	}
	if isOmpSession("/Users/a/.grok/sessions/abc/updates.jsonl") {
		t.Error("Grok updates.jsonl must not match")
	}
	if isOmpSession("/tmp/sessions/proj/not-a-uuid.jsonl") {
		t.Error("a sessions file without a trailing uuid must not match")
	}
}

func TestOmpRootsForHonoursEnv(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(t.TempDir(), "agent"))
	t.Setenv("OMP_HOME", filepath.Join(t.TempDir(), "missing-omp"))
	got := OmpRootsFor("/unused")
	want := filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "sessions")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("OmpRootsFor = %v, want [%s]", got, want)
	}
}

func TestOmpRootsForProfiles(t *testing.T) {
	base := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("OMP_HOME", base)
	if err := os.MkdirAll(filepath.Join(base, "profiles", "work", "agent", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := OmpRootsFor("/unused")
	wantDefault := filepath.Join(base, "agent", "sessions")
	wantProfile := filepath.Join(base, "profiles", "work", "agent", "sessions")
	if len(got) != 2 || got[0] != wantDefault || got[1] != wantProfile {
		t.Fatalf("OmpRootsFor = %v, want [%s %s]", got, wantDefault, wantProfile)
	}
}

func ompFixtureLines(t *testing.T) []byte {
	t.Helper()
	toolID := "call-1\nfc_1"
	records := []map[string]any{
		{"type": "session", "version": 3, "id": ompSessID, "timestamp": "2026-10-25T21:37:47.176Z", "cwd": "/Users/dev/work/github.com/acme/webapp"},
		{"type": "model_change", "id": "cdca8338", "timestamp": "2026-10-25T21:37:47.220Z", "model": "cursor/grok-4.7"},
		{"type": "title", "title": "Wrong title"},
		{
			"type": "message", "id": "c5279d6e", "timestamp": "2026-10-25T21:37:53.502Z",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "text", "text": "How do I fix the authentication bug?",
				}},
			},
		},
		{
			"type": "message", "id": "aaaa1111", "timestamp": "2026-10-25T21:38:01.000Z",
			"message": map[string]any{
				"role": "assistant", "model": "grok-4.7-high",
				"provider": "cursor", "api": "cursor-agent",
				"usage": map[string]any{"input": 11, "output": 22, "cacheRead": 33, "cacheWrite": 44},
				"content": []map[string]any{
					{"type": "thinking", "thinking": "Look at the login handler."},
					{"type": "text", "text": "Inspecting the login handler."},
					{"type": "toolCall", "id": toolID, "name": "read", "arguments": map[string]any{"path": "/tmp/auth.go"}},
				},
			},
		},
		{
			"type": "custom", "customType": "tool_execution_start", "id": "start1111",
			"timestamp": "2026-10-25T21:38:01.100Z",
			"data":      map[string]any{"toolName": "read", "args": map[string]any{"path": "/tmp/auth.go"}},
		},
		{
			"type": "message", "id": "bbbb2222", "timestamp": "2026-10-25T21:38:02.000Z",
			"message": map[string]any{
				"role": "toolResult", "toolCallId": toolID, "toolName": "read", "isError": false,
				"content": []map[string]any{{"type": "text", "text": "package auth"}},
			},
		},
		{
			"type": "message", "id": "cccc3333", "timestamp": "2026-10-25T21:38:03.000Z",
			"message": map[string]any{
				"role": "developer", "synthetic": true,
				"content": []map[string]any{{"type": "text", "text": "Resume the user's latest intent."}},
			},
		},
		{
			"type": "compaction", "id": "dddd4444", "parentId": "bbbb2222", "timestamp": "2026-10-25T21:39:00.000Z",
			"summary":          "Fixed the authentication bug in the login handler.",
			"firstKeptEntryId": "bbbb2222",
			"tokensBefore":     100,
			"tokensAfter":      40,
		},
		{"type": "title_change", "id": "eeee5555", "timestamp": "2026-10-25T21:39:01.000Z", "title": "Still not the topic"},
		{"type": "label", "id": "ffff6666", "timestamp": "2026-10-25T21:39:02.000Z", "text": "do-not-project-label"},
	}
	var b strings.Builder
	for _, rec := range records {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	b.WriteString("{not json\n")
	return []byte(b.String())
}

func writeOmpSession(t *testing.T, ompHome, cwd string) string {
	t.Helper()
	_ = cwd
	dir := filepath.Join(ompHome, "agent", "sessions", "-Users-dev-work-github.com-acme-webapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-10-25T21-37-47-176Z_"+ompSessID+".jsonl")
	if err := os.WriteFile(path, ompFixtureLines(t), 0o644); err != nil {
		t.Fatal(err)
	}
	// A cache index that must not become its own session.
	cache := filepath.Join(ompHome, "agent", "cache", "composer", "abc")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "recent-sessions.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseOmpFile(t *testing.T) {
	home := t.TempDir()
	cwd := "/Users/dev/work/github.com/acme/webapp"
	path := writeOmpSession(t, home, cwd)

	pf, err := parseOmpFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pf.sessionID != ompSessID {
		t.Errorf("sessionID = %q", pf.sessionID)
	}
	if pf.source != "omp" {
		t.Errorf("source = %q", pf.source)
	}
	if pf.cwd != cwd {
		t.Errorf("cwd = %q, want %q", pf.cwd, cwd)
	}
	if pf.project != "acme/webapp" {
		t.Errorf("project = %q, want acme/webapp", pf.project)
	}
	if pf.model != "cursor/grok-4.7" {
		t.Errorf("model = %q", pf.model)
	}
	if pf.topic == "" || !strings.Contains(pf.topic, "authentication") {
		t.Errorf("topic = %q, want the user query", pf.topic)
	}
	if strings.Contains(pf.topic, "Wrong title") || strings.Contains(pf.topic, "Still not") {
		t.Errorf("topic followed the auto title: %q", pf.topic)
	}

	var sawUser, sawAssist, sawThink, sawTool, sawResult, sawDev, sawCompact bool
	var toolUses int
	for _, m := range pf.messages {
		switch {
		case m.contentType == "text" && m.role == "user" && strings.Contains(m.text, "authentication bug"):
			sawUser = true
			if m.isNoise != 0 {
				t.Error("user query marked noise")
			}
		case m.contentType == "thinking" && strings.Contains(m.text, "login handler"):
			sawThink = true
			if m.isNoise != 1 {
				t.Error("thinking should be noise")
			}
		case m.contentType == "text" && strings.Contains(m.text, "Inspecting"):
			sawAssist = true
		case m.contentType == "tool_use" && m.toolName == "read":
			sawTool = true
			toolUses++
			if strings.Contains(m.toolUseID, "\n") {
				t.Errorf("tool id still contains a newline: %q", m.toolUseID)
			}
			var in map[string]any
			if json.Unmarshal(m.toolInput, &in) != nil || in["file_path"] != "/tmp/auth.go" {
				t.Errorf("tool input not normalised: %s", m.toolInput)
			}
		case m.contentType == "tool_result" && strings.Contains(m.text, "package auth"):
			sawResult = true
			if m.toolUseID != "call-1|fc_1" {
				t.Errorf("tool result id = %q", m.toolUseID)
			}
		case strings.Contains(m.text, "latest intent"):
			sawDev = true
			if m.isNoise != 1 {
				t.Error("synthetic developer prompt should be noise")
			}
		case strings.Contains(m.text, "[omp compaction]") && strings.Contains(m.text, "authentication bug"):
			sawCompact = true
		}
	}
	if !sawUser || !sawAssist || !sawThink || !sawTool || !sawResult || !sawDev || !sawCompact {
		t.Errorf("missing messages user=%v assist=%v think=%v tool=%v result=%v dev=%v compact=%v",
			sawUser, sawAssist, sawThink, sawTool, sawResult, sawDev, sawCompact)
	}
	if toolUses != 1 {
		t.Errorf("tool_use count = %d, want 1 (tool_execution_start must not duplicate)", toolUses)
	}

	var sawLabel, sawCustom, sawCompaction bool
	for _, e := range pf.entries {
		switch e.entryType {
		case "label":
			sawLabel = true
			if !strings.Contains(string(e.raw), "do-not-project-label") {
				t.Error("unknown entry type was not kept raw")
			}
		case "custom":
			sawCustom = true
		case "compaction":
			sawCompaction = true
			if !strings.Contains(string(e.raw), `"firstKeptEntryId":"bbbb2222"`) {
				t.Error("compaction entry lost firstKeptEntryId")
			}
		}
	}
	if !sawLabel || !sawCustom || !sawCompaction {
		t.Errorf("catch-all entries label=%v custom=%v compaction=%v", sawLabel, sawCustom, sawCompaction)
	}
	for _, m := range pf.messages {
		if strings.Contains(m.text, "do-not-project-label") {
			t.Error("unknown entry type was projected into a message")
		}
	}
	var sawProvider bool
	for _, e := range pf.entries {
		if e.entryType == "assistant" && strings.Contains(string(e.raw), `"provider":"cursor"`) {
			sawProvider = true
		}
	}
	if !sawProvider {
		t.Error("assistant entry dropped message.provider")
	}
}

func TestOmpIngestEndToEnd(t *testing.T) {
	home := t.TempDir()
	cwd := "/Users/dev/work/github.com/acme/webapp"
	path := writeOmpSession(t, home, cwd)

	s := newTestStore(t, t.TempDir())
	s.SetOmpRoots([]string{filepath.Join(home, "agent", "sessions")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search("authentication", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].SessionID != ompSessID {
		t.Fatalf("search did not surface the omp session: %+v", results)
	}

	var source, repo, cwdGot, topic string
	if err := s.readDB.QueryRow(
		`SELECT source, repo, cwd, topic FROM session_meta WHERE session_id = ?`, ompSessID,
	).Scan(&source, &repo, &cwdGot, &topic); err != nil {
		t.Fatalf("session_meta: %v", err)
	}
	if source != "omp" {
		t.Errorf("source = %q, want omp", source)
	}
	if repo != "acme/webapp" {
		t.Errorf("repo = %q, want acme/webapp", repo)
	}
	if cwdGot != cwd {
		t.Errorf("cwd = %q, want %q", cwdGot, cwd)
	}
	if !strings.Contains(topic, "authentication") {
		t.Errorf("topic = %q", topic)
	}

	var nPath int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages_v WHERE session_id = ? AND tool_file_path = '/tmp/auth.go'`,
		ompSessID,
	).Scan(&nPath); err != nil || nPath < 1 {
		t.Errorf("tool_file_path count = %d err=%v", nPath, err)
	}

	var inTok, outTok, cacheRead, cacheWrite int
	var model string
	if err := s.readDB.QueryRow(
		`SELECT input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, model
		 FROM entries_v WHERE session_id = ? AND type = 'assistant' AND input_tokens > 0`,
		ompSessID,
	).Scan(&inTok, &outTok, &cacheRead, &cacheWrite, &model); err != nil {
		t.Fatalf("usage columns: %v", err)
	}
	if inTok != 11 || outTok != 22 || cacheRead != 33 || cacheWrite != 44 {
		t.Errorf("tokens = %d/%d cache %d/%d, want 11/22 cache 33/44", inTok, outTok, cacheRead, cacheWrite)
	}
	if model != "grok-4.7-high" {
		t.Errorf("model = %q, want the assistant message model", model)
	}

	var phantom int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM session_meta WHERE session_id LIKE '%recent-sessions%' OR source = 'claude' AND cwd = ?`,
		cwd,
	).Scan(&phantom); err != nil {
		t.Fatal(err)
	}
	if phantom != 0 {
		t.Errorf("phantom sessions = %d", phantom)
	}

	entryCount := func() int {
		var n int
		_ = s.readDB.QueryRow(`SELECT count(*) FROM entries_v WHERE session_id = ?`, ompSessID).Scan(&n)
		return n
	}
	before := entryCount()
	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	if err := s.ingestOmpFile(path); err != nil {
		t.Fatal(err)
	}
	if got := entryCount(); got != before {
		t.Errorf("entries after re-ingest = %d, want %d", got, before)
	}

	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	var claudeN int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM session_meta WHERE session_id = ? AND source = 'claude'`,
		ompSessID,
	).Scan(&claudeN); err != nil {
		t.Fatal(err)
	}
	if claudeN != 0 {
		t.Errorf("phantom claude rows for omp session: %d", claudeN)
	}
}
