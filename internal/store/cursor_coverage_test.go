// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live Cursor JSONL has no tool ids and no tool_result — just role/message
// content arrays, tool_use{name,input}, and a turn_ended trailer.
func cursorLiveShapeLines(t *testing.T) []byte {
	t.Helper()
	records := []map[string]any{
		{"role": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "<user_query>\nExplain the maze physics uniqueCursorLiveShape\n</user_query>"},
		}}},
		{"role": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "[REDACTED]"},
			{"type": "tool_use", "name": "Read", "input": map[string]any{"path": "/tmp/Physics.h", "limit": 40}},
			{"type": "tool_use", "name": "Grep", "input": map[string]any{"pattern": "Box2D", "glob": "src/*", "head_limit": 15}},
			{"type": "tool_use", "name": "Shell", "input": map[string]any{"command": "make unit-test", "working_directory": "/tmp"}},
		}}},
		{"role": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "Physics uses Box2D v3 uniqueCursorLiveShape."},
		}}},
		{"type": "turn_ended", "status": "completed"},
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
	return []byte(b.String())
}

func writeCursorTree(t *testing.T, cursorHome, slug, sessionID string, body []byte) string {
	t.Helper()
	dir := filepath.Join(cursorHome, "projects", slug, "agent-transcripts", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseCursorLiveShape(t *testing.T) {
	home := t.TempDir()
	id := "212eba6b-9752-4710-bfa5-b19e72b947aa"
	path := writeCursorTree(t, home, "Users-dev-work-github-com-acme-webapp", id, cursorLiveShapeLines(t))

	pf, err := parseCursorFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pf.source != "cursor" || pf.sessionID != id {
		t.Fatalf("source/id = %s %s", pf.source, pf.sessionID)
	}

	var nUser, nAssist, nRead, nGrep, nShell, nResult, nRedacted int
	for _, m := range pf.messages {
		switch m.contentType {
		case "text":
			if m.role == "user" && strings.Contains(m.text, "uniqueCursorLiveShape") {
				nUser++
				if strings.Contains(m.text, "<user_query>") {
					t.Errorf("wrapped: %q", m.text)
				}
			}
			if m.role == "assistant" && strings.Contains(m.text, "Box2D") {
				nAssist++
			}
			if strings.Contains(m.text, "[REDACTED]") {
				nRedacted++
			}
		case "tool_use":
			switch m.toolName {
			case "Read":
				nRead++
				var in map[string]any
				if json.Unmarshal(m.toolInput, &in) != nil || in["file_path"] != "/tmp/Physics.h" {
					t.Errorf("Read input = %s", m.toolInput)
				}
				if m.toolUseID != "" {
					t.Errorf("live tool_use has no id, got %q", m.toolUseID)
				}
			case "Grep":
				nGrep++
			case "Shell":
				nShell++
			}
		case "tool_result":
			nResult++
		}
	}
	if nUser != 1 || nAssist != 1 || nRead != 1 || nGrep != 1 || nShell != 1 {
		t.Errorf("counts user=%d assist=%d read=%d grep=%d shell=%d", nUser, nAssist, nRead, nGrep, nShell)
	}
	if nResult != 0 {
		t.Error("live shape has no tool_result")
	}
	if nRedacted != 0 {
		t.Error("[REDACTED] placeholder was indexed")
	}
}

func TestCursorIncrementalIngest(t *testing.T) {
	home := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	slug := "Users-dev-work-github-com-acme-webapp"
	first, _ := json.Marshal(map[string]any{
		"role": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "<user_query>\ncursorIncFirst uniqueCursorInc\n</user_query>"},
		}},
	})
	path := writeCursorTree(t, home, slug, id, append(first, '\n'))

	s := newTestStore(t, t.TempDir())
	s.SetCursorRoots([]string{filepath.Join(home, "projects")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		_ = s.readDB.QueryRow(`SELECT count(*) FROM entries WHERE session_id = ?`, id).Scan(&n)
		return n
	}
	before := count()
	if before < 1 {
		t.Fatal("expected first line ingested")
	}

	second, _ := json.Marshal(map[string]any{
		"role": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "cursorIncSecond uniqueCursorInc"},
		}},
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(second, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := s.ingestCursorFile(path); err != nil {
		t.Fatal(err)
	}
	after := count()
	if after <= before {
		t.Errorf("entries did not grow after append: %d → %d", before, after)
	}
	if err := s.ingestCursorFile(path); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != after {
		t.Errorf("third ingest not idempotent: %d → %d", after, got)
	}
}

func TestWatchIngestsCursorAppend(t *testing.T) {
	home := t.TempDir()
	id := "11111111-2222-3333-4444-555555555555"
	slug := "Users-dev-work-github-com-acme-webapp"
	seed, _ := json.Marshal(map[string]any{
		"role": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "<user_query>\ncursor watch seed enough text\n</user_query>"},
		}},
	})
	path := writeCursorTree(t, home, slug, id, append(seed, '\n'))

	s := newTestStore(t, t.TempDir())
	s.SetCursorRoots([]string{filepath.Join(home, "projects")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Watch(ctx) }()
	time.Sleep(400 * time.Millisecond)

	extra, _ := json.Marshal(map[string]any{
		"role": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "<user_query>\nuniquecursort149watch enough text\n</user_query>"},
		}},
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(extra, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	deadline := time.Now().Add(8 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		hits, err := s.Search("uniquecursort149watch", 5, "all", "", 0, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) > 0 {
			found = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Watch returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return after cancel")
	}
	if !found {
		t.Fatal("appended Cursor transcript line not ingested via Store.Watch")
	}
}

func TestCursorIgnoresSiblingJSONL(t *testing.T) {
	home := t.TempDir()
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	slug := "Users-dev-work-github-com-acme-webapp"
	writeCursorTree(t, home, slug, id, cursorLiveShapeLines(t))
	decoyDir := filepath.Join(home, "projects", slug)
	if err := os.WriteFile(filepath.Join(decoyDir, "noise.jsonl"),
		[]byte(`{"role":"user","message":{"content":[{"type":"text","text":"decoyCursorSiblingMarker"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(decoyDir, "agent-tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyDir, "agent-tools", id+".txt"), []byte("not a transcript"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t, t.TempDir())
	s.SetCursorRoots([]string{filepath.Join(home, "projects")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search("decoyCursorSiblingMarker", 5, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("sibling jsonl was ingested: %+v", hits)
	}
	hits, err = s.Search("uniqueCursorLiveShape", 5, "all", "", 0, 0, false)
	if err != nil || len(hits) == 0 {
		t.Fatalf("real transcript missed: %v %+v", err, hits)
	}
}

func TestCursorRootsForHonoursCURSOR_HOME(t *testing.T) {
	t.Setenv("CURSOR_HOME", "/tmp/cursor-home-t149")
	got := CursorRootsFor("/unused")
	if len(got) != 1 || got[0] != "/tmp/cursor-home-t149/projects" {
		t.Errorf("CursorRootsFor = %v", got)
	}
}

func TestCursorEmptyFile(t *testing.T) {
	home := t.TempDir()
	id := "00000000-0000-0000-0000-000000000000"
	path := writeCursorTree(t, home, "empty-window", id, nil)
	pf, err := parseCursorFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.messages) != 0 || pf.newOffset != 0 {
		t.Errorf("empty file: messages=%d offset=%d", len(pf.messages), pf.newOffset)
	}
}

func TestCursorMCPMetaNotTopic(t *testing.T) {
	home := t.TempDir()
	id := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	body, _ := json.Marshal(map[string]any{
		"role": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "<mcp_meta_tools>\nYou have access to MCP tools through GetMcpTools.\n</mcp_meta_tools>"},
		}},
	})
	path := writeCursorTree(t, home, "Users-dev-work-github-com-acme-webapp", id, append(body, '\n'))
	pf, err := parseCursorFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pf.topic != "" {
		t.Errorf("mcp_meta_tools became topic: %q", pf.topic)
	}
	for _, m := range pf.messages {
		if strings.Contains(m.text, "GetMcpTools") {
			t.Errorf("injected MCP preamble indexed: %q", m.text)
		}
	}
}

func TestCursorResumeMatchesAgentHelp(t *testing.T) {
	if _, err := exec.LookPath("agent"); err != nil {
		t.Skip("agent CLI not on PATH")
	}
	out, err := exec.Command("agent", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("agent --help: %v\n%s", err, out)
	}
	help := string(out)
	if !strings.Contains(help, "--resume") {
		t.Fatalf("agent --help no longer documents --resume:\n%s", help)
	}
	cmd, err := (SessionRef{SessionID: "deadbeef-0000-0000-0000-000000000000", Source: "cursor"}).ResumeCommand()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cmd, "agent --resume ") {
		t.Errorf("ResumeCommand = %q", cmd)
	}
}
