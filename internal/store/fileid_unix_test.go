// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package store

import (
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSameSizeChangeDuringReadDiscardsStaleSnapshot(t *testing.T) {
	for _, mode := range []string{"rotated with same mtime", "rewritten with new mtime"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			s := newReaderStore(t, readerCodex, root)
			path := readerCodex.pathFor(t, root)
			readerCodex.prepare(t, s, root, path)
			// Keep the changed record beyond the sampled prefix and retain
			// the same length. Only identity or mtime can detect the edit.
			first := readerCodex.line(strings.Repeat("p", 72<<10))
			const stale = "old snapshot tail marker"
			const fresh = "new snapshot tail marker"
			writeLines(t, path, first+readerCodex.line(stale))
			stamp := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			testAfterJSONLLine = func(p string, n int) {
				if p != path || n != 1 {
					return
				}
				once.Do(func() {
					mtime := stamp.Add(time.Second)
					if mode == "rotated with same mtime" {
						if err := os.Rename(path, path+".rotated"); err != nil {
							t.Fatal(err)
						}
						mtime = stamp
					}
					writeLines(t, path, first+readerCodex.line(fresh))
					if err := os.Chtimes(path, mtime, mtime); err != nil {
						t.Fatal(err)
					}
				})
			}
			t.Cleanup(func() { testAfterJSONLLine = nil })
			readerCodex.ingest(t, s, path)
			expectOnce(t, s, fresh)
			if n := countText(t, s, stale); n != 0 {
				t.Fatalf("stale snapshot tail indexed %d times", n)
			}
		})
	}
}

func TestCaughtUpRestartLearnsInodeBeforeSameStatRotation(t *testing.T) {
	root := t.TempDir()
	path := readerClaudeBatch.pathFor(t, root)
	old := readerClaudeBatch.line("old same-stat record")
	replacement := readerClaudeBatch.line("new same-stat record")
	writeLines(t, path, old)
	stamp := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, root)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.DBPath(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	reopened.AwaitStartup()
	if err := reopened.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".rotated"); err != nil {
		t.Fatal(err)
	}
	writeLines(t, path, replacement)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := reopened.IngestAll(); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, reopened, "old same-stat record")
	expectOnce(t, reopened, "new same-stat record")
}

type transcriptFileInfoWithStat struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (i transcriptFileInfoWithStat) Sys() any { return i.stat }

func TestSameInodeOnAnotherDeviceRestartsTranscript(t *testing.T) {
	root := t.TempDir()
	s := newTestStore(t, root)
	path := readerClaudeWatch.pathFor(t, root)
	writeLines(t, path, readerClaudeWatch.line("device identity marker"))
	readerClaudeWatch.ingest(t, s, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := *info.Sys().(*syscall.Stat_t)
	other.Dev ^= 1
	if d := s.jsonlResume(path, transcriptFileInfoWithStat{info, &other}); !d.restarted || d.statusClass != "rewritten_at" {
		t.Fatalf("resume on a different device = %+v, want a rewrite from byte zero", d)
	}
}
