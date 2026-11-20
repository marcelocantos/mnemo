// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
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
				if n := countText(t, s, "old alpha marker"); n != 0 {
					t.Fatalf("%s: old record still indexed %d times, want 0", mut, n)
				}
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
						t.Fatalf("pre-rewrite record indexed %d times, want 0", n)
					}
				}
			})
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
}
