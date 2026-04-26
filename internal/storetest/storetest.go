// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package storetest provides shared test helpers for packages that
// need to spin up a populated mnemo store from synthetic JSONL
// fixtures. Tests inside the store package use sibling unexported
// helpers; this package mirrors them with exported names so packages
// like internal/reviewer can drive the same fixture path without
// taking a dep cycle through the store's own _test.go files.
package storetest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// WriteJSONL drops a fixture transcript file under
// dir/<project>/<sessionID>.jsonl, one map per JSONL line.
func WriteJSONL(t *testing.T, dir, project, sessionID string, entries []map[string]any) string {
	t.Helper()
	projDir := filepath.Join(dir, project)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projDir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Msg returns the minimal Claude Code transcript entry shape — type,
// timestamp, and a single text content block.
func Msg(typ, text, ts string) map[string]any {
	return map[string]any{
		"type":      typ,
		"timestamp": ts,
		"message":   map[string]any{"content": text},
	}
}

// MetaMsg is Msg with cwd + branch metadata so the ingest path can
// derive a session_meta row (and therefore a repo identifier).
func MetaMsg(typ, text, ts, cwd, branch string) map[string]any {
	m := Msg(typ, text, ts)
	if cwd != "" {
		m["cwd"] = cwd
	}
	if branch != "" {
		m["gitBranch"] = branch
	}
	return m
}

// NewStore opens a fresh store backed by a temp directory and the
// supplied projectDir for transcript scanning.
func NewStore(t *testing.T, projectDir string) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.New(dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
