# Defect hunt — mnemo (2026-11-20)

Read-only hunt. Product code and the existing tests were not changed.
Every reproduction used `newTestStore` (a temporary database) and
synthetic transcript files. The installed daemon, `~/.mnemo`, and the
real transcript directories were not opened.

Evidence is `GOWORK=off go test -tags sqlite_fts5 -count=1 -v -run TestDefect ./internal/store`
against this tree. The run failed. Each failure line is quoted under
the defect it proves. The tests live in `package store` and call
unexported parsers and `newTestStore`, `writeJSONL`, `msg`,
`writeGrokSession`, `writeCursorStoreFixture`, `writerDriverName`.
`countText` is the helper in the first reproduction.

## Confirmed defects

### 1. An ingest offset never advances past an incomplete trailing line

**Severity:** High. One finished record is dropped and never comes
back, on every JSONL ingest path, while the lines around it are stored.
A watcher that runs during a write (the live debouncer is 300ms) or a
crash mid-line is enough. The session then looks healthy.

**Mechanism:** `ReadBytes('\n')` returns the partial tail together with
`io.EOF`. The reader adds those bytes to `consumed` before
`json.Unmarshal`, which fails and skips the line. The same transaction
commits `ingest_state.offset` at that byte. The next pass seeks there,
so the completed record's prefix is never read and its suffix is not a
JSON line. A later pass does not recover it. The comments at
`store.go:4590` and `store.go:7421` call this offset a true line
boundary. `TestIngestFileMidStreamOffsetOnBoundary` only commits
complete lines.

**Where:**

- `internal/store/store.go:7428` (`ingestFile` loop) and `:7498` (the commit)
- `internal/store/store.go:4596` (`parseFile`, used by `IngestAll`)
- `internal/store/codex.go:341`
- `internal/store/grok.go:312`
- `internal/store/cursor.go:229`
- `internal/store/omp.go:237`

**Smallest fix:** If the chunk does not end in `\n`, leave it unread:
do not parse it and do not add its length to the offset that will be
committed. Do this in all six loops, or in one shared reader.

**Reproduction:**

```go
func countText(t *testing.T, s *Store, needle string) int {
	t.Helper()
	var n int
	err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM messages_v WHERE text LIKE ?`, "%"+needle+"%",
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDefectPartialLineCommitSkipsTheCompletedRecord(t *testing.T) {
	projectDir := t.TempDir()
	proj := filepath.Join(projectDir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "sess-partial.jsonl")
	line1 := `{"type":"user","uuid":"u-alpha","timestamp":"2026-04-01T10:00:00Z","message":{"content":"alpha marker intact"}}` + "\n"
	partial := `{"type":"user","uuid":"u-beta","timestamp":"2026-04-01T10:00:01Z","message":{"content":"beta marker lost`
	if err := os.WriteFile(path, []byte(line1+partial), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, projectDir)
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	var off int64
	if err := s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&off); err != nil {
		t.Fatal(err)
	}
	rest := ` forever"}}` + "\n" +
		`{"type":"user","uuid":"u-gamma","timestamp":"2026-04-01T10:00:02Z","message":{"content":"gamma marker kept"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(rest); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}
	if n := countText(t, s, "beta marker lost"); n != 1 {
		t.Fatalf("completed record was not indexed after the torn line was finished (hits=%d, offset was %d)", n, off)
	}
}
```

Observed: `offset after torn read = 216, file = 216, last byte newline = false`, then `hits=0, offset was 216`. The same run reported `parseFile`, `parseCodexFile`, `parseGrokFile`, `parseCursorFile`, and `parseOmpFile` each advancing `newOffset` to the length of a buffer with no newline (`53`, `156`, `201`, `72`, `86`).

### 2. A Grok session's signals usage row is never removed unless a newer copy is written in the same transaction

**Severity:** High. Every Grok session that is ingested and then
appended loses the only usage row the pipeline stores for it. The new
turn is indexed, so the session looks current while its token total
becomes zero. This is the normal watcher path, not a race.

**Mechanism:** `parseGrokFile` builds the synthetic
`grok-<id>-signals-usage` entry, and the `[grok signals]` message,
only when `offset == 0`. `writeParsedFile` always, for
`source == "grok"`, deletes that message and that entry before
inserting. An incremental parse has no replacement row, so the delete
stands. Updating `signals.json` after the first byte has been consumed
does not recreate the row either, because the builder is gated on
`offset == 0`. The rows are `is_noise = 1`, so this is a loss in
`messages` / `entries`, not an FTS ghost.

**Where:** `internal/store/grok.go:329` and `internal/store/store.go:4340`.

**Smallest fix:** Delete the previous signals rows only in a
transaction that also inserts the replacement. Build that replacement
on every parse, including a tail, from the current `signals.json`.

**Reproduction:**

```go
func TestDefectGrokTailIngestDeletesSignalsUsage(t *testing.T) {
	grokHome := t.TempDir()
	path := writeGrokSession(t, grokHome, "/Users/dev/work/github.com/acme/webapp")
	s := newTestStore(t, t.TempDir())
	s.SetGrokRoots([]string{filepath.Join(grokHome, "sessions")})
	if err := s.ingestGrokFile(path); err != nil {
		t.Fatal(err)
	}
	var tokens int
	if err := s.readDB.QueryRow(
		`SELECT COALESCE(SUM(input_tokens), 0) FROM entries_v WHERE session_id = ?`, grokSessUUID,
	).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 12345 || countText(t, s, "[grok signals]") != 1 {
		t.Fatalf("first ingest tokens=%d signals=%d, want 12345 and 1", tokens, countText(t, s, "[grok signals]"))
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"timestamp":1783679400,"method":"session/update","params":{"sessionId":"` + grokSessUUID + `","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"a later turn that should not erase usage"}}}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "signals.json"),
		[]byte(`{"contextTokensUsed":99999,"contextWindowTokens":500000,"primaryModelId":"grok-4.5","toolCallCount":3,"turnCount":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestGrokFile(path); err != nil {
		t.Fatal(err)
	}
	n := countText(t, s, "[grok signals]")
	if err := s.readDB.QueryRow(
		`SELECT COALESCE(SUM(input_tokens), 0) FROM entries_v WHERE session_id = ?`, grokSessUUID,
	).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if n != 1 || tokens < 12345 {
		t.Fatalf("signals usage row did not survive the tail ingest (messages=%d input_tokens=%d)", n, tokens)
	}
}
```

Observed: `messages=0 input_tokens=0`. The appended turn was indexed (that assertion did not fire).

### 3. A failed read of a Cursor store.db never advances its ingest cursor

**Severity:** High. The index stays empty for that chat and will not
try again until the main database file grows past the size recorded
for the failed read. A file Cursor has created but not initialised, a
`SQLITE_BUSY` while Cursor holds the write lock, and an unfamiliar
schema all take this path. `openCursorStoreRO` sets `mode=ro` and no
`busy_timeout`, so a lock is a query error rather than a wait.

**Mechanism:** `parseCursorStoreFile` sets `newOffset` to `info.Size()`
before the query. `SELECT id, data FROM blobs` failing returns
`(pf, nil)`. `ingestCursorStoreFile` commits that parse. The next call
returns immediately when `offset >= info.Size() && offset > 0`. A
later valid database that is the same size or smaller is never opened.

**Where:** `internal/store/cursor_store.go:267`, `:279`, `:416`, and `:224`.

**Smallest fix:** Return the query error and do not write
`ingest_state` for that attempt. Set `newOffset` only after the query
succeeds. Put a busy timeout on the read-only open.

**Reproduction:**

```go
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
	_ = s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&off)
	if err := os.WriteFile(path, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	err2 := s.ingestCursorStoreFile(path)
	if n := countText(t, s, "beta-tool-result-marker"); n != 1 {
		t.Fatalf("valid store.db (%d bytes) was not indexed after a failed read of a larger file recorded offset %d (second err=%v hits=%d)", len(valid), off, err2, n)
	}
}
```

Observed: `unreadable ingest err=<nil> offset=28672`, then `valid store.db (20480 bytes) was not indexed ... offset 28672 (second err=<nil> hits=0)`.

### 4. A Cursor store.db is re-read when its contents change without the file growing

**Severity:** High. Tool-result text is rewritten inside an existing
SQLite page as a chat continues. The file size stays the same, the
watcher and `IngestAll` both treat the cursor as caught up, and the
index keeps the old blob forever.

**Mechanism:** The cursor is the main-file size (`os.Stat`), not a
content hash, mtime, or SQLite change counter. After one successful
ingest, `offset >= info.Size()` skips the file. An `UPDATE` that
replaces a blob with the same number of bytes leaves the main file at
20480 bytes. Page reuse and a WAL that grows beside the main file have
the same effect, because only the main file is measured.

**Where:** `internal/store/cursor_store.go:248` (the size cursor) and `:416`.

**Smallest fix:** Remember a fingerprint that changes when the pages
change (mtime plus size is the fingerprint already stored for JSONL;
SQLite also has the change counter in the header). Re-read when it
differs. `INSERT OR IGNORE` on `cursor-<session>-<blobId>` will not
replace a changed payload, so the re-read has to update the existing
row or delete it first.

**Reproduction:**

```go
func TestDefectCursorStoreSameSizeRewriteIsInvisible(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "chats", "abc", "11111111-2222-3333-4444-555555555555", "store.db")
	writeCursorStoreFixture(t, path, "original-tool-marker-xxxxxx", "/tmp/proj")
	s := newTestStore(t, t.TempDir())
	if err := s.ingestCursorStoreFile(path); err != nil {
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
	updated, _ := res.RowsAffected()
	db.Close()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ingestCursorStoreFile(path); err != nil {
		t.Fatal(err)
	}
	n := countText(t, s, "original-tool-marker-YYYYYY")
	if after.Size() <= before.Size() && n != 1 {
		t.Fatalf("same-size store.db (%d bytes) was not re-read after its blob changed (rows=%d)", after.Size(), updated)
	}
}
```

Observed: `in-place rows=1 size 20480 -> 20480 hits=0`.

### 5. A transcript whose bytes before the recorded offset no longer match what was ingested is not treated as an append

**Severity:** High. A replacement file that is longer than the old
cursor is parsed from the middle. The new record is dropped, the old
rows stay, and source-drift does not tag the file: growth is neither
"truncated" (`size < offset`) nor "rewritten" (same size, newer
mtime). Same-size and shorter replacements are left as the durable
copy on purpose (`sourcetier.go:74`); this defect is only the longer
replacement.

**Mechanism:** `IngestAll` skips a file only when `offset >= size`.
Otherwise it calls `parseFile(path, offset)`, which seeks to the old
offset and treats whatever follows as new lines. Those bytes are the
tail of a JSON object that did not exist in the original file, so
`json.Unmarshal` skips them. The old messages remain.

**Where:** `internal/store/store.go:3827`, `:3979`, `:4495`. Drift classification is `internal/store/sourcetier.go:110`.

**Smallest fix:** Before seeking, compare a prefix fingerprint with
what was ingested. On a mismatch, parse from byte 0. `INSERT OR IGNORE`
keeps rows whose uuids differ, so a from-zero parse of a Claude file
adds the new line and also leaves the old lines; Codex uuids are the
byte offset, so a from-zero parse keeps the stale payload. The mismatch
path has to replace that session's rows, or the fingerprint check has
nothing to repair.

**Reproduction:**

```go
func TestDefectGrownRewriteIsReadFromTheOldOffset(t *testing.T) {
	projectDir := t.TempDir()
	proj := filepath.Join(projectDir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "sess-rewrite.jsonl")
	original := `{"type":"user","uuid":"old-1","timestamp":"2026-04-01T10:00:00Z","message":{"content":"old alpha"}}` + "\n" +
		`{"type":"user","uuid":"old-2","timestamp":"2026-04-01T10:00:01Z","message":{"content":"old beta"}}` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var off int64
	if err := s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&off); err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", int(off))
	rewritten := `{"type":"user","uuid":"new-1","timestamp":"2026-04-01T11:00:00Z","message":{"content":"brand new preamble ` + pad + ` end"}}` + "\n"
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if n := countText(t, s, "brand new preamble"); n != 1 {
		t.Fatalf("replacement longer than the recorded offset was not indexed from the start (hits=%d, old alpha=%d, offset=%d)", n, countText(t, s, "old alpha"), off)
	}
}
```

Observed: `hits=0, old alpha=1, offset=199`.

### 6. A message that fails to insert is not skipped forever, and the offset is not committed past it

**Severity:** Medium. It needs an insert failure (constraint, disk,
trigger). When that happens the ingest returns success, the lines
around the failure are stored, and the failed line is never retried.
The session looks complete.

**Mechanism:** `insertMessage` turns a statement error into `(0, isNoise)`.
`ingestFile` does not look at that result. `RAISE(ABORT)` undoes the
one statement and leaves the transaction open, so the offset update
and `Commit` succeed. The next `ingestFile` sees `offset >= size` and
seeks to EOF. `writeParsedFile` has the same shape: an entry-insert
error is logged and skipped, a message whose `entryID` is 0 is
skipped, and the offset is still written.

**Where:** `internal/store/store.go:4255`, `:7398`, `:7498`. The batch writer is `:4354` and `:4373`.

**Smallest fix:** Return the error from `insertMessage`. If a line's
entry or message insert fails, return that error without committing an
offset past the line.

**Reproduction:**

```go
func TestDefectMessageInsertErrorIsPermanent(t *testing.T) {
	projectDir := t.TempDir()
	path := writeJSONL(t, projectDir, "proj", "sess-poison", []map[string]any{
		msg("user", "before poison marker", "2026-04-01T10:00:00Z"),
		msg("user", "POISONMARKER please drop me", "2026-04-01T10:00:01Z"),
		msg("user", "after poison marker", "2026-04-01T10:00:02Z"),
	})
	s := newTestStore(t, projectDir)
	if _, err := s.writeDB.Exec(`
		CREATE TRIGGER defect_poison_msg BEFORE INSERT ON messages
		WHEN new.text LIKE '%POISONMARKER%'
		BEGIN
			SELECT RAISE(ABORT, 'poison');
		END`); err != nil {
		t.Fatal(err)
	}
	err1 := s.ingestFile(path)
	var off int64
	_ = s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&off)
	info, _ := os.Stat(path)
	if _, err := s.writeDB.Exec(`DROP TRIGGER defect_poison_msg`); err != nil {
		t.Fatal(err)
	}
	_ = s.ingestFile(path)
	if n := countText(t, s, "POISONMARKER"); n != 1 {
		t.Fatalf("message that failed to insert is still absent after the cause is gone (hits=%d, first err=%v, offset=%d, file=%d)", n, err1, off, info.Size())
	}
}
```

Observed: `first ingest err=<nil> offset=294 file=294 before=1 poison=0 after=1`, then `second ingest err=<nil> poison=0`.

## Unconfirmed suspicions

1. **Watch can move a cursor backwards.** `ingestFile` writes the offset with `INSERT OR REPLACE` (`store.go:7499`). `writeParsedFile` uses `MAX(offset)` (`store.go:4456`). `Watch` starts before the cold `IngestAll`. A stale watch callback could replace a newer offset. Same-path debouncer callbacks are serialised. Not reproduced as lost rows.

2. **Shutdown does not join an in-flight debounced ingest.** `debouncer.stop` drops unfired timers and does not wait for the callback that is inside `ingestFile`. `Registry.Close` waits `workerDrainGrace` (3s) and then checkpoints. The offset is committed in the same transaction as the inserts, so a restart should resume at the last commit. Not shown to skip a tail.

3. **The replicator accepts a same-name, same-size destination file without hashing it** (`internal/backup/replicate.go:294`) and then deletes the staged copy. `uniqueDest` (`internal/backup/worker.go:681`) steps the timestamp so two snapshots in the same second do not share a name. A realistic sequence that discards the only good copy was not built. `BackupWith` does run `PRAGMA integrity_check` and a full decode of the compressed file before retention removes the previous snapshot. Missing verification is not the suspicion.

4. **`messages_fts` has insert triggers and no delete or update trigger.** `dedupe.go` and `selfnoise.go` issue an explicit FTS `delete` before they remove or retitle a row. The Grok signals delete is `is_noise = 1`, and the insert trigger does not index those rows. No orphaned FTS row was shown. Compression leaves search text in `text_z`, and search reads `mnemo_text`.

5. **The hours cursor advances across timestamps it cannot parse.** That loses a minute of attendance only if a real transcript timestamp takes that branch. Not exercised.

6. **`ingestFile` ignores the error from `Seek` (`store.go:7314`) and from the `ingest_state` `Exec` (`store.go:7499`).** A failed seek would read from byte 0. A failed offset statement can still commit the inserts, after which the in-memory cursor is set, so the process will not retry until restart. Not reproduced.

## Coverage

Read closely, and covered by the reproductions where a defect is
listed above: the JSONL readers and their offset commits (`ingestFile`,
`parseFile`, Codex, Grok, Cursor, Oh My Pi), `writeParsedFile`,
`IngestAll`'s size check and seek, the Cursor `store.db` reader,
source-drift classification, `insertMessage`'s error return, the
`messages_fts` insert triggers and the writers that delete message
rows, backup verification (`VACUUM INTO`, `integrity_check`,
compressed-file decode), and the replicator's same-size short-circuit.

Read enough to decide they were not the defect, and not line-by-line:
registry startup order (`Watch` before `IngestAll`), `Registry.Close`
and the WAL checkpoint, the debouncer, `backup.answer` and staging,
`uniqueDest`.

Not read line by line: the compression packer and dictionary training,
the hours materialiser beyond the timestamp cursor, vault clustering,
federation, the compactor and segmenter, the GitHub and commit
mirrors, docs/plans/targets ingest, the image pipeline, pricing and
budget, the MCP tool adapters, and the menubar shim.
