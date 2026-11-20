// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Defect hunt 2026-11-20 #1 (and the longer-rewrite half of #5, which is
// the same cursor: an offset is only meaningful for the bytes it was
// taken from). A line torn across two reads must be indexed once, and so
// must a file that is appended, truncated, rotated, or rewritten while a
// reader is in the middle of it. Each of the six JSONL readers is covered.

type jsonlReaderKind string

const (
	readerClaudeWatch jsonlReaderKind = "claude-watch"
	readerClaudeBatch jsonlReaderKind = "claude-batch"
	readerCodex       jsonlReaderKind = "codex"
	readerGrok        jsonlReaderKind = "grok"
	readerCursor      jsonlReaderKind = "cursor"
	readerOmp         jsonlReaderKind = "omp"
)

func allJSONLReaders() []jsonlReaderKind {
	return []jsonlReaderKind{
		readerClaudeWatch, readerClaudeBatch,
		readerCodex, readerGrok, readerCursor, readerOmp,
	}
}

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

func (k jsonlReaderKind) line(marker string) string {
	switch k {
	case readerClaudeWatch, readerClaudeBatch:
		return `{"type":"user","uuid":"u-` + marker + `","timestamp":"2026-04-01T10:00:00Z","message":{"content":"` + marker + `"}}` + "\n"
	case readerCodex:
		return `{"timestamp":"2026-06-30T10:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + marker + `"}]}}` + "\n"
	case readerGrok:
		return `{"timestamp":1783670400,"method":"session/update","params":{"sessionId":"sess","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"` + marker + `"}}}}` + "\n"
	case readerCursor:
		return `{"role":"user","message":{"content":[{"type":"text","text":"` + marker + `"}]}}` + "\n"
	case readerOmp:
		return `{"type":"message","id":"m-` + marker + `","timestamp":"2026-04-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"` + marker + `"}]}}` + "\n"
	default:
		panic("unknown reader " + string(k))
	}
}

// pathFor lays the fixture out where that reader's router expects it.
func (k jsonlReaderKind) pathFor(t *testing.T, root string) string {
	t.Helper()
	var path string
	switch k {
	case readerClaudeWatch, readerClaudeBatch:
		dir := filepath.Join(root, "proj")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "sess-jsonl.jsonl")
	case readerCodex:
		dir := filepath.Join(root, "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "rollout-2026-06-30T10-00-00-019ee2a8-189f-7540-a035-1cc6ee7bd02f.jsonl")
	case readerGrok:
		dir := filepath.Join(root, "sessions", "019f4b93-3082-75f2-9c15-1aff0d5f2c3f")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "updates.jsonl")
	case readerCursor:
		id := cursorSessUUID
		dir := filepath.Join(root, "projects", "proj", "agent-transcripts", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, id+".jsonl")
	case readerOmp:
		dir := filepath.Join(root, "agent", "sessions", "-work-proj")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "2026-10-25T21-37-47-176Z_"+ompSessID+".jsonl")
	default:
		t.Fatalf("unknown reader %s", k)
	}
	return path
}

func (k jsonlReaderKind) ingest(t *testing.T, s *Store, path string) {
	t.Helper()
	var err error
	switch k {
	case readerClaudeWatch:
		err = s.ingestFile(path)
	case readerClaudeBatch:
		err = s.IngestAll()
	case readerCodex:
		err = s.ingestCodexFile(path)
	case readerGrok:
		err = s.ingestGrokFile(path)
	case readerCursor:
		err = s.ingestCursorFile(path)
	case readerOmp:
		err = s.ingestOmpFile(path)
	default:
		t.Fatalf("unknown reader %s", k)
	}
	if err != nil {
		t.Fatalf("%s ingest: %v", k, err)
	}
}

func (k jsonlReaderKind) prepare(t *testing.T, s *Store, root, path string) {
	t.Helper()
	switch k {
	case readerClaudeBatch:
		// IngestAll walks the store's project dir. root is that dir.
	case readerCodex:
		s.SetCodexRoots([]string{filepath.Dir(path)})
	case readerGrok:
		s.SetGrokRoots([]string{filepath.Join(root, "sessions")})
	case readerCursor:
		t.Setenv("CURSOR_HOME", root)
		s.SetCursorRoots([]string{filepath.Join(root, "projects")})
	case readerOmp:
		s.SetOmpRoots([]string{filepath.Join(root, "agent", "sessions")})
	}
}

func newReaderStore(t *testing.T, k jsonlReaderKind, root string) *Store {
	t.Helper()
	project := root
	if k != readerClaudeWatch && k != readerClaudeBatch {
		project = t.TempDir()
	}
	return newTestStore(t, project)
}

func writeLines(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLPartialLineIsIndexedOnce(t *testing.T) {
	for _, k := range allJSONLReaders() {
		t.Run(string(k), func(t *testing.T) {
			root := t.TempDir()
			s := newReaderStore(t, k, root)
			path := k.pathFor(t, root)
			k.prepare(t, s, root, path)

			whole := k.line("beta marker lost forever")
			// Split inside the JSON string, after a prefix that is not
			// itself a line. The first read must not commit past it.
			cut := len(whole) / 2
			if cut < 1 || whole[len(whole)-1] != '\n' {
				t.Fatal("fixture line has no newline")
			}
			writeLines(t, path, k.line("alpha marker intact")+whole[:cut])
			k.ingest(t, s, path)
			assertJSONLOffset(t, s, path, len(k.line("alpha marker intact")))

			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(whole[cut:] + k.line("gamma marker kept")); err != nil {
				t.Fatal(err)
			}
			f.Close()

			k.ingest(t, s, path)
			k.ingest(t, s, path)

			if n := countText(t, s, "beta marker lost forever"); n != 1 {
				t.Fatalf("completed record indexed %d times, want 1", n)
			}
			if n := countText(t, s, "alpha marker intact"); n != 1 {
				t.Fatalf("leading record indexed %d times, want 1", n)
			}
			if n := countText(t, s, "gamma marker kept"); n != 1 {
				t.Fatalf("trailing record indexed %d times, want 1", n)
			}
		})
	}
}

func assertJSONLOffset(t *testing.T, s *Store, path string, want int) {
	t.Helper()
	var offset int
	if err := s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if offset != want {
		t.Fatalf("stored offset = %d, want %d", offset, want)
	}
}

func TestJSONLFinalRecordWithoutNewline(t *testing.T) {
	for _, k := range allJSONLReaders() {
		t.Run(string(k), func(t *testing.T) {
			root := t.TempDir()
			s := newReaderStore(t, k, root)
			path := k.pathFor(t, root)
			k.prepare(t, s, root, path)
			body := strings.TrimSuffix(k.line("complete final record"), "\n")
			writeLines(t, path, body)
			k.ingest(t, s, path)
			k.ingest(t, s, path)
			expectOnce(t, s, "complete final record")
			assertJSONLOffset(t, s, path, len(body))

			writeLines(t, path, body+"\n"+k.line("record after final record"))
			k.ingest(t, s, path)
			expectOnce(t, s, "complete final record")
			expectOnce(t, s, "record after final record")
		})
	}
}

func TestJSONLLongTailSplitInsideUTF8(t *testing.T) {
	for _, k := range allJSONLReaders() {
		t.Run(string(k), func(t *testing.T) {
			root := t.TempDir()
			s := newReaderStore(t, k, root)
			path := k.pathFor(t, root)
			k.prepare(t, s, root, path)
			first := k.line("record before long tail")
			long := k.line("long tail " + strings.Repeat("x", 2<<20) + "é end of long tail")
			cut := strings.Index(long, "é") + 1
			writeLines(t, path, first+long[:cut])
			// An unchanged, incomplete file can be seen repeatedly before the producer resumes.
			for range 2 {
				k.ingest(t, s, path)
				assertJSONLOffset(t, s, path, len(first))
				if got := countText(t, s, "long tail xxx"); got != 0 {
					t.Fatalf("incomplete record indexed %d times", got)
				}
			}
			writeLines(t, path, first+long)
			k.ingest(t, s, path)
			k.ingest(t, s, path)
			expectOnce(t, s, "é end of long tail")
			assertJSONLOffset(t, s, path, len(first)+len(long))
		})
	}
}

func TestJSONLCursorMigrationPreservesOldOffset(t *testing.T) {
	// This revision predates the prefix column. Build its schema and write
	// the same durable cursor the old reader wrote for an incomplete line.
	oldSchema := previousSchemaSQL(t)
	root := t.TempDir()
	path := readerClaudeWatch.pathFor(t, root)
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	if err := upgradeSchemaWith(dbPath, oldSchema); err != nil {
		t.Fatal(err)
	}
	first := readerClaudeWatch.line("legacy committed record")
	second := readerClaudeWatch.line("finished legacy torn record")
	cut := len(second) / 2
	writeLines(t, path, first+second[:cut])
	oldDB, err := sql.Open(writerDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer oldDB.Close()
	if _, err := oldDB.Exec(entryInsertLegacySQL, "sess-jsonl", "proj", "user", "2026-04-01T10:00:00Z", strings.TrimSpace(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := oldDB.Exec(`INSERT INTO ingest_state (path, offset) VALUES (?, ?)`, path, len(first)+cut); err != nil {
		t.Fatal(err)
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(dbPath, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.AwaitStartup()
	assertJSONLOffset(t, s, path, len(first)+cut)
	var previousPrefix sql.NullString
	if err := s.readDB.QueryRow(`SELECT prefix_sha256 FROM ingest_state WHERE path = ?`, path).Scan(&previousPrefix); err != nil || previousPrefix.Valid {
		t.Fatalf("migrated prefix = %v, err = %v", previousPrefix, err)
	}
	writeLines(t, path, first+second)
	readerClaudeWatch.ingest(t, s, path)
	readerClaudeWatch.ingest(t, s, path)
	expectOnce(t, s, "finished legacy torn record")
	assertJSONLOffset(t, s, path, len(first)+len(second))
	var entries int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM entries WHERE session_id = ?`, "sess-jsonl").Scan(&entries); err != nil || entries != 2 {
		t.Fatalf("entries after upgrade = %d, err = %v", entries, err)
	}
}

func TestJSONLRotationBeforeMidstreamCommit(t *testing.T) {
	root := t.TempDir()
	s := newReaderStore(t, readerClaudeWatch, root)
	path := readerClaudeWatch.pathFor(t, root)
	line := readerClaudeWatch.line
	oldFirst, oldSecond := line("first record in rotated file"), line("second record in rotated file")
	replacement := line("only record in current file")
	writeLines(t, path, oldFirst+oldSecond)
	savedInterval, savedCheck := ingestCommitInterval, ingestLineCheckInterval
	ingestCommitInterval, ingestLineCheckInterval = 0, 1
	t.Cleanup(func() {
		ingestCommitInterval, ingestLineCheckInterval = savedInterval, savedCheck
		testMidStreamCommitOffset, testAfterJSONLLine = nil, nil
	})
	var commits []int64
	testMidStreamCommitOffset = func(offset int64) { commits = append(commits, offset) }
	var once sync.Once
	testAfterJSONLLine = func(p string, n int) {
		if p == path && n == 1 {
			once.Do(func() {
				if err := os.Rename(path, path+".rotated"); err != nil {
					t.Fatal(err)
				}
				writeLines(t, path, replacement)
			})
		}
	}
	planted := plantIndexedHistory(t, s, path)
	readerClaudeWatch.ingest(t, s, path)
	for _, offset := range commits {
		if offset == int64(len(oldFirst)+len(oldSecond)) {
			t.Fatalf("committed rotated-away record at offset %d", offset)
		}
	}
	expectOnce(t, s, "only record in current file")
	// The first line was committed before the rotation. A later read of
	// the replacement must not remove it. The second line never committed.
	expectOnce(t, s, "first record in rotated file")
	if n := countText(t, s, "second record in rotated file"); n != 0 {
		t.Fatalf("uncommitted rotated-away record indexed %d times", n)
	}
	assertPlantedHistory(t, s, planted)
}

func TestJSONLRewriteDuringReadBeyondPrefix(t *testing.T) {
	root := t.TempDir()
	s := newReaderStore(t, readerCodex, root)
	path := readerCodex.pathFor(t, root)
	readerCodex.prepare(t, s, root, path)
	first := readerCodex.line(strings.Repeat("p", jsonlPrefixBytes))
	writeLines(t, path, first+readerCodex.line("stale record beyond sampled prefix"))
	var once sync.Once
	testAfterJSONLLine = func(p string, n int) {
		if p == path && n == 1 {
			once.Do(func() {
				writeLines(t, path, first+readerCodex.line("replacement record beyond sampled prefix")+readerCodex.line("another replacement record"))
			})
		}
	}
	t.Cleanup(func() { testAfterJSONLLine = nil })
	planted := plantIndexedHistory(t, s, path)
	readerCodex.ingest(t, s, path)
	readerCodex.ingest(t, s, path)
	expectOnce(t, s, "replacement record beyond sampled prefix")
	expectOnce(t, s, "another replacement record")
	// The stale line was parsed and then the file changed under the
	// scan, so that transaction does not commit. Nothing already indexed
	// is removed to achieve that.
	if n := countText(t, s, "stale record beyond sampled prefix"); n != 0 {
		t.Fatalf("uncommitted stale record indexed %d times", n)
	}
	assertPlantedHistory(t, s, planted)
}

func TestCursorJSONLReplacementKeepsStoreResults(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_HOME", root)
	s := newReaderStore(t, readerCursor, root)
	path := readerCursor.pathFor(t, root)
	readerCursor.prepare(t, s, root, path)
	blobs := filepath.Join(root, "chats", "abc", cursorSessUUID, "store.db")
	writeCursorStoreFixture(t, blobs, "tool result from Cursor database", "/tmp/proj")
	writeLines(t, path, readerCursor.line("old Cursor conversation"))
	readerCursor.ingest(t, s, path)
	if err := s.ingestCursorStoreFile(blobs); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, "tool result from Cursor database")
	writeLines(t, path, readerCursor.line("replacement Cursor conversation in longer file"))
	readerCursor.ingest(t, s, path)
	expectOnce(t, s, "replacement Cursor conversation")
	expectOnce(t, s, "tool result from Cursor database")
	expectOnce(t, s, "old Cursor conversation")
	if class := sourceStatusClassFor(t, s, "old Cursor conversation"); class != "rewritten_at" {
		t.Fatalf("replaced Cursor transcript source_status class %q, want rewritten_at", class)
	}
}

func TestJSONLAppendIsIndexedOnce(t *testing.T) {
	for _, k := range allJSONLReaders() {
		t.Run(string(k), func(t *testing.T) {
			root := t.TempDir()
			s := newReaderStore(t, k, root)
			path := k.pathFor(t, root)
			k.prepare(t, s, root, path)
			writeLines(t, path, k.line("alpha marker intact"))
			k.ingest(t, s, path)

			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(k.line("appended marker once")); err != nil {
				t.Fatal(err)
			}
			f.Close()
			k.ingest(t, s, path)
			k.ingest(t, s, path)

			if n := countText(t, s, "alpha marker intact"); n != 1 {
				t.Fatalf("original record indexed %d times, want 1", n)
			}
			if n := countText(t, s, "appended marker once"); n != 1 {
				t.Fatalf("appended record indexed %d times, want 1", n)
			}
		})
	}
}

func TestJSONLTruncateBetweenReadsStaysOnce(t *testing.T) {
	for _, k := range allJSONLReaders() {
		t.Run(string(k), func(t *testing.T) {
			root := t.TempDir()
			s := newReaderStore(t, k, root)
			path := k.pathFor(t, root)
			k.prepare(t, s, root, path)
			first := k.line("kept marker after truncate")
			writeLines(t, path, first+k.line("dropped marker after truncate"))
			k.ingest(t, s, path)
			planted := plantIndexedHistory(t, s, path)

			// Shrink below the committed offset, keeping a complete first line.
			if err := os.Truncate(path, int64(len(first))); err != nil {
				t.Fatal(err)
			}
			k.ingest(t, s, path)
			k.ingest(t, s, path)

			if n := countText(t, s, "kept marker after truncate"); n != 1 {
				t.Fatalf("surviving record indexed %d times, want 1", n)
			}
			if n := countText(t, s, "dropped marker after truncate"); n != 1 {
				t.Fatalf("durable copy of the truncated tail indexed %d times, want 1", n)
			}
			if class := sourceStatusClassFor(t, s, "kept marker after truncate"); class != "truncated_at" {
				t.Fatalf("truncated source_status class %q, want truncated_at", class)
			}
			assertPlantedHistory(t, s, planted)
			assertNoDanglingRefs(t, s)
		})
	}
}

func TestJSONLRotateAndRewriteAreIndexedOnce(t *testing.T) {
	for _, k := range allJSONLReaders() {
		for _, mut := range []string{"rotate", "rewrite"} {
			t.Run(string(k)+"/"+mut, func(t *testing.T) {
				root := t.TempDir()
				s := newReaderStore(t, k, root)
				path := k.pathFor(t, root)
				k.prepare(t, s, root, path)
				writeLines(t, path, k.line("old alpha marker")+k.line("old beta marker"))
				k.ingest(t, s, path)
				planted := plantIndexedHistory(t, s, path)

				// Longer than the recorded offset, and a different prefix,
				// so a seek to the old offset lands mid-record.
				replacement := k.line("brand new preamble marker") + k.line("brand new tail marker")
				if mut == "rotate" {
					if err := os.Rename(path, path+".rotated"); err != nil {
						t.Fatal(err)
					}
				}
				writeLines(t, path, replacement)
				k.ingest(t, s, path)
				k.ingest(t, s, path)

				if n := countText(t, s, "brand new preamble marker"); n != 1 {
					t.Fatalf("%s: new preamble indexed %d times, want 1", mut, n)
				}
				if n := countText(t, s, "brand new tail marker"); n != 1 {
					t.Fatalf("%s: new tail indexed %d times, want 1", mut, n)
				}
				expectOnce(t, s, "old alpha marker")
				expectOnce(t, s, "old beta marker")
				if class := sourceStatusClassFor(t, s, "old alpha marker"); class != "rewritten_at" {
					t.Fatalf("%s: source_status class %q, want rewritten_at", mut, class)
				}
				assertPlantedHistory(t, s, planted)
				assertNoDanglingRefs(t, s)
			})
		}
	}
}

func TestJSONLMutatedWhileReadingIsIndexedOnce(t *testing.T) {
	for _, k := range allJSONLReaders() {
		for _, mut := range []string{"append", "truncate", "rotate", "rewrite"} {
			t.Run(string(k)+"/"+mut, func(t *testing.T) {
				root := t.TempDir()
				s := newReaderStore(t, k, root)
				path := k.pathFor(t, root)
				k.prepare(t, s, root, path)

				body := k.line("while read first marker") + k.line("while read second marker")
				writeLines(t, path, body)
				planted := plantIndexedHistory(t, s, path)

				var once sync.Once
				testAfterJSONLLine = func(p string, lines int) {
					if p != path || lines != 1 {
						return
					}
					once.Do(func() {
						switch mut {
						case "append":
							f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
							if err != nil {
								t.Errorf("append: %v", err)
								return
							}
							_, err = f.WriteString(k.line("while read appended marker"))
							f.Close()
							if err != nil {
								t.Errorf("append write: %v", err)
							}
						case "truncate":
							first := k.line("while read first marker")
							if err := os.Truncate(p, int64(len(first))); err != nil {
								t.Errorf("truncate: %v", err)
							}
						case "rotate":
							if err := os.Rename(p, p+".rotated"); err != nil {
								t.Errorf("rotate: %v", err)
								return
							}
							writeLines(t, p, k.line("while read rotated marker"))
						case "rewrite":
							writeLines(t, p, k.line("while read rewritten marker")+k.line("while read rewritten tail"))
						}
					})
				}
				t.Cleanup(func() { testAfterJSONLLine = nil })

				k.ingest(t, s, path)
				k.ingest(t, s, path)

				switch mut {
				case "append":
					expectOnce(t, s, "while read first marker")
					expectOnce(t, s, "while read second marker")
					expectOnce(t, s, "while read appended marker")
				case "truncate":
					expectOnce(t, s, "while read first marker")
					if n := countText(t, s, "while read second marker"); n != 0 {
						t.Fatalf("truncated-away record indexed %d times, want 0", n)
					}
				case "rotate":
					expectOnce(t, s, "while read rotated marker")
					if n := countText(t, s, "while read first marker"); n != 0 {
						t.Fatalf("pre-rotate record indexed %d times, want 0", n)
					}
				case "rewrite":
					expectOnce(t, s, "while read rewritten marker")
					expectOnce(t, s, "while read rewritten tail")
					if n := countText(t, s, "while read first marker"); n != 0 {
						t.Fatalf("uncommitted pre-rewrite record indexed %d times", n)
					}
				}
				assertPlantedHistory(t, s, planted)
				assertNoDanglingRefs(t, s)
			})
		}
	}
}

// previousSchemaSQL is the schema an upgrading daemon already has.
// The bytes live in testdata/schema_pre_prefix.sql. They were produced by
// `git show 9d3c830d16e5c111fb31c54682828c5b01a605b3:internal/store/schema.sql`
// (the merge-base of this branch and master on 2026-11-20) and committed
// so the test does not depend on that object still being in the clone.
func previousSchemaSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/schema_pre_prefix.sql")
	if err != nil {
		t.Fatalf("read previous schema fixture: %v", err)
	}
	return string(b)
}

// plantedHistory is one indexed row plus an image_occurrences reference
// to it. A source change must leave both in place.
type plantedHistory struct {
	entryID   int64
	messageID int64
}

func plantIndexedHistory(t *testing.T, s *Store, path string) plantedHistory {
	t.Helper()
	sessionID, ok := sessionIDFromPath(path)
	if !ok || sessionID == "" {
		t.Fatalf("no session id for %s", path)
	}
	const marker = "planted history marker"
	raw := `{"uuid":"planted-history-` + sessionID + `","type":"user","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"` + marker + `"}}`
	res, err := s.writeDB.Exec(
		`INSERT INTO entries (session_id, project, type, timestamp, raw) VALUES (?, 'proj', 'user', '2020-01-01T00:00:00Z', jsonb(?))`,
		sessionID, raw)
	if err != nil {
		t.Fatal(err)
	}
	entryID, err := res.LastInsertId()
	if err != nil || entryID == 0 {
		t.Fatalf("planted entry id %d: %v", entryID, err)
	}
	res, err = s.writeDB.Exec(
		`INSERT INTO messages (entry_id, session_id, project, role, text, timestamp, type, content_type)
		 VALUES (?, ?, 'proj', 'user', ?, '2020-01-01T00:00:00Z', 'user', 'text')`,
		entryID, sessionID, marker)
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := res.LastInsertId()
	if err != nil || messageID == 0 {
		t.Fatalf("planted message id %d: %v", messageID, err)
	}
	img, err := s.writeDB.Exec(
		`INSERT INTO images (content_hash, bytes, mime_type, width, height, pixel_format, byte_size)
		 VALUES (?, ?, 'text/plain', 1, 1, 'unknown', 4)`,
		"plant-"+sessionID, []byte("plant"))
	if err != nil {
		t.Fatal(err)
	}
	imageID, err := img.LastInsertId()
	if err != nil || imageID == 0 {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO image_occurrences (image_id, entry_id, message_id, session_id, source_type, occurred_at)
		 VALUES (?, ?, ?, ?, 'inline', '2020-01-01T00:00:00Z')`,
		imageID, entryID, messageID, sessionID); err != nil {
		t.Fatal(err)
	}
	return plantedHistory{entryID: entryID, messageID: messageID}
}

func assertPlantedHistory(t *testing.T, s *Store, planted plantedHistory) {
	t.Helper()
	expectOnce(t, s, "planted history marker")
	var gotEntry, gotMessage int64
	err := s.readDB.QueryRow(
		`SELECT e.id, m.id FROM entries e JOIN messages_v m ON m.entry_id = e.id WHERE m.text = ?`,
		"planted history marker").Scan(&gotEntry, &gotMessage)
	if err != nil {
		t.Fatal(err)
	}
	if gotEntry != planted.entryID || gotMessage != planted.messageID {
		t.Fatalf("planted row replaced: entry %d->%d message %d->%d", planted.entryID, gotEntry, planted.messageID, gotMessage)
	}
}

func sourceStatusClassFor(t *testing.T, s *Store, marker string) string {
	t.Helper()
	var status string
	err := s.readDB.QueryRow(`
		SELECT sm.source_status FROM session_meta sm
		WHERE sm.session_id = (
			SELECT session_id FROM messages_v WHERE text LIKE ? LIMIT 1
		)`, "%"+marker+"%").Scan(&status)
	if err != nil {
		t.Fatalf("source_status for %q: %v", marker, err)
	}
	return sourceStatusClass(status)
}

// assertNoDanglingRefs fails when a child row points at an entry or
// message id that is no longer there. image_occurrences is the case the
// transcript-replace delete left behind; the other references are the
// same shape.
func assertNoDanglingRefs(t *testing.T, s *Store) {
	t.Helper()
	checks := []struct {
		name string
		q    string
	}{
		{"image_occurrences.entry_id", `SELECT COUNT(*) FROM image_occurrences io WHERE io.entry_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM entries e WHERE e.id = io.entry_id)`},
		{"image_occurrences.message_id", `SELECT COUNT(*) FROM image_occurrences io WHERE io.message_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = io.message_id)`},
		{"messages.entry_id", `SELECT COUNT(*) FROM messages m WHERE m.entry_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM entries e WHERE e.id = m.entry_id)`},
		{"tool_outputs.message_id", `SELECT COUNT(*) FROM tool_outputs t WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = t.message_id)`},
		{"snapshot_files.entry_id", `SELECT COUNT(*) FROM snapshot_files sf WHERE NOT EXISTS (SELECT 1 FROM entries e WHERE e.id = sf.entry_id)`},
		{"decisions.proposal_msg_id", `SELECT COUNT(*) FROM decisions d WHERE d.proposal_msg_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = d.proposal_msg_id)`},
		{"decisions.confirmation_msg_id", `SELECT COUNT(*) FROM decisions d WHERE d.confirmation_msg_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = d.confirmation_msg_id)`},
	}
	for _, c := range checks {
		var n int
		if err := s.readDB.QueryRow(c.q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n != 0 {
			t.Errorf("%s has %d dangling rows", c.name, n)
		}
	}
}

func expectOnce(t *testing.T, s *Store, marker string) {
	t.Helper()
	if n := countText(t, s, marker); n != 1 {
		t.Fatalf("%s indexed %d times, want 1", marker, n)
	}
}

// Guard against the fixture lines drifting into something a parser skips.
func TestJSONLReaderFixturesAreIndexed(t *testing.T) {
	for _, k := range allJSONLReaders() {
		t.Run(string(k), func(t *testing.T) {
			root := t.TempDir()
			s := newReaderStore(t, k, root)
			path := k.pathFor(t, root)
			k.prepare(t, s, root, path)
			marker := fmt.Sprintf("fixture marker %s", k)
			writeLines(t, path, k.line(marker))
			k.ingest(t, s, path)
			if n := countText(t, s, marker); n != 1 {
				info, _ := os.Stat(path)
				var off int64
				_ = s.readDB.QueryRow(`SELECT offset FROM ingest_state WHERE path = ?`, path).Scan(&off)
				t.Fatalf("fixture not indexed (hits=%d size=%d offset=%d)", n, info.Size(), off)
			}
		})
	}
}

func TestDefectGrownRewriteIsReadFromTheOldOffset(t *testing.T) {
	// Report reproduction for defect 5. The padding makes the replacement
	// longer than the recorded offset so a resume seeks into the new line.
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
	expectOnce(t, s, "old alpha")
	expectOnce(t, s, "old beta")
	if class := sourceStatusClassFor(t, s, "old alpha"); class != "rewritten_at" {
		t.Fatalf("grown rewrite source_status class %q, want rewritten_at", class)
	}
}
