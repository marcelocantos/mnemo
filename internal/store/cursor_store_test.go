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
	"time"

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
	stamp := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	checkImage := checkReferencedMessage(t, s, "original-tool-marker-xxxxxx")
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
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Keep the main-file stat unchanged. The SQLite header change
	// counter must be enough to notice this page update.
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		t.Fatal("fixture changed the main database's size, mtime, or identity")
	}
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	n := countText(t, s, "original-tool-marker-YYYYYY")
	if n != 1 {
		t.Fatalf("same-size store.db (%d bytes) was not re-read after its blob changed (rows=%d)", after.Size(), updated)
	}
	if old := countText(t, s, "original-tool-marker-xxxxxx"); old != 0 {
		t.Fatalf("old blob text still indexed (%d)", old)
	}
	checkImage("original-tool-marker-YYYYYY")
	for _, fts := range []string{
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`,
		`SELECT COUNT(*) FROM messages_fts_stem WHERE messages_fts_stem MATCH ?`,
	} {
		for term, want := range map[string]int{`"original-tool-marker-xxxxxx"`: 0, `"original-tool-marker-YYYYYY"`: 1} {
			var hits int
			if err := s.readDB.QueryRow(fts, term).Scan(&hits); err != nil || hits != want {
				t.Fatalf("search %q for %q returned %d, want %d, err %v", fts, term, hits, want, err)
			}
		}
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

func TestCursorWALFingerprintTracksSizeAndMtimeIndependently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, path, "store-fingerprint-fixture", "")
	wal := path + "-wal"
	stamp := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	writeWAL := func(content string, mtime time.Time) string {
		t.Helper()
		// The fingerprint uses WAL file metadata, so no database opens
		// while the synthetic sidecar is in place.
		writeLines(t, wal, content)
		if err := os.Chtimes(wal, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return cursorStoreFingerprint(path)
	}
	first := writeWAL("initial", stamp)
	if got := writeWAL("changed", stamp.Add(time.Second)); got == first {
		t.Error("same-size WAL rewrite did not change the fingerprint")
	}
	if got := writeWAL("grew in place", stamp); got == first {
		t.Error("WAL growth at the same mtime did not change the fingerprint")
	}
}

func TestCursorBlobIterationFailureLeavesNoPartialIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	const marker = "cursorresultbeforeiterationfailure"
	writeCursorStoreFixture(t, path, marker, "")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Deliver one valid blob before SQLite fails on the next step.
	if _, err := db.Exec(`ALTER TABLE blobs RENAME TO fixture_blobs;
		CREATE VIEW blobs AS
		SELECT id, data FROM fixture_blobs WHERE id = 'blob0'
		UNION ALL SELECT 'broken', json('invalid fixture');`); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("ingest returned %v, want the SQLite iteration error", err)
	}
	var offset int64
	if err := s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&offset); err != sql.ErrNoRows {
		t.Fatalf("failed read recorded cursor %d, query error %v", offset, err)
	}
	if n := countText(t, s, marker); n != 0 {
		t.Fatalf("failed read committed %d tool results", n)
	}
}

func TestCursorBlobKeepsEveryToolResultPart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, path, "replaced fixture", "")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const resultA = "cursorfirstmultipartresult"
	const resultB = "cursorsecondmultipartresult"
	if _, err := db.Exec(`UPDATE blobs SET data = json_object('role', 'tool', 'content', json_array(
		json_object('type', 'tool-result', 'toolCallId', 'call-a', 'result', ?),
		json_object('type', 'tool-result', 'toolCallId', 'call-b', 'result', ?)
	)) WHERE id = 'blob0'`, resultA, resultB); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, resultA)
	expectOnce(t, s, resultB)
}

func TestCursorSQLiteHeaderChangeCounterIsBigEndianAtByte24(t *testing.T) {
	path := filepath.Join(t.TempDir(), "header.db")
	var header [100]byte
	copy(header[:], "SQLite format 3\x00")
	copy(header[24:], []byte{0x12, 0x34, 0x56, 0x78, 0x9a})
	writeLines(t, path, string(header[:]))
	if got := sqliteChangeCounter(path); got != 0x12345678 {
		t.Fatalf("change counter = %#x, want 0x12345678", got)
	}
}

func TestCursorProjectionRefreshesToolLinkWhenTextIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, path, "projection fixture", "")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const result = "same tool result after moving the link"
	const fallback = "blob-fallback-tool-call"
	writeResult := func(partID, name string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE blobs SET data = json_object('role', 'tool', 'id', ?, 'content', json_array(
			json_object('type', 'tool-result', 'toolCallId', ?, 'toolName', ?, 'result', ?)))
			WHERE id = 'blob0'`, fallback, partID, name, result); err != nil {
			t.Fatal(err)
		}
	}
	s := newTestStore(t, t.TempDir())
	check := func(wantID, wantName string) {
		t.Helper()
		if err := s.ingestCursorStoreFile(path); err != nil {
			t.Fatal(err)
		}
		expectOnce(t, s, result)
		var toolID, name, entryToolID string
		if err := s.readDB.QueryRow(`SELECT m.tool_use_id, m.tool_name, e.spawn_tool_use_id
			FROM messages_v m JOIN entries_v e ON e.id = m.entry_id WHERE m.text = ?`, result).
			Scan(&toolID, &name, &entryToolID); err != nil {
			t.Fatal(err)
		}
		if toolID != wantID || name != wantName || entryToolID != wantID {
			t.Fatalf("tool link = %q/%q, entry link = %q, want %q/%q and %q", toolID, name, entryToolID, wantID, wantName, wantID)
		}
	}
	const explicit = "explicit-part-tool-call"
	writeResult(explicit, "Read")
	check(explicit, "Read")
	writeResult("", "Write")
	check(fallback, "Write")
}

func TestUnopenedCursorStoreIsRetriedAfterBatchParseFailure(t *testing.T) {
	chats := filepath.Join(t.TempDir(), "chats")
	path := filepath.Join(chats, "abc", cursorSessUUID, "store.db")
	const marker = "cursor tool result recovered by a retry"
	fixture := filepath.Join(filepath.Dir(path), "fixture.db")
	writeCursorStoreFixture(t, fixture, marker, "")
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, len(body)), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	s.SetCursorChatRoots([]string{chats})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var offset int64
	if err := s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&offset); err != sql.ErrNoRows {
		t.Fatalf("failed startup parse recorded cursor %d, err %v", offset, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	s.safetyPoll(fswatch.NewPollTracker(time.Minute, 2), s.ingestJSONLPath)
	expectOnce(t, s, marker)
}

func TestCursorDatabaseShrinkReplacesTheStoredCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	const marker = "cursor result survives a database vacuum"
	writeCursorStoreFixture(t, path, marker, "")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE fixture_padding (data BLOB);
		INSERT INTO fixture_padding VALUES (zeroblob(65536));`); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE fixture_padding; VACUUM`); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("fixture vacuum did not shrink the database: %d to %d", before.Size(), after.Size())
	}
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	assertJSONLOffset(t, s, path, int(after.Size()))
	expectOnce(t, s, marker)
}

func TestPollRecoversSilentCursorWALUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chats", "abc", cursorSessUUID, "store.db")
	const old = "before-silent-wal-update"
	const updated = "after-silent-wal-update"
	writeCursorStoreFixture(t, path, old, "")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA wal_autocheckpoint = 0;`); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, old)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE blobs SET data = CAST(REPLACE(CAST(data AS TEXT), ?, ?) AS BLOB)
		WHERE id = 'blob0'`, old, updated); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("main database changed during WAL-only update, err %v", err)
	}
	// Drive the recovery path without starting a watcher or calling NoteEvent.
	poller := fswatch.NewPollTracker(time.Minute, 2)
	s.safetyPoll(poller, s.ingestJSONLPath)
	expectOnce(t, s, updated)
	if n := countText(t, s, old); n != 0 {
		t.Fatalf("poll left %d stale tool results", n)
	}
	s.safetyPoll(poller, func(path string) { t.Errorf("unchanged store was queued again: %s", path) })
}

// TestCursorStoreUpgradeKeepsTheOldCursor is the first start after an
// install of this code over a database the previous binary wrote. That
// binary stored "caught up" as offset >= file size and had no
// prefix_sha256. The new fingerprint must not make every store.db look
// unread.
func TestCursorStoreUpgradeKeepsTheOldCursor(t *testing.T) {
	const marker = "legacy-must-not-reread-tool"
	chats := filepath.Join(t.TempDir(), "chats")
	path := filepath.Join(chats, "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, path, marker, "/tmp/proj")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	pf, err := parseCursorStoreFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wouldIndex bool
	for _, m := range pf.messages {
		if strings.Contains(m.text, marker) {
			wouldIndex = true
		}
	}
	if !wouldIndex {
		t.Fatal("fixture produces no tool row; a re-read would be invisible")
	}

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	if err := upgradeSchemaWith(dbPath, previousSchemaSQL(t)); err != nil {
		t.Fatal(err)
	}
	oldDB, err := sql.Open(writerDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldDB.Exec(
		`INSERT INTO ingest_state (path, offset, recorded_size, recorded_mtime) VALUES (?, ?, ?, ?)`,
		path, info.Size(), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := oldDB.Exec(
		`INSERT INTO messages (session_id, project, role, text, timestamp, content_type)
		 VALUES (?, 'proj', 'user', 'legacy cursor sentinel', '2020-01-01T00:00:00Z', 'text')`,
		cursorSessUUID); err != nil {
		t.Fatal(err)
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(dbPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.AwaitStartup()

	s.SetCursorChatRoots([]string{chats})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if n := countText(t, s, marker); n != 0 {
		t.Fatalf("upgrade re-read a caught-up store.db (%d new rows)", n)
	}
	expectOnce(t, s, "legacy cursor sentinel")
	var off int64
	var prefix sql.NullString
	if err := s.readDB.QueryRow(
		`SELECT offset, prefix_sha256 FROM ingest_state WHERE path = ?`, path).Scan(&off, &prefix); err != nil {
		t.Fatal(err)
	}
	if off != info.Size() {
		t.Fatalf("upgrade moved the cursor from %d to %d", info.Size(), off)
	}
	if !prefix.Valid || prefix.String == "" {
		t.Fatal("legacy cursor was not given a fingerprint")
	}
	if prefix.String != cursorStoreFingerprint(path) {
		t.Fatalf("adopted fingerprint %q, file is %q", prefix.String, cursorStoreFingerprint(path))
	}
}
