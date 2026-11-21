// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingTranscript counts Read and ReadAt. io.ReadFull and
// SectionReader both land on one of those.
type countingTranscript struct {
	*os.File
	reads *atomic.Int64
}

func (c *countingTranscript) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.File.Read(p)
}

func (c *countingTranscript) ReadAt(p []byte, off int64) (int, error) {
	c.reads.Add(1)
	return c.File.ReadAt(p, off)
}

func countTranscriptIO(t *testing.T) (opens, reads *atomic.Int64) {
	t.Helper()
	opens = &atomic.Int64{}
	reads = &atomic.Int64{}
	prev := openTranscript
	openTranscript = func(path string) (transcriptFile, error) {
		opens.Add(1)
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &countingTranscript{File: f, reads: reads}, nil
	}
	t.Cleanup(func() { openTranscript = prev })
	return opens, reads
}

func writeCaughtUpTree(t *testing.T, root string, n int) []byte {
	t.Helper()
	body := []byte("{\"type\":\"user\",\"uuid\":\"u-caught\",\"timestamp\":\"2026-04-01T00:00:00Z\",\"message\":{\"content\":\"caught-up marker\"}}\n")
	const shards = 100
	if n%shards != 0 {
		t.Fatalf("file count %d is not divisible by %d", n, shards)
	}
	per := n / shards
	var wg sync.WaitGroup
	const workers = 2
	errCh := make(chan error, workers)
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for shard := worker; shard < shards; shard += workers {
				dir := filepath.Join(root, fmt.Sprintf("p%03d", shard))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					errCh <- err
					return
				}
				for i := range per {
					path := filepath.Join(dir, fmt.Sprintf("s%05d.jsonl", shard*per+i))
					if err := os.WriteFile(path, body, 0o644); err != nil {
						errCh <- err
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	return body
}

func seedCaughtUpCursors(t *testing.T, db *sql.DB, root string, body []byte, want int) {
	t.Helper()
	sum := prefixHash(body)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO ingest_state (path, offset, recorded_size, recorded_mtime, prefix_sha256) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	n := 0
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".jsonl" {
			return err
		}
		mtime := info.ModTime().UTC().Format(time.RFC3339Nano)
		if _, err := stmt.Exec(path, info.Size(), info.Size(), mtime, sum); err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("seeded %d cursors, want %d", n, want)
	}
}

// TestStartupCaughtUpCostsOneStat is the startup scan over a tree the
// size of a real transcript corpus, with every file already caught up.
// The second store loads its cursors from disk: memory holds only what ingest_state
// stored. An unchanged file must not be opened or read.
func TestStartupCaughtUpCostsOneStat(t *testing.T) {
	const n = 100_000
	project := t.TempDir()
	body := writeCaughtUpTree(t, project, n)

	dbPath := filepath.Join(t.TempDir(), "scan.db")
	installTestRateCard(t, testRateCard())
	first, err := New(dbPath, project)
	if err != nil {
		t.Fatal(err)
	}
	first.AwaitStartup()
	seedCaughtUpCursors(t, first.writeDB, project, body, n)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(dbPath, project)
	if err != nil {
		t.Fatal(err)
	}
	second.AwaitStartup()
	t.Cleanup(func() { second.Close() })

	opens, reads := countTranscriptIO(t)
	start := time.Now()
	if err := second.IngestAll(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("caught-up startup files=%d elapsed=%s opens=%d reads=%d", n, elapsed.Round(time.Millisecond), opens.Load(), reads.Load())
	if opens.Load() != 0 || reads.Load() != 0 {
		t.Fatalf("caught-up startup opened %d files and read %d times, want 0 opens and 0 reads (one stat from the walk)", opens.Load(), reads.Load())
	}
}

func TestCaughtUpMtimeChangeIsHashed(t *testing.T) {
	project := t.TempDir()
	path := writeJSONL(t, project, "proj", "sess-mtime", []map[string]any{
		msg("user", "stable marker", "2026-04-01T10:00:00Z"),
	})
	s := newTestStore(t, project)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	opens, reads := countTranscriptIO(t)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 || reads.Load() != 0 {
		t.Fatalf("unchanged second pass opened %d read %d, want 0", opens.Load(), reads.Load())
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	opens.Store(0)
	reads.Store(0)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if opens.Load() == 0 || reads.Load() == 0 {
		t.Fatalf("mtime change was not read (opens=%d reads=%d)", opens.Load(), reads.Load())
	}
	if n := countText(t, s, "stable marker"); n != 1 {
		t.Fatalf("stable marker indexed %d times", n)
	}
}

// TestCheapCheckMissesRestoredMtime pins the replacement the stat check
// cannot see: different bytes, unchanged inode, size and mtime restored.
func TestCheapCheckMissesRestoredMtime(t *testing.T) {
	project := t.TempDir()
	path := writeJSONL(t, project, "proj", "sess-blind", []map[string]any{
		msg("user", "original blind marker", "2026-04-01T10:00:00Z"),
	})
	stamp := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, project)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	swapped := []byte(string(raw))
	old := []byte("original")
	neu := []byte("swappedX")
	if len(old) != len(neu) || !bytes.Contains(swapped, old) {
		t.Fatalf("fixture has no same-length swap point")
	}
	for i := 0; i+len(old) <= len(swapped); i++ {
		if string(swapped[i:i+len(old)]) == string(old) {
			copy(swapped[i:], neu)
			break
		}
	}
	if err := os.WriteFile(path, swapped, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	opens, reads := countTranscriptIO(t)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 || reads.Load() != 0 {
		t.Fatalf("restored mtime was read (opens=%d reads=%d)", opens.Load(), reads.Load())
	}
	if n := countText(t, s, "swappedX blind marker"); n != 0 {
		t.Fatalf("same-size same-mtime swap was indexed %d times", n)
	}
	if n := countText(t, s, "original blind marker"); n != 1 {
		t.Fatalf("original marker count %d, want 1", n)
	}
}

func TestStartupResumesLegacyCursorInsideCompletedLine(t *testing.T) {
	project := t.TempDir()
	path := readerClaudeBatch.pathFor(t, project)
	first := readerClaudeBatch.line("legacy first line")
	finished := readerClaudeBatch.line("completed legacy appended line")
	writeLines(t, path, first)

	dbPath := filepath.Join(t.TempDir(), "legacy-scan.db")
	installTestRateCard(t, testRateCard())
	s, err := New(dbPath, project)
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	writeLines(t, path, first+finished)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The legacy reader consumed a partial line, then recorded file
	// metadata after the writer appended the remaining bytes.
	if _, err := s.writeDB.Exec(`
		UPDATE ingest_state
		SET offset = ?, recorded_size = ?, recorded_mtime = ?, prefix_sha256 = NULL
		WHERE path = ?`, len(first)+len(finished)/2, info.Size(), fingerprintMtime(info), path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = New(dbPath, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.AwaitStartup()
	for range 2 {
		if err := s.IngestAll(); err != nil {
			t.Fatal(err)
		}
	}
	expectOnce(t, s, "legacy first line")
	expectOnce(t, s, "completed legacy appended line")
	assertJSONLOffset(t, s, path, len(first)+len(finished))
}

func TestLegacyCursorBacktracksAcrossMoreThanOneBlock(t *testing.T) {
	project := t.TempDir()
	path := readerClaudeBatch.pathFor(t, project)
	first := readerClaudeBatch.line("before the long legacy record")
	const marker = "long legacy tail survived"
	long := readerClaudeBatch.line(marker + strings.Repeat("x", 16<<10))
	writeLines(t, path, first)
	s := newTestStore(t, project)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	writeLines(t, path, first+long)
	off := int64(len(first) + len(long)/2)
	if _, err := s.writeDB.Exec(`UPDATE ingest_state SET offset = ?, prefix_sha256 = NULL WHERE path = ?`, off, path); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.offsets[path] = off
	s.gens[path] = ingestGen{}
	s.mu.Unlock()
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, "before the long legacy record")
	expectOnce(t, s, marker)
	assertJSONLOffset(t, s, path, len(first)+len(long))
}

func TestLegacyCompleteEOFRecordIsNotReplayed(t *testing.T) {
	project := t.TempDir()
	path := readerClaudeBatch.pathFor(t, project)
	const old = `{"type":"user","timestamp":"2026-04-01T10:00:00Z","message":{"content":"legacy complete anonymous record"}}`
	const marker = "first line after the legacy complete record"
	writeLines(t, path, old)
	s := newTestStore(t, project)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`UPDATE ingest_state SET prefix_sha256 = NULL WHERE path = ?`, path); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.gens[path] = ingestGen{}
	s.mu.Unlock()
	writeLines(t, path, old+"\n"+readerClaudeBatch.line(marker))
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, "legacy complete anonymous record")
	expectOnce(t, s, marker)
}

func TestRestartAfterTruncationUsesReplacementPrefix(t *testing.T) {
	root := t.TempDir()
	path := readerClaudeBatch.pathFor(t, root)
	old := readerClaudeBatch.line("first record of original source") + readerClaudeBatch.line("second record of original source")
	const truncated = "first new record after truncation"
	const appended = "appended after restarting the store"
	writeLines(t, path, old)
	s := newTestStore(t, root)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	initial := readerClaudeBatch.line(truncated)
	writeLines(t, path, initial)
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
	writeLines(t, path, initial+readerClaudeBatch.line(appended))
	if err := reopened.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := reopened.readDB.QueryRow(`SELECT source_status FROM session_meta WHERE session_id = 'sess-jsonl'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if sourceStatusClass(status) != "truncated_at" {
		t.Fatalf("ordinary append after reopening changed the truncation tag to %q", status)
	}
	expectOnce(t, reopened, truncated)
	expectOnce(t, reopened, appended)
}

func TestReopenUsesPersistedPrefixWhenTranscriptWasRewrittenBeforeStartup(t *testing.T) {
	root := t.TempDir()
	path := readerClaudeBatch.pathFor(t, root)
	line := readerClaudeBatch.line
	// Equal widths put the stale cursor at a valid line boundary in
	// the replacement, so only the persisted prefix can detect drift.
	const old = "old prefix before restart"
	const rewritten = "new prefix before restart"
	const appended = "tail of grown replacement file"
	writeLines(t, path, line(old))
	s := newTestStore(t, root)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	writeLines(t, path, line(rewritten)+line(appended))
	reopened, err := New(s.DBPath(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	reopened.AwaitStartup()
	if err := reopened.IngestAll(); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{old, rewritten, appended} {
		expectOnce(t, reopened, marker)
	}
}

func TestStartupUsesMetadataFromBeforePrefixMigration(t *testing.T) {
	project := t.TempDir()
	path := writeJSONL(t, project, "proj", "sess-legacy", []map[string]any{
		msg("user", "legacy caught-up transcript", "2026-04-01T10:00:00Z"),
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "pre-prefix-scan.db")
	if err := upgradeSchemaWith(dbPath, previousSchemaSQL(t)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(writerDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO ingest_state (path, offset, recorded_size, recorded_mtime)
		VALUES (?, ?, ?, ?)`, path, info.Size(), info.Size(), fingerprintMtime(info))
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("seed legacy cursor: insert=%v close=%v", err, closeErr)
	}

	installTestRateCard(t, testRateCard())
	s, err := New(dbPath, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.AwaitStartup()
	opens, reads := countTranscriptIO(t)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 || reads.Load() != 0 {
		t.Fatalf("unchanged legacy transcript opened %d times and read %d times, want none", opens.Load(), reads.Load())
	}
}
