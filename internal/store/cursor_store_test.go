// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsCursorStore(t *testing.T) {
	ok := "/Users/a/.cursor/chats/715a64dc9166ff303f3c91472e757187/" + cursorSessUUID + "/store.db"
	if !isCursorStore(ok) {
		t.Error("expected chats/<hash>/<id>/store.db to match")
	}
	if isCursorStore("/Users/a/.cursor/acp-sessions/" + cursorSessUUID + "/store.db") {
		t.Error("acp-sessions store.db must not match")
	}
	if isCursorStore(ok + "-wal") {
		t.Error("WAL sidecar must not match")
	}
	if isCursorStore("/Users/a/.cursor/projects/x/agent-transcripts/" + cursorSessUUID + "/" + cursorSessUUID + ".jsonl") {
		t.Error("JSONL must not match isCursorStore")
	}
}

func TestCursorACPSessionsExcluded(t *testing.T) {
	// Decision lock (🎯T149.1): acp-sessions use the same blobs/meta schema
	// as chats but have 0 id overlap with Agent CLI chats and are not on
	// the agent --resume path for JSONL sessions. CursorChatRootsFor must
	// not discover them; isCursorStore must reject their paths.
	home := t.TempDir()
	t.Setenv("CURSOR_HOME", home)
	acp := filepath.Join(home, "acp-sessions", cursorSessUUID)
	if err := os.MkdirAll(acp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCursorStoreFixture(t, filepath.Join(acp, "store.db"), "secret-acp-only-token", "acme/webapp")
	if isCursorStore(filepath.Join(acp, "store.db")) {
		t.Fatal("acp store must not be classified as chats store")
	}
	roots := CursorChatRootsFor(home)
	if len(roots) != 1 || roots[0] != filepath.Join(home, "chats") {
		t.Fatalf("CursorChatRootsFor = %v, want only chats/", roots)
	}
	s := newTestStore(t, t.TempDir())
	s.SetCursorChatRoots(roots)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages WHERE text LIKE '%secret-acp-only-token%'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("ACP store was ingested (%d hits)", n)
	}
}

func TestParseCursorStoreToolResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chats", "abc", cursorSessUUID, "store.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "unique-tool-result-only-in-store-db-xyzzy"
	writeCursorStoreFixture(t, path, marker, "/Users/dev/work/github.com/acme/webapp")
	writeCursorMetaJSON(t, filepath.Dir(path), "/Users/dev/work/github.com/acme/webapp", "Auth Fix Session")

	pf, err := parseCursorStoreFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pf.source != "cursor" || pf.sessionID != cursorSessUUID {
		t.Fatalf("session=%s source=%s", pf.sessionID, pf.source)
	}
	if pf.cwd != "/Users/dev/work/github.com/acme/webapp" {
		t.Errorf("cwd = %q", pf.cwd)
	}
	if pf.topic != "Auth Fix Session" {
		t.Errorf("topic = %q", pf.topic)
	}
	if pf.model != "default" {
		t.Errorf("model = %q, want default", pf.model)
	}
	var sawResult, sawSystem bool
	for _, m := range pf.messages {
		if m.contentType == "tool_result" && strings.Contains(m.text, marker) {
			sawResult = true
			if m.toolName != "Read" {
				t.Errorf("toolName = %q", m.toolName)
			}
			if m.toolUseID == "" {
				t.Error("expected toolUseID from toolCallId")
			}
		}
		if strings.Contains(m.text, "blobEncryptionKey") || strings.Contains(m.text, "you are a system") {
			sawSystem = true
		}
	}
	if !sawResult {
		t.Fatal("tool_result not parsed")
	}
	if sawSystem {
		t.Error("system / encryption noise was indexed")
	}
	for _, e := range pf.entries {
		var raw map[string]any
		if json.Unmarshal(e.raw, &raw) != nil {
			t.Fatalf("raw not json: %s", e.raw)
		}
		uuid, _ := raw["uuid"].(string)
		if !strings.HasPrefix(uuid, "cursor-"+cursorSessUUID+"-") {
			t.Errorf("uuid = %q", uuid)
		}
		msg, _ := raw["message"].(map[string]any)
		if msg["model"] != "default" {
			t.Errorf("entry model = %v", msg["model"])
		}
	}
}

func TestCursorStoreIngestEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CURSOR_HOME", home)
	cwd := "/Users/dev/work/github.com/acme/squz-multimaze2"
	hash := fmt.Sprintf("%x", md5.Sum([]byte(cwd)))
	sess := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	chatDir := filepath.Join(home, "chats", hash, sess)
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "storedbonlyoraclestringt1491"
	writeCursorStoreFixture(t, filepath.Join(chatDir, "store.db"), marker, cwd)
	writeCursorMetaJSON(t, chatDir, cwd, "Hyphenated Repo Session")

	// Minimal JSONL with tool_use but no tool_result (live shape).
	projSlug := "Users-dev-work-github-com-acme-squz-multimaze2"
	jsonlBody, _ := json.Marshal(map[string]any{
		"role": "assistant",
		"message": map[string]any{"content": []map[string]any{
			{"type": "text", "text": "Reading the skill."},
			{"type": "tool_use", "name": "Read", "input": map[string]any{"path": "/tmp/x"}},
		}},
	})
	writeCursorTree(t, home, projSlug, sess, append(jsonlBody, '\n'))

	s := newTestStore(t, t.TempDir())
	s.SetCursorRoots([]string{filepath.Join(home, "projects")})
	s.SetCursorChatRoots([]string{filepath.Join(home, "chats")})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	var source, repo, cwdGot, topic string
	if err := s.readDB.QueryRow(
		`SELECT source, repo, cwd, topic FROM session_meta WHERE session_id = ?`, sess,
	).Scan(&source, &repo, &cwdGot, &topic); err != nil {
		t.Fatalf("session_meta: %v", err)
	}
	if source != "cursor" {
		t.Errorf("source = %q", source)
	}
	if cwdGot != cwd {
		t.Errorf("cwd = %q, want meta.json cwd (hyphenated repo)", cwdGot)
	}
	if repo != "acme/squz-multimaze2" {
		t.Errorf("repo = %q", repo)
	}
	if !strings.Contains(topic, "Hyphenated") && !strings.Contains(topic, "Reading") {
		t.Errorf("topic = %q", topic)
	}

	var nResult int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages WHERE session_id = ? AND content_type = 'tool_result'`, sess,
	).Scan(&nResult); err != nil || nResult < 1 {
		t.Fatalf("tool_result count = %d err=%v", nResult, err)
	}

	hits, err := s.Search(marker, 10, "all", "", 0, 0, false)
	if err != nil || len(hits) == 0 {
		t.Fatalf("Search for store-only marker failed: %v %+v", err, hits)
	}
	if hits[0].SessionID != sess {
		t.Errorf("hit session = %s", hits[0].SessionID)
	}

	var model string
	_ = s.readDB.QueryRow(
		`SELECT model FROM entries WHERE session_id = ? AND model != '' LIMIT 1`, sess,
	).Scan(&model)
	if model != "default" {
		t.Errorf("entries.model = %q, want default from lastUsedModel", model)
	}

	// Idempotent re-ingest.
	before := nResult
	s.mu.Lock()
	for p := range s.offsets {
		if strings.HasSuffix(p, "store.db") {
			s.offsets[p] = 0
		}
	}
	s.mu.Unlock()
	if err := s.ingestCursorStoreFile(filepath.Join(chatDir, "store.db")); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages WHERE session_id = ? AND content_type = 'tool_result'`, sess,
	).Scan(&nResult); err != nil {
		t.Fatal(err)
	}
	if nResult != before {
		t.Errorf("tool_result after re-ingest = %d, want %d", nResult, before)
	}

	var claudeN int
	_ = s.readDB.QueryRow(
		`SELECT count(*) FROM session_meta WHERE session_id = ? AND source = 'claude'`, sess,
	).Scan(&claudeN)
	if claudeN != 0 {
		t.Errorf("phantom claude rows: %d", claudeN)
	}
}

func TestCursorChatRootsForHonoursCURSOR_HOME(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cursor-home-t149-1")
	t.Setenv("CURSOR_HOME", base)
	got := CursorChatRootsFor("/unused")
	if len(got) != 1 || got[0] != filepath.Join(base, "chats") {
		t.Errorf("CursorChatRootsFor = %v", got)
	}
}

func TestCursorLiveChatsCorpus(t *testing.T) {
	root := os.Getenv("MNEMO_CURSOR_CHATS")
	if root == "" {
		t.Skip("set MNEMO_CURSOR_CHATS to ~/.cursor/chats to run live oracle")
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		t.Fatalf("MNEMO_CURSOR_CHATS=%q: %v", root, err)
	}
	s := newTestStore(t, t.TempDir())
	s.SetCursorChatRoots([]string{root})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var nResult int
	if err := s.readDB.QueryRow(
		`SELECT count(*) FROM messages WHERE content_type = 'tool_result'`,
	).Scan(&nResult); err != nil {
		t.Fatal(err)
	}
	if nResult == 0 {
		t.Fatal("live chats corpus produced 0 tool_result rows")
	}
	t.Logf("live chats: tool_result=%d", nResult)
}

func writeCursorMetaJSON(t *testing.T, sessionDir, cwd, title string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"schemaVersion":   1,
		"cwd":             cwd,
		"title":           title,
		"hasConversation": true,
	})
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCursorStoreFixture(t *testing.T, path, toolMarker, cwd string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB);
		CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
	`); err != nil {
		t.Fatal(err)
	}
	metaObj := map[string]any{
		"agentId":           cursorSessUUID,
		"name":              "fixture",
		"lastUsedModel":     "default",
		"blobEncryptionKey": "should-not-be-indexed",
		"latestRootBlobId":  "deadbeef",
	}
	metaJSON, _ := json.Marshal(metaObj)
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES ('0', ?)`, hex.EncodeToString(metaJSON)); err != nil {
		t.Fatal(err)
	}
	if cwd != "" {
		cwdHex := hex.EncodeToString([]byte(`"` + cwd + `"`))
		_, _ = db.Exec(`INSERT INTO meta(key, value) VALUES ('cwd', ?)`, cwdHex)
	}

	toolBlob, _ := json.Marshal(map[string]any{
		"role": "tool",
		"id":   "call-fixture-1",
		"content": []map[string]any{{
			"type":       "tool-result",
			"toolCallId": "call-fixture-1",
			"toolName":   "Read",
			"result":     toolMarker + "\nline two of tool output",
		}},
	})
	sysBlob, _ := json.Marshal(map[string]any{
		"role": "system",
		"content": []map[string]any{{
			"type": "text", "text": "you are a system prompt with blobEncryptionKey",
		}},
	})
	userBlob, _ := json.Marshal(map[string]any{
		"role": "user",
		"content": []map[string]any{{
			"type": "text", "text": "<user_info>should not index from store</user_info>",
		}},
	})
	for i, blob := range [][]byte{toolBlob, sysBlob, userBlob, {0x00, 0x01, 0x02, 0xff}} {
		id := fmt.Sprintf("blob%d", i)
		if _, err := db.Exec(`INSERT INTO blobs(id, data) VALUES (?, ?)`, id, blob); err != nil {
			t.Fatal(err)
		}
	}
}
