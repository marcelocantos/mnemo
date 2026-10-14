// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func toolUseLine(session, toolUseID string) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"sessionId": session,
		"timestamp": "2026-04-01T10:00:05Z",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    toolUseID,
					"name":  "mnemo_search",
					"input": map[string]any{"query": "zqxcallertoken"},
				},
			},
		},
	}
}

func TestCallerDBMissReadsTranscript(t *testing.T) {
	proj := t.TempDir()
	s := newTestStore(t, proj)
	const toolUse = "toolu_scanlookup01"
	const fromEntry = "sess-from-entry01"
	// The filename stem is a different valid session id. The match must
	// report the sessionId on the entry, which is what Claude writes.
	writeJSONL(t, proj, "workbench", "file-stem-bbbbbbbb", []map[string]any{
		toolUseLine(fromEntry, toolUse),
	})

	got, err := s.SessionForToolUse(toolUse)
	if err != nil {
		t.Fatal(err)
	}
	if got != fromEntry {
		t.Fatalf("session = %q, want the entry sessionId %q", got, fromEntry)
	}

	// A second call is served from the cache, after the file is gone.
	path := filepath.Join(proj, "workbench", "file-stem-bbbbbbbb.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err = s.SessionForToolUse(toolUse)
	if err != nil {
		t.Fatal(err)
	}
	if got != fromEntry {
		t.Fatalf("cached session = %q, want %q", got, fromEntry)
	}
}

func TestCallerDBRetryBeatsTheScan(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const toolUse = "toolu_retrylookup01"
	const sess = "sess-retry-aaaa"
	t.Cleanup(func() { callerAfterDBMiss = nil })
	landed := false
	callerAfterDBMiss = func() {
		if landed {
			return
		}
		landed = true
		if _, err := s.writeDB.Exec(`INSERT INTO messages
			(session_id, project, role, text, timestamp, type, is_noise, content_type, tool_use_id)
			VALUES (?, 'workbench', 'assistant', 'lookup', '2026-01-01T00:00:00Z', 'assistant', 0, 'tool_use', ?)`,
			sess, toolUse); err != nil {
			t.Errorf("insert: %v", err)
		}
	}
	got, err := s.SessionForToolUse(toolUse)
	if err != nil {
		t.Fatal(err)
	}
	if got != sess {
		t.Fatalf("session = %q, want the row that landed on retry", sess)
	}
	if !landed {
		t.Fatal("retry hook did not run")
	}
}

func TestCallerTranscriptMissExcludesNothing(t *testing.T) {
	proj := t.TempDir()
	s := newTestStore(t, proj)
	const toolUse = "toolu_missinglookup1"
	path := writeJSONL(t, proj, "workbench", "sess-stale-file01", []map[string]any{
		toolUseLine("sess-stale-file01", toolUse),
	})
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	// A recent file that does not contain this tool-use id.
	writeJSONL(t, proj, "workbench", "sess-other-file01", []map[string]any{
		toolUseLine("sess-other-file01", "toolu_someotherid01"),
	})

	got, err := s.SessionForToolUse(toolUse)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("session = %q, want empty when the only copy is outside the mtime window", got)
	}
}

func TestCallerScanHonoursExtraProjectDir(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	extra := t.TempDir()
	s.SetExtraProjectDirs([]string{extra})
	const toolUse = "toolu_extradirlookup"
	const sess = "sess-extra-dir001"
	writeJSONL(t, extra, "workbench", sess, []map[string]any{
		toolUseLine(sess, toolUse),
	})
	got, err := s.SessionForToolUse(toolUse)
	if err != nil {
		t.Fatal(err)
	}
	if got != sess {
		t.Fatalf("session = %q, want the session under the extra project dir", sess)
	}
}

func TestCallerScanStopsAfterTheFileBound(t *testing.T) {
	proj := t.TempDir()
	s := newTestStore(t, proj)
	const toolUse = "toolu_boundlookup001"
	// Older than the decoys, still inside the window, and past the
	// file bound once eight newer transcripts exist.
	oldPath := writeJSONL(t, proj, "workbench", "sess-bound-target1", []map[string]any{
		toolUseLine("sess-bound-target1", toolUse),
	})
	base := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(oldPath, base, base); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < callerScanLimit; i++ {
		sid := "sess-bound-decoy" + string(byte('0'+i))
		p := writeJSONL(t, proj, "workbench", sid, []map[string]any{
			toolUseLine(sid, "toolu_decoy0000000"+string(byte('0'+i))),
		})
		ts := base.Add(time.Duration(i+1) * time.Second)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SessionForToolUse(toolUse)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("session = %q, want empty once newer files fill the scan bound", got)
	}
}
