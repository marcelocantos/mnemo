// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSourceDrift covers 🎯T68.6 detection: a deleted source .jsonl is
// reported as "deleted" and a truncated one as "truncated", while
// intact sources produce no drift.
func TestSourceDrift(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	writeJSONL(t, dir, "proj", "sess-del", []map[string]any{
		msg("user", "to be deleted", now.Format(time.RFC3339)),
	})
	writeJSONL(t, dir, "proj", "sess-trunc", []map[string]any{
		msg("user", "to be truncated one", now.Format(time.RFC3339)),
		msg("assistant", "to be truncated two", now.Add(time.Second).Format(time.RFC3339)),
	})
	writeJSONL(t, dir, "proj", "sess-keep", []map[string]any{
		msg("user", "intact", now.Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Intact corpus → no drift.
	if rep := s.SourceDrift(); rep.Deleted != 0 || rep.Truncated != 0 {
		t.Fatalf("expected no drift on intact corpus, got %+v", rep)
	}

	// Locate the two files under the project dir and mutate them.
	var delPath, truncPath string
	projDir := filepath.Join(dir, "proj")
	entries, _ := os.ReadDir(projDir)
	for _, e := range entries {
		p := filepath.Join(projDir, e.Name())
		switch {
		case strings.Contains(p, "sess-del"):
			delPath = p
		case strings.Contains(p, "sess-trunc"):
			truncPath = p
		}
	}
	if delPath == "" || truncPath == "" {
		t.Fatalf("could not locate ingested files in %s: %v", projDir, entries)
	}

	if err := os.Remove(delPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Truncate(truncPath, 1); err != nil { // shrink below the ingested offset
		t.Fatalf("truncate: %v", err)
	}

	rep := s.SourceDrift()
	if rep.Deleted != 1 {
		t.Errorf("expected 1 deleted, got %d (%+v)", rep.Deleted, rep)
	}
	if rep.Truncated != 1 {
		t.Errorf("expected 1 truncated, got %d (%+v)", rep.Truncated, rep)
	}
}
