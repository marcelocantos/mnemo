// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store/fswatch"
)

// TestIngestAllIsolatesFileFailures is two bad files in one startup batch:
// one replaced between parse and cursor write, one whose message insert
// fails. Every other file is indexed. Each failure names its path. The
// watcher poll picks both up without depending on a new filesystem event.
func TestIngestAllIsolatesFileFailures(t *testing.T) {
	project := t.TempDir()
	goodA := writeJSONL(t, project, "proj", "sess-good-a", []map[string]any{
		msg("user", "good file alpha", "2026-04-01T10:00:00Z"),
	})
	goodB := writeJSONL(t, project, "proj", "sess-good-b", []map[string]any{
		msg("user", "good file beta", "2026-04-01T10:00:01Z"),
	})
	poison := writeJSONL(t, project, "proj", "sess-poison", []map[string]any{
		msg("user", "before poison", "2026-04-01T10:00:02Z"),
		msg("user", "POISONMARKER drop this file", "2026-04-01T10:00:03Z"),
	})
	replaced := writeJSONL(t, project, "proj", "sess-replaced", []map[string]any{
		msg("user", "old replace marker", "2026-04-01T10:00:04Z"),
	})
	replacement := []byte(`{"type":"user","timestamp":"2026-04-01T11:00:00Z","message":{"content":"new replace marker"}}` + "\n")

	s := newTestStore(t, project)
	if _, err := s.writeDB.Exec(`
		CREATE TRIGGER defect_batch_poison BEFORE INSERT ON messages
		WHEN new.text LIKE '%POISONMARKER%'
		BEGIN
			SELECT RAISE(ABORT, 'poison');
		END`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	testBeforeParsedWrite = func(pf parsedFile) {
		if pf.path != replaced {
			return
		}
		once.Do(func() {
			if err := os.WriteFile(replaced, replacement, 0o644); err != nil {
				t.Errorf("rewrite during write: %v", err)
			}
		})
	}
	t.Cleanup(func() { testBeforeParsedWrite = nil })

	err := s.IngestAll()
	if err == nil || !strings.Contains(err.Error(), poison) || !strings.Contains(err.Error(), replaced) {
		t.Fatalf("ingest error = %v, want both %s and %s", err, poison, replaced)
	}
	if n := countText(t, s, "good file alpha"); n != 1 {
		t.Fatalf("good alpha indexed %d times", n)
	}
	if n := countText(t, s, "good file beta"); n != 1 {
		t.Fatalf("good beta indexed %d times", n)
	}
	if n := countText(t, s, "POISONMARKER"); n != 0 {
		t.Fatalf("poison insert was committed (%d)", n)
	}
	if n := countText(t, s, "old replace marker"); n != 0 {
		t.Fatalf("replaced file's old parse was committed (%d)", n)
	}
	if n := countText(t, s, "new replace marker"); n != 0 {
		t.Fatalf("replacement was indexed on the failing pass (%d)", n)
	}
	for _, path := range []string{poison, replaced} {
		var n int
		if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM ingest_state WHERE path = ?`, path).Scan(&n); err != nil || n != 0 {
			t.Fatalf("cursor rows for %s = %d, err %v", path, n, err)
		}
	}
	for _, path := range []string{goodA, goodB} {
		var n int
		if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM ingest_state WHERE path = ?`, path).Scan(&n); err != nil || n != 1 {
			t.Fatalf("cursor rows for %s = %d, err %v", path, n, err)
		}
	}

	batchPoll := fswatch.NewPollTracker(time.Minute, 2)
	s.safetyPoll(batchPoll, s.ingestJSONLPath)
	if n := countText(t, s, "new replace marker"); n != 1 {
		t.Fatalf("replacement after safety poll indexed %d times, want 1", n)
	}
	if n := countText(t, s, "POISONMARKER"); n != 0 {
		t.Fatalf("poison inserted %d times while the trigger was active", n)
	}
	if _, err := s.writeDB.Exec(`DROP TRIGGER defect_batch_poison`); err != nil {
		t.Fatal(err)
	}
	s.safetyPoll(batchPoll, s.ingestJSONLPath)
	if n := countText(t, s, "POISONMARKER"); n != 1 {
		t.Fatalf("poison file after retry indexed %d times", n)
	}
	if n := countText(t, s, "before poison"); n != 1 {
		t.Fatalf("line before poison after retry indexed %d times", n)
	}
	if n := countText(t, s, "new replace marker"); n != 1 {
		t.Fatalf("replacement after retry indexed %d times", n)
	}
	if n := countText(t, s, "good file alpha"); n != 1 {
		t.Fatalf("good alpha after retry indexed %d times", n)
	}
	if n := countText(t, s, "good file beta"); n != 1 {
		t.Fatalf("good beta after retry indexed %d times", n)
	}
}

func TestBatchRollbackDoesNotHideAnotherFilesToolResult(t *testing.T) {
	project := t.TempDir()
	failed := writeJSONL(t, project, "proj", "sess-failed", []map[string]any{
		toolUseMsg("2026-04-01T10:00:00Z", "mnemo_search", "shared-tool-id", map[string]any{"query": "needle"}),
		msg("user", "fail this message", "2026-04-01T10:00:01Z"),
	})
	good := writeJSONL(t, project, "proj", "sess-good", []map[string]any{
		toolResultMsg("2026-04-01T10:00:02Z", "shared-tool-id", "retrievablebatchresult"),
	})
	s := newTestStore(t, project)
	if _, err := s.writeDB.Exec(`
		CREATE TRIGGER fail_batch_message BEFORE INSERT ON messages
		WHEN new.text = 'fail this message'
		BEGIN
			SELECT RAISE(ABORT, 'bad message');
		END`); err != nil {
		t.Fatal(err)
	}

	// Fix the input order so both files use the same writer state
	// and the failed tool_use is seen before the unrelated result.
	parsed := make(chan parsedFile, 2)
	for _, path := range []string{failed, good} {
		pf, err := parseFile(path, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed <- pf
	}
	close(parsed)
	if err := s.runWriter(parsed, len(parsed)); err == nil || !strings.Contains(err.Error(), failed) {
		t.Fatalf("writer error = %v, want failed path %s", err, failed)
	}
	if n := searchHits(t, s, "retrievablebatchresult"); n != 1 {
		t.Fatalf("tool result in the good file has %d search hits, want 1 after the failed tool_use is rolled back", n)
	}
}
