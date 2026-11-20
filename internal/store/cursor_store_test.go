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

	"github.com/marcelocantos/mnemo/internal/store/fswatch"
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
		`SELECT count(*) FROM messages_v WHERE text LIKE '%secret-acp-only-token%'`,
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
		`SELECT count(*) FROM messages_v WHERE session_id = ? AND content_type = 'tool_result'`, sess,
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
		`SELECT model FROM entries_v WHERE session_id = ? AND model != '' LIMIT 1`, sess,
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
		`SELECT count(*) FROM messages_v WHERE session_id = ? AND content_type = 'tool_result'`, sess,
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
		`SELECT count(*) FROM messages_v WHERE content_type = 'tool_result'`,
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

func TestDefectCursorStoreFailedReadStopsTheIndex(t *testing.T) {
	home := t.TempDir()
	side := filepath.Join(home, "side", "store.db")
	writeCursorStoreFixture(t, side, "beta-tool-result-marker", "/Users/dev/work/github.com/acme/webapp")
	valid, err := os.ReadFile(side)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "chats", "abc", cursorSessUUID, "store.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, len(valid)+8192), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	err = s.ingestCursorStoreFile(path)
	var off int64
	cursorErr := s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&off)
	if err := os.WriteFile(path, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	err2 := s.ingestCursorStoreFile(path)
	if n := countText(t, s, "beta-tool-result-marker"); n != 1 {
		t.Fatalf("valid store.db (%d bytes) was not indexed after a failed read of a larger file recorded offset %d (second err=%v hits=%d)", len(valid), off, err2, n)
	}
	if err == nil || cursorErr != sql.ErrNoRows {
		t.Fatalf("failed read err=%v; cursor offset=%d err=%v, want no cursor", err, off, cursorErr)
	}
}

func TestDefectCursorStoreSameSizeRewriteIsInvisible(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "chats", "abc", "11111111-2222-3333-4444-555555555555", "store.db")
	writeCursorStoreFixture(t, path, "original-tool-marker-xxxxxx", "/tmp/proj")
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`
		INSERT INTO messages (session_id, project, role, text, timestamp, content_type)
		VALUES (?, '', 'user', 'jsonl-sibling-must-stay', '2026-11-20T00:00:00Z', 'text')`,
		"11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`UPDATE blobs SET data = CAST(REPLACE(CAST(data AS TEXT), 'original-tool-marker-xxxxxx', 'original-tool-marker-YYYYYY') AS BLOB) WHERE CAST(data AS TEXT) LIKE '%original-tool-marker-xxxxxx%'`)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := res.RowsAffected()
	if err != nil || updated != 1 {
		t.Fatalf("unexpected blob UPDATE rows=%d err=%v", updated, err)
	}
	db.Close()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	n := countText(t, s, "original-tool-marker-YYYYYY")
	if after.Size() != before.Size() {
		t.Fatalf("fixture changed main database size from %d to %d", before.Size(), after.Size())
	}
	if n != 1 {
		t.Fatalf("same-size store.db (%d bytes) was not re-read after its blob changed (rows=%d)", after.Size(), updated)
	}
	if old := countText(t, s, "original-tool-marker-xxxxxx"); old != 0 {
		t.Fatalf("old blob text still indexed (%d)", old)
	}
	if sib := countText(t, s, "jsonl-sibling-must-stay"); sib != 1 {
		t.Fatalf("session rows outside the rewritten blob were removed (%d)", sib)
	}
}

func TestCursorStoreFingerprintSeesWAL(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "chats", "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, path, "before-wal-tool-result", "/tmp/proj")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA wal_autocheckpoint = 0;`); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, "before-wal-tool-result")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE blobs SET data = CAST(REPLACE(CAST(data AS TEXT), ?, ?) AS BLOB)
		WHERE INSTR(CAST(data AS TEXT), ?) > 0`, "before-wal-tool-result", "after-wal-tool-result", "before-wal-tool-result"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("main database changed during WAL-only update, err=%v", err)
	}
	if !fswatch.Interest(path+"-wal", fswatch.ModeTranscript) {
		t.Fatal("transcript watcher drops Cursor WAL events")
	}
	s.ingestJSONLPath(path + "-wal")
	expectOnce(t, s, "after-wal-tool-result")
	if n := countText(t, s, "before-wal-tool-result"); n != 0 {
		t.Fatalf("old WAL-backed result is still indexed %d times", n)
	}
}

func TestCursorStoreSnapshotDoesNotClaimNewerPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, path, "before-db-rewrite-marker", "/tmp/proj")
	s := newTestStore(t, t.TempDir())
	pf, err := parseCursorStoreFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE blobs SET data = CAST(REPLACE(CAST(data AS TEXT), ?, ?) AS BLOB)
		WHERE INSTR(CAST(data AS TEXT), ?) > 0`, "before-db-rewrite-marker", "after-db-rewrite-marker", "before-db-rewrite-marker"); err != nil {
		t.Fatal(err)
	}
	ws, err := s.newWriterState()
	if err != nil {
		t.Fatal(err)
	}
	defer ws.tx.Rollback()
	defer ws.Close()
	if err := s.writeParsedFile(ws, pf); err != nil {
		t.Fatal(err)
	}
	if err := s.commitIngest(ws); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, "after-db-rewrite-marker")
}

func TestCursorStoreReadOnlyBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, path, "busy-timeout-marker", "/tmp/proj")
	db, err := openCursorStoreRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var timeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != cursorStoreBusyTimeout {
		t.Fatalf("read-only busy_timeout = %d, err = %v", timeout, err)
	}
}
