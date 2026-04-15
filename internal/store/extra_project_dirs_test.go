// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestExtraProjectDirs_Ingest verifies that JSONL transcripts from
// both the primary projectDir and a configured extra dir land in the
// same index and are jointly searchable. This is the core acceptance
// for 🎯T15 (cross-platform transcript ingest).
func TestExtraProjectDirs_Ingest(t *testing.T) {
	primary := t.TempDir()
	extra := t.TempDir()

	writeJSONL(t, primary, "mac-project", "sess-mac-001", []map[string]any{
		metaMsg("user", "macos session about kubernetes deployment",
			"2026-04-15T09:00:00Z",
			"/Users/dev/work/github.com/acme/cluster", "feat/deploy"),
		msg("assistant", "Here's the deploy manifest.", "2026-04-15T09:00:05Z"),
	})

	writeJSONL(t, extra, "win-project", "sess-win-001", []map[string]any{
		metaMsg("user", "windows vm session about powershell automation",
			"2026-04-15T10:00:00Z",
			"C:\\Users\\dev\\work\\github.com\\acme\\tools", "feat/ps-scripts"),
		msg("assistant", "PowerShell here.", "2026-04-15T10:00:05Z"),
	})

	s := newTestStore(t, primary)
	s.SetExtraProjectDirs([]string{extra})

	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Search should find the macOS session.
	macHits, err := s.Search("kubernetes", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSession(macHits, "sess-mac-001") {
		t.Errorf("primary-dir session not indexed: got %v", sessionIDs(macHits))
	}

	// Search should also find the Windows session.
	winHits, err := s.Search("powershell", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSession(winHits, "sess-win-001") {
		t.Errorf("extra-dir session not indexed: got %v", sessionIDs(winHits))
	}
}

// TestExtraProjectDirs_MissingIsSkipped verifies that a configured
// extra dir which does not exist (e.g. an unmounted SMB share) does
// not fail ingest — the primary dir still gets indexed.
func TestExtraProjectDirs_MissingIsSkipped(t *testing.T) {
	primary := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	writeJSONL(t, primary, "mac-project", "sess-mac-002", []map[string]any{
		metaMsg("user", "local session about unmounted vm",
			"2026-04-15T11:00:00Z",
			"/Users/dev/work/github.com/acme/thing", "main"),
		msg("assistant", "ok.", "2026-04-15T11:00:05Z"),
	})

	s := newTestStore(t, primary)
	s.SetExtraProjectDirs([]string{missing})

	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll with missing extra dir should not fail: %v", err)
	}

	hits, err := s.Search("unmounted", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSession(hits, "sess-mac-002") {
		t.Errorf("primary session not indexed when extra dir missing: got %v", sessionIDs(hits))
	}
}

// TestLoadConfig_ExtraProjectDirs verifies JSON round-trip of the
// extra_project_dirs field.
func TestLoadConfig_ExtraProjectDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	payload := map[string]any{
		"workspace_roots":    []string{"/home/me/work"},
		"extra_project_dirs": []string{"/mnt/winc/Users/me/.claude/projects"},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom: %v", err)
	}
	if got, want := len(cfg.ExtraProjectDirs), 1; got != want {
		t.Fatalf("ExtraProjectDirs length: got %d, want %d", got, want)
	}
	if got, want := cfg.ExtraProjectDirs[0], "/mnt/winc/Users/me/.claude/projects"; got != want {
		t.Errorf("ExtraProjectDirs[0]: got %q, want %q", got, want)
	}
}

func containsSession(results []SearchResult, sessionID string) bool {
	for _, r := range results {
		if r.SessionID == sessionID {
			return true
		}
	}
	return false
}

func sessionIDs(results []SearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.SessionID)
	}
	return ids
}
