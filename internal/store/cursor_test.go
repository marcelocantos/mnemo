// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const cursorSessUUID = "32cc2bac-0bc4-4f7b-8de7-164d6b9f46c6"

func TestIsCursorTranscript(t *testing.T) {
	ok := "/Users/a/.cursor/projects/Users-a-work/agent-transcripts/" + cursorSessUUID + "/" + cursorSessUUID + ".jsonl"
	if !isCursorTranscript(ok) {
		t.Error("expected agent-transcripts/<id>/<id>.jsonl to match")
	}
	if isCursorTranscript("/Users/a/.cursor/projects/Users-a-work/worker.log") {
		t.Error("worker.log must not match")
	}
	if isCursorTranscript("/Users/a/.claude/projects/-Users-a-work/" + cursorSessUUID + ".jsonl") {
		t.Error("Claude JSONL must not match")
	}
	if isCursorTranscript("/x/agent-transcripts/other/" + cursorSessUUID + ".jsonl") {
		t.Error("mismatched parent dir must not match")
	}
}

func cursorFixtureLines(t *testing.T) []byte {
	t.Helper()
	records := []map[string]any{
		{
			"role": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": "<timestamp>Friday, Jul 10, 2026, 8:25 PM (UTC+10)</timestamp>\n<user_query>\nHow do I fix the authentication bug?\n</user_query>",
				}},
			},
		},
		{
			"role": "user",
			"message": map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": "<environment_context>\nWorkspace: /tmp/ignore\n</environment_context>",
				}},
			},
		},
		{
			"role": "assistant",
			"message": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "Inspecting the login handler."},
					{"type": "tool_use", "name": "Read", "id": "call-1", "input": map[string]any{"path": "/tmp/auth.go"}},
				},
			},
		},
		{
			"role": "assistant",
			"message": map[string]any{
				"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "call-1", "content": "package auth"},
					{"type": "text", "text": "Fixed the authentication bug in the login handler."},
				},
			},
		},
		{
			"type":   "turn_ended",
			"status": "aborted",
			"error":  "User aborted/interrupted manually.",
		},
		{"not": "json-shaped-enough-wait-this-is-json-but-no-role"},
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

func writeCursorSession(t *testing.T, cursorHome, cwd string) string {
	t.Helper()
	slug := "Users-dev-work-github-com-acme-webapp"
	proj := filepath.Join(cursorHome, "projects", slug)
	dir := filepath.Join(proj, "agent-transcripts", cursorSessUUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, cursorSessUUID+".jsonl")
	if err := os.WriteFile(path, cursorFixtureLines(t), 0o644); err != nil {
		t.Fatal(err)
	}
	log := "Starting typescript-language-server\n" +
		"[info] Getting tree structure for workspacePath=" + cwd + "\n"
	if err := os.WriteFile(filepath.Join(proj, "worker.log"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseCursorFile(t *testing.T) {
	home := t.TempDir()
	cwd := "/Users/dev/work/github.com/acme/webapp"
	path := writeCursorSession(t, home, cwd)

	pf, err := parseCursorFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pf.sessionID != cursorSessUUID {
		t.Errorf("sessionID = %q", pf.sessionID)
	}
	if pf.source != "cursor" {
		t.Errorf("source = %q", pf.source)
	}
	if pf.cwd != cwd {
		t.Errorf("cwd = %q, want %q", pf.cwd, cwd)
	}
	if pf.project != "acme/webapp" {
		t.Errorf("project = %q, want acme/webapp", pf.project)
	}

	var sawUser, sawAssist, sawTool, sawResult, sawEnv, sawEnded bool
	for _, m := range pf.messages {
		switch {
		case m.contentType == "text" && strings.Contains(m.text, "authentication bug") && m.role == "user":
			sawUser = true
			if strings.Contains(m.text, "<user_query>") || strings.Contains(m.text, "<timestamp>") {
				t.Errorf("user text still wrapped: %q", m.text)
			}
		case m.contentType == "text" && strings.Contains(m.text, "environment_context"):
			sawEnv = true
		case m.contentType == "text" && strings.Contains(m.text, "Inspecting"):
			sawAssist = true
		case m.contentType == "tool_use" && m.toolName == "Read":
			sawTool = true
			var in map[string]any
			if json.Unmarshal(m.toolInput, &in) != nil || in["file_path"] != "/tmp/auth.go" {
				t.Errorf("tool input not normalised: %s", m.toolInput)
			}
		case m.contentType == "tool_result" && strings.Contains(m.text, "package auth"):
			sawResult = true
		case strings.Contains(m.text, "turn_ended") || strings.Contains(m.text, "aborted"):
			sawEnded = true
		}
	}
	if !sawUser || !sawAssist || !sawTool || !sawResult {
		t.Errorf("missing core messages user=%v assist=%v tool=%v result=%v", sawUser, sawAssist, sawTool, sawResult)
	}
	if sawEnv {
		t.Error("environment_context wrapper was indexed")
	}
	if sawEnded {
		t.Error("turn_ended trailer was indexed")
	}
	if pf.topic == "" || !strings.Contains(pf.topic, "authentication") {
		t.Errorf("topic = %q, want the user query", pf.topic)
	}
}

func TestCursorIngestEndToEnd(t *testing.T) {
	home := t.TempDir()
	cwd := "/Users/dev/work/github.com/acme/webapp"
	path := writeCursorSession(t, home, cwd)

	s := newTestStore(t, t.TempDir())
	s.SetCursorRoots([]string{filepath.Join(home, "projects")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search("authentication", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].SessionID != cursorSessUUID {
		t.Fatalf("search did not surface the Cursor session: %+v", results)
	}

	var source, repo, cwdGot string
	if err := s.readDB.QueryRow(
		`SELECT source, repo, cwd FROM session_meta WHERE session_id = ?`, cursorSessUUID,
	).Scan(&source, &repo, &cwdGot); err != nil {
		t.Fatalf("session_meta: %v", err)
	}
	if source != "cursor" {
		t.Errorf("source = %q, want cursor", source)
	}
	if repo != "acme/webapp" {
		t.Errorf("repo = %q, want acme/webapp", repo)
	}
	if cwdGot != cwd {
		t.Errorf("cwd = %q, want %q", cwdGot, cwd)
	}

	var nPath int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages_v WHERE session_id = ? AND tool_file_path = '/tmp/auth.go'`,
		cursorSessUUID,
	).Scan(&nPath); err != nil || nPath < 1 {
		t.Errorf("tool_file_path count = %d err=%v", nPath, err)
	}

	entryCount := func() int {
		var n int
		_ = s.readDB.QueryRow(`SELECT count(*) FROM entries_v WHERE session_id = ?`, cursorSessUUID).Scan(&n)
		return n
	}
	before := entryCount()
	s.mu.Lock()
	for p := range s.offsets {
		s.offsets[p] = 0
	}
	s.mu.Unlock()
	if err := s.ingestCursorFile(path); err != nil {
		t.Fatal(err)
	}
	if got := entryCount(); got != before {
		t.Errorf("entries after re-ingest = %d, want %d", got, before)
	}

	// ingestFile must route, not mint a phantom claude session.
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	var claudeN int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM session_meta WHERE session_id = ? AND source = 'claude'`,
		cursorSessUUID,
	).Scan(&claudeN); err != nil {
		t.Fatal(err)
	}
	if claudeN != 0 {
		t.Errorf("phantom claude rows for Cursor session: %d", claudeN)
	}
}

func TestCursorCwdFromSlugWhenWorkerLogHasNoWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cursorCwdFromSlug decodes POSIX slugs only; a Windows temp path has no slug form")
	}
	real := filepath.Join(t.TempDir(), "work", "github.com", "acme", "webapp")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(strings.TrimPrefix(real, "/"))
	home := t.TempDir()
	proj := filepath.Join(home, "projects", slug)
	dir := filepath.Join(proj, "agent-transcripts", cursorSessUUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, cursorSessUUID+".jsonl")
	if err := os.WriteFile(path, cursorFixtureLines(t), 0o644); err != nil {
		t.Fatal(err)
	}
	// worker.log present but no workspacePath= — the live majority shape.
	if err := os.WriteFile(filepath.Join(proj, "worker.log"), []byte("Skipping codebase telemetry sync\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pf, err := parseCursorFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pf.cwd != real {
		t.Errorf("cwd = %q, want reconstructed %q", pf.cwd, real)
	}
}

func TestCursorCwdSlugDoesNotInventHyphenatedRepo(t *testing.T) {
	// squz-multimaze2 encodes the same as squz/multimaze2. Stat-if-exists
	// must not return a path that is not a directory.
	got := cursorCwdFromSlug("Users-nobody-work-github-com-squz-multimaze2")
	if got != "" {
		t.Errorf("lossy slug must not invent cwd, got %q", got)
	}
}

// TestCursorLiveCorpus parses every agent-transcript JSONL under a real
// Cursor projects tree. Skipped in CI; the live oracle is
// MNEMO_CURSOR_CORPUS=$HOME/.cursor/projects.
func TestCursorLiveCorpus(t *testing.T) {
	root := os.Getenv("MNEMO_CURSOR_CORPUS")
	if root == "" {
		t.Skip("set MNEMO_CURSOR_CORPUS to a Cursor projects dir (e.g. ~/.cursor/projects)")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("MNEMO_CURSOR_CORPUS=%q is not a directory: %v", root, err)
	}

	s := newTestStore(t, t.TempDir())
	s.SetCursorRoots([]string{root})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	var files []string
	filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && isCursorTranscript(path) {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		t.Fatalf("no agent-transcripts JSONL under %s", root)
	}

	var nSess, nCursor, nCwd, nUser int
	rows, err := s.readDB.Query(`
		SELECT sm.session_id, sm.source, sm.cwd,
		       (SELECT count(*) FROM messages_v m
		         WHERE m.session_id = sm.session_id AND m.role = 'user' AND m.content_type = 'text')
		FROM session_meta sm WHERE sm.source = 'cursor'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, source, cwd string
		var users int
		if err := rows.Scan(&id, &source, &cwd, &users); err != nil {
			t.Fatal(err)
		}
		nSess++
		if source == "cursor" {
			nCursor++
		}
		if cwd != "" {
			nCwd++
		}
		if users > 0 {
			nUser++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus %s: jsonl=%d sessions=%d cursor=%d with_cwd=%d with_user_text=%d",
		root, len(files), nSess, nCursor, nCwd, nUser)
	if nSess != len(files) {
		t.Errorf("sessions = %d, jsonl files = %d", nSess, len(files))
	}
	if nCursor != nSess {
		t.Errorf("source=cursor on %d of %d sessions", nCursor, nSess)
	}
	if nUser == 0 {
		t.Error("no user text indexed; user_query unwrap likely failed on live files")
	}
	if nCwd == 0 {
		t.Error("no session got a cwd; worker.log and slug fallback both empty")
	}

	wantIDs := map[string]bool{}
	for _, p := range files {
		wantIDs[cursorSessionID(p)] = true
	}
	var missing []string
	for id := range wantIDs {
		var n int
		_ = s.readDB.QueryRow(`SELECT count(*) FROM session_meta WHERE session_id = ? AND source = 'cursor'`, id).Scan(&n)
		if n != 1 {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("session ids not indexed 1:1: %v", missing)
	}

	var nTool, nPath, nEnded, nRedacted int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM messages_v WHERE content_type = 'tool_use' AND session_id IN (SELECT session_id FROM session_meta WHERE source = 'cursor')`).Scan(&nTool); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`SELECT count(*) FROM messages_v WHERE tool_file_path != '' AND session_id IN (SELECT session_id FROM session_meta WHERE source = 'cursor')`).Scan(&nPath); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`SELECT count(*) FROM messages_v WHERE text LIKE '%turn_ended%' AND session_id IN (SELECT session_id FROM session_meta WHERE source = 'cursor')`).Scan(&nEnded); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`SELECT count(*) FROM messages_v WHERE text = '[REDACTED]' AND session_id IN (SELECT session_id FROM session_meta WHERE source = 'cursor')`).Scan(&nRedacted); err != nil {
		t.Fatal(err)
	}
	t.Logf("live messages tool_use=%d tool_file_path=%d turn_ended=%d redacted=%d", nTool, nPath, nEnded, nRedacted)
	if nTool == 0 {
		t.Error("live corpus has hundreds of tool_use; none indexed")
	}
	if nPath == 0 {
		t.Error("Read/Grep/Write path aliases did not populate tool_file_path")
	}
	if nEnded != 0 {
		t.Errorf("turn_ended trailers indexed as messages: %d", nEnded)
	}
	if nRedacted != 0 {
		t.Errorf("[REDACTED] placeholders indexed: %d", nRedacted)
	}

	var sample string
	if err := s.readDB.QueryRow(`
		SELECT text FROM messages_v
		WHERE role = 'user' AND content_type = 'text' AND length(text) >= 24
		  AND session_id IN (SELECT session_id FROM session_meta WHERE source = 'cursor')
		ORDER BY length(text) DESC
		LIMIT 1`).Scan(&sample); err != nil {
		t.Fatalf("no user text to search: %v", err)
	}
	q := ""
	for _, w := range strings.Fields(sample) {
		w = strings.Trim(w, ".,;:!?\"'")
		if len(w) >= 6 && !strings.ContainsAny(w, "<>/\\") {
			q = w
			break
		}
	}
	if q == "" {
		t.Logf("no FTS-safe token in %q", sample)
	} else {
		hits, err := s.Search(q, 5, "all", "", 0, 0, false)
		if err != nil || len(hits) == 0 {
			t.Errorf("search %q from live user text: %v hits=%d", q, err, len(hits))
		}
	}

	var claudeN int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM session_meta WHERE source = 'claude'`).Scan(&claudeN); err != nil {
		t.Fatal(err)
	}
	if claudeN != 0 {
		t.Errorf("phantom claude rows from Cursor corpus: %d", claudeN)
	}
}
