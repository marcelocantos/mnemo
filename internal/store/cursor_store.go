// Cursor Agent CLI store.db fidelity (🎯T149.1). The JSONL under
// agent-transcripts is the linearized transcript; ~/.cursor/chats/<md5>/<id>/store.db
// is the CLI's content-addressed source of truth and holds tool results
// plus session meta (title, model, cwd) that JSONL omits. This file
// maps those blobs into the same parsedFile intermediate without
// re-indexing user/assistant text already covered by the JSONL path.
// See docs/design/cursor-ingest.md § Fidelity.
package store

import (
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CursorChatRootsFor returns the candidate Cursor Agent CLI chat roots
// (~/.cursor/chats). ACP sessions under acp-sessions/ use the same
// schema but are a separate product surface (0 id overlap with chats on
// observed machines) and are deliberately not discovered here — see
// TestCursorACPSessionsExcluded.
func CursorChatRootsFor(home string) []string {
	base := os.Getenv("CURSOR_HOME")
	if base == "" {
		base = filepath.Join(home, ".cursor")
	}
	return []string{filepath.Join(base, "chats")}
}

func cursorHomeBase() string {
	if base := os.Getenv("CURSOR_HOME"); base != "" {
		return base
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cursor")
}

// isCursorStore reports whether path is a Cursor Agent CLI store.db:
// .../chats/<workspace-hash>/<uuid>/store.db. Rejects acp-sessions,
// WAL/SHM sidecars, and any other sqlite under ~/.cursor.
func isCursorStore(path string) bool {
	base := filepath.Base(path)
	if base != "store.db" {
		return false
	}
	// .../chats/<hash>/<uuid>/store.db → grandparent of parent is "chats"
	sessionDir := filepath.Dir(path)
	hashDir := filepath.Dir(sessionDir)
	chatsDir := filepath.Dir(hashDir)
	return filepath.Base(chatsDir) == "chats" &&
		filepath.Base(sessionDir) != "" &&
		filepath.Base(hashDir) != "chats"
}

func cursorStoreSessionID(path string) string {
	return filepath.Base(filepath.Dir(path))
}

// cursorChatSessionDir locates ~/.cursor/chats/*/<sessionID>/ for a
// session. Returns "" when absent. Used by JSONL ingest to pull
// meta.json cwd/title before store.db is walked.
func cursorChatSessionDir(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	root := filepath.Join(cursorHomeBase(), "chats")
	matches, err := filepath.Glob(filepath.Join(root, "*", sessionID))
	if err != nil {
		return ""
	}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			return m
		}
	}
	return ""
}

type cursorChatMeta struct {
	cwd   string
	title string
	model string
}

// cursorReadChatMeta loads meta.json + store.db meta for a session dir.
// Missing files are fine; unknown shapes skip-and-continue.
func cursorReadChatMeta(sessionDir string) cursorChatMeta {
	var out cursorChatMeta
	if sessionDir == "" {
		return out
	}
	if raw, err := os.ReadFile(filepath.Join(sessionDir, "meta.json")); err == nil {
		var mj struct {
			Cwd   string `json:"cwd"`
			Title string `json:"title"`
			Name  string `json:"name"`
		}
		if json.Unmarshal(raw, &mj) == nil {
			out.cwd = strings.TrimSpace(mj.Cwd)
			out.title = strings.TrimSpace(mj.Title)
			if out.title == "" {
				out.title = strings.TrimSpace(mj.Name)
			}
		}
	}
	storePath := filepath.Join(sessionDir, "store.db")
	if fi, err := os.Stat(storePath); err != nil || fi.IsDir() {
		return out
	}
	db, err := openCursorStoreRO(storePath)
	if err != nil {
		return out
	}
	defer db.Close()
	sm := cursorStoreMetaRows(db)
	if out.cwd == "" {
		out.cwd = sm.cwd
	}
	if out.title == "" {
		out.title = sm.title
	}
	out.model = sm.model
	return out
}

func cursorStoreMetaRows(db *sql.DB) cursorChatMeta {
	var out cursorChatMeta
	rows, err := db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw any
		if err := rows.Scan(&key, &raw); err != nil {
			continue
		}
		val := cursorDecodeMetaValue(raw)
		if val == nil {
			continue
		}
		switch key {
		case "title", "name", "cwd", "updatedAtMs":
			cursorMergeChatMeta(&out, map[string]any{key: val})
		}
		if m, ok := val.(map[string]any); ok {
			cursorMergeChatMeta(&out, m)
		}
	}
	return out
}

func cursorMergeChatMeta(dst *cursorChatMeta, m map[string]any) {
	if dst.cwd == "" {
		if v, ok := m["cwd"].(string); ok {
			dst.cwd = strings.TrimSpace(v)
		}
	}
	if dst.title == "" {
		for _, k := range []string{"title", "name"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				dst.title = strings.TrimSpace(v)
				break
			}
		}
	}
	if dst.model == "" {
		if v, ok := m["lastUsedModel"].(string); ok {
			dst.model = strings.TrimSpace(v)
		}
	}
}

func cursorDecodeMetaValue(raw any) any {
	switch v := raw.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		// Live store.db encodes JSON meta as hex text.
		if b, err := hex.DecodeString(v); err == nil {
			var out any
			if json.Unmarshal(b, &out) == nil {
				return out
			}
			return string(b)
		}
		var out any
		if json.Unmarshal([]byte(v), &out) == nil {
			return out
		}
		return v
	case []byte:
		if len(v) == 0 {
			return nil
		}
		if b, err := hex.DecodeString(string(v)); err == nil {
			var out any
			if json.Unmarshal(b, &out) == nil {
				return out
			}
		}
		var out any
		if json.Unmarshal(v, &out) == nil {
			return out
		}
		return string(v)
	default:
		return v
	}
}

// cursorStoreBusyTimeout is how long a read-only open waits when Cursor
// holds the write lock. Without it a lock is an immediate query error.
const cursorStoreBusyTimeout = 5000

// sqliteHeaderMagic is the 16-byte header. The file change counter is the
// big-endian uint32 at offset 24. WAL mode may leave that counter alone
// until a checkpoint, so the fingerprint also names the -wal sibling.
const sqliteHeaderMagic = "SQLite format 3\x00"

func openCursorStoreRO(path string) (*sql.DB, error) {
	// mode=ro avoids taking a write lock on a store Cursor may hold open.
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d", filepath.ToSlash(path), cursorStoreBusyTimeout)
	return sql.Open("sqlite3", dsn)
}

// cursorStoreFingerprint is the identity stored in prefix_sha256 for a
// store.db. It changes when the main file's size, mtime, or header
// change counter changes, and when the sibling -wal appears, grows, or
// is rewritten. An empty string means the file could not be stated.
func cursorStoreFingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	walSize, walMtime := cursorStoreSidecar(path + "-wal")
	return fmt.Sprintf("size=%d mtime=%s change=%d wal_size=%d wal_mtime=%s",
		info.Size(),
		info.ModTime().UTC().Format(time.RFC3339Nano),
		sqliteChangeCounter(path),
		walSize,
		walMtime)
}

func cursorStoreSidecar(path string) (int64, string) {
	info, err := os.Stat(path)
	if err != nil {
		return -1, ""
	}
	return info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano)
}

func sqliteChangeCounter(path string) uint32 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, sqliteChangeCounterOff+4)
	if _, err := io.ReadFull(f, buf); err != nil {
		return 0
	}
	if string(buf[:len(sqliteHeaderMagic)]) != sqliteHeaderMagic {
		return 0
	}
	return binary.BigEndian.Uint32(buf[sqliteChangeCounterOff:])
}

const sqliteChangeCounterOff = 24

// cursorStoreCaughtUp is true only when a previous read committed a
// cursor at or past the current main-file size and the fingerprint still
// matches. A failed read leaves no cursor. Growth, a shrink, an empty
// fingerprint, or any fingerprint change is read again. A shorter
// store.db is not the JSONL durable-truncation tier.
func (s *Store) cursorStoreCaughtUp(path string, size int64) bool {
	s.mu.Lock()
	off := s.offsets[path]
	stored := s.gens[path].prefix
	s.mu.Unlock()
	if off <= 0 || off < size {
		return false
	}
	fp := cursorStoreFingerprint(path)
	return stored != "" && stored == fp
}

type cursorStoreToolBlob struct {
	Role    string          `json:"role"`
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type cursorStoreToolPart struct {
	Type       string          `json:"type"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Result     json.RawMessage `json:"result"`
	Content    json.RawMessage `json:"content"`
	Text       string          `json:"text"`
}

// parseCursorStoreFile reads tool-result JSON blobs from a Cursor CLI
// store.db. Binary DAG nodes, system/user/assistant JSON, and unknown
// shapes are skipped. offset is ignored for reading (SQLite is not
// append-log). newOffset is the file size only after the blob query
// succeeds: a failed read must not hand the caller a cursor to commit.
func parseCursorStoreFile(path string, offset int64) (parsedFile, error) {
	_ = offset
	info, err := os.Stat(path)
	if err != nil {
		return parsedFile{}, err
	}
	sessionID := cursorStoreSessionID(path)
	sessionDir := filepath.Dir(path)
	meta := cursorReadChatMeta(sessionDir)
	cwd := meta.cwd
	pf := parsedFile{
		path:      path,
		sessionID: sessionID,
		source:    "cursor",
		cwd:       cwd,
		project:   cursorProject(cwd),
		topic:     meta.title,
		model:     meta.model,
	}
	if meta.title != "" && len(pf.topic) > 200 {
		pf.topic = pf.topic[:197] + "..."
	}

	db, err := openCursorStoreRO(path)
	if err != nil {
		return parsedFile{}, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT id, data FROM blobs`)
	if err != nil {
		// Unreadable, locked past the busy timeout, or not a store yet.
		// The caller must not record a cursor for this attempt.
		return parsedFile{}, err
	}
	defer rows.Close()

	fallbackTS := info.ModTime().UTC().Format(time.RFC3339)
	for rows.Next() {
		var blobID string
		var data []byte
		if err := rows.Scan(&blobID, &data); err != nil {
			continue
		}
		if len(data) == 0 || data[0] != '{' {
			continue // binary DAG / non-JSON
		}
		var blob cursorStoreToolBlob
		if json.Unmarshal(data, &blob) != nil {
			continue
		}
		if strings.ToLower(blob.Role) != "tool" {
			continue // system/user/assistant — JSONL already covers conversation
		}
		parts := cursorStoreToolParts(blob.Content)
		for i, part := range parts {
			text := cursorStoreResultText(part)
			if strings.TrimSpace(text) == "" {
				continue
			}
			toolID := part.ToolCallID
			if toolID == "" {
				toolID = blob.ID
			}
			name := part.ToolName
			uuid := fmt.Sprintf("cursor-%s-%s", sessionID, blobID)
			if i > 0 {
				uuid = fmt.Sprintf("%s-%d", uuid, i)
			}
			rawObj := map[string]any{
				"uuid": uuid,
				"type": "user",
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{{
						"type":        "tool_result",
						"tool_use_id": toolID,
						"content":     text,
					}},
				},
			}
			if meta.model != "" {
				rawObj["message"].(map[string]any)["model"] = meta.model
			}
			raw, err := json.Marshal(rawObj)
			if err != nil {
				continue
			}
			entryIdx := len(pf.entries)
			pf.entries = append(pf.entries, parsedRawEntry{
				entryType: "user",
				timestamp: fallbackTS,
				raw:       raw,
			})
			pf.messages = append(pf.messages, parsedMessage{
				entryIdx:    entryIdx,
				role:        "user",
				typ:         "user",
				contentType: "tool_result",
				toolName:    name,
				toolUseID:   toolID,
				text:        text,
				timestamp:   fallbackTS,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return parsedFile{}, err
	}
	pf.newOffset = info.Size()
	return pf, nil
}

func cursorStoreToolParts(raw json.RawMessage) []cursorStoreToolPart {
	if len(raw) == 0 {
		return nil
	}
	var arr []cursorStoreToolPart
	if json.Unmarshal(raw, &arr) == nil {
		var out []cursorStoreToolPart
		for _, p := range arr {
			if p.Type == "tool-result" || p.Type == "tool_result" || p.ToolName != "" || len(p.Result) > 0 {
				out = append(out, p)
			}
		}
		return out
	}
	var one cursorStoreToolPart
	if json.Unmarshal(raw, &one) == nil {
		return []cursorStoreToolPart{one}
	}
	return nil
}

func cursorStoreResultText(p cursorStoreToolPart) string {
	if s := cursorJSONText(p.Result); s != "" {
		return s
	}
	if s := cursorContentText(p.Content); s != "" {
		return s
	}
	return strings.TrimSpace(p.Text)
}

func cursorJSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Non-string JSON (object/array/number) — index the compact form.
	return string(raw)
}

// ingestCursorStoreFile ingests tool results from a Cursor CLI store.db.
// A file whose main-file size has not grown is still read when its
// fingerprint changed. The blob uuids in this parse are deleted and
// inserted again, because INSERT OR IGNORE will not replace a payload.
func (s *Store) ingestCursorStoreFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if s.cursorStoreCaughtUp(path, info.Size()) {
		return nil
	}
	s.mu.Lock()
	offset := s.offsets[path]
	s.mu.Unlock()

	pf, err := parseCursorStoreFile(path, offset)
	if err != nil {
		return err
	}

	ws, err := s.newWriterState()
	if err != nil {
		return err
	}
	defer func() { _ = ws.tx.Rollback() }()
	defer ws.Close()

	if err := s.writeParsedFile(ws, pf); err != nil {
		return err
	}

	return s.commitIngest(ws)
}

func cursorEntryUUID(raw []byte) string {
	var probe struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	return probe.UUID
}

// replaceCursorStoreBlobs deletes the entries this parse is about to
// insert, and only those. messages_fts is external content, so the
// delete command is issued before the row goes, matching
// dedupeDeleteBatch. Other rows for the session — the JSONL transcript
// included — stay.
func replaceCursorStoreBlobs(tx *sql.Tx, sessionID string, entries []parsedRawEntry) error {
	if sessionID == "" {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		uuid := cursorEntryUUID(e.raw)
		if uuid == "" {
			continue
		}
		if _, ok := seen[uuid]; ok {
			continue
		}
		seen[uuid] = struct{}{}
		var entryID int64
		err := tx.QueryRow(`
			SELECT id FROM entries
			WHERE session_id = ? AND COALESCE(uuid_m, uuid) = ?`,
			sessionID, uuid).Scan(&entryID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO messages_fts(messages_fts, rowid, text, role, project, session_id)
			SELECT 'delete', m.id, m.text, m.role, m.project, m.session_id
			FROM messages_v m WHERE m.entry_id = ? AND m.is_noise = 0`, entryID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO messages_fts_stem(messages_fts_stem, rowid, text, role, project, session_id)
			SELECT 'delete', m.id, m.text, m.role, m.project, m.session_id
			FROM messages_v m WHERE m.entry_id = ? AND m.is_noise = 0
			  AND EXISTS (SELECT 1 FROM messages_fts_stem_docsize ds WHERE ds.id = m.id)`, entryID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			DELETE FROM tool_outputs WHERE message_id IN (
				SELECT id FROM messages WHERE entry_id = ?)`, entryID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM messages WHERE entry_id = ?`, entryID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM entries WHERE id = ?`, entryID); err != nil {
			return err
		}
	}
	return nil
}
