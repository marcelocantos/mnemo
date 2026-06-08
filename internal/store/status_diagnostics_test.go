// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"strings"
	"testing"
	"time"
)

// cwdMsg is like msg but stamps the entry's cwd, so ingest populates
// session_meta.cwd (needed to exercise the repo→project-dir mapping).
func cwdMsg(typ, text, ts, cwd string) map[string]any {
	return map[string]any{
		"type":      typ,
		"timestamp": ts,
		"cwd":       cwd,
		"message":   map[string]any{"content": text},
	}
}

func appendToFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func sourceFor(diag *IngestDiagnostics, dir string) *TranscriptSource {
	for i := range diag.Sources {
		if diag.Sources[i].Path == dir {
			return &diag.Sources[i]
		}
	}
	return nil
}

// TestIngestDiagnosticsFreshCorpus: a fully-ingested recent corpus
// reports near-zero lag and no behind files.
func TestIngestDiagnosticsFreshCorpus(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeJSONL(t, dir, "p", "sess-fresh", []map[string]any{
		msg("user", "a recent question with enough text", now.Add(-2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "a recent answer with enough text", now.Add(-1*time.Minute).Format(time.RFC3339)),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	diag := s.IngestDiagnostics("")
	if diag.Freshness == nil {
		t.Fatalf("expected a freshness block")
	}
	if diag.Freshness.LagSeconds > 600 {
		t.Errorf("fresh corpus should have small lag, got %ds", diag.Freshness.LagSeconds)
	}
	src := sourceFor(diag, dir)
	if src == nil || !src.Exists {
		t.Fatalf("expected the project dir as an existing source, got %+v", diag.Sources)
	}
	if src.TotalFiles != 1 {
		t.Errorf("expected 1 transcript file, got %d", src.TotalFiles)
	}
	if src.BehindFiles != 0 || src.PendingBytes != 0 || len(src.Examples) != 0 {
		t.Errorf("fully-ingested corpus must have no behind files, got %+v", src)
	}
}

// TestIngestDiagnosticsAppendBehind: appending to an already-ingested
// transcript without re-ingesting surfaces it as append_behind with
// pending bytes.
func TestIngestDiagnosticsAppendBehind(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeJSONL(t, dir, "p", "sess-behind", []map[string]any{
		msg("user", "first turn question text here", now.Add(-3*time.Minute).Format(time.RFC3339)),
		msg("assistant", "first turn answer text here", now.Add(-2*time.Minute).Format(time.RFC3339)),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}
	// Append a new line on disk; do NOT re-ingest.
	appendToFile(t, path,
		`{"type":"user","timestamp":"`+now.Format(time.RFC3339)+`","message":{"content":"a brand new untracked line"}}`+"\n")

	diag := s.IngestDiagnostics("")
	src := sourceFor(diag, dir)
	if src == nil {
		t.Fatalf("expected the project dir as a source")
	}
	if src.BehindFiles != 1 || src.PendingBytes <= 0 {
		t.Fatalf("expected 1 behind file with pending bytes, got %+v", src)
	}
	if len(src.Examples) != 1 || src.Examples[0].State != "append_behind" {
		t.Fatalf("expected one append_behind example, got %+v", src.Examples)
	}
	ex := src.Examples[0]
	if ex.SessionID != "sess-behind" || ex.Offset <= 0 || ex.Size <= ex.Offset {
		t.Errorf("behind example lacks forensic detail: %+v", ex)
	}
}

// TestIngestDiagnosticsNewUnseenFile: a transcript that appeared after
// ingest (never seen) is reported as a new/unknown-offset source.
func TestIngestDiagnosticsNewUnseenFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeJSONL(t, dir, "p", "sess-seen", []map[string]any{
		msg("user", "an indexed question with text", now.Add(-2*time.Minute).Format(time.RFC3339)),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}
	// A brand-new file lands after ingest; the watcher hasn't picked it up.
	writeJSONL(t, dir, "p", "sess-unseen", []map[string]any{
		msg("user", "a freshly written unseen line", now.Format(time.RFC3339)),
	})

	diag := s.IngestDiagnostics("")
	src := sourceFor(diag, dir)
	if src == nil {
		t.Fatalf("expected the project dir as a source")
	}
	if src.UnknownOffset < 1 {
		t.Fatalf("expected at least one never-ingested file, got %+v", src)
	}
	var found bool
	for _, ex := range src.Examples {
		if ex.SessionID == "sess-unseen" && ex.State == "new" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sess-unseen reported with state=new, got %+v", src.Examples)
	}
}

// TestIngestDiagnosticsRepoFilter: a repo filter maps to the encoded
// Claude project dir of its sessions; appending after ingest makes the
// on-disk transcript newer than the index and the note says so. An
// unmatched filter says so explicitly.
func TestIngestDiagnosticsRepoFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	cwd := "/Users/marcelo/work/github.com/squz/multimaze2"
	encoded := encodeClaudeProjectDir(cwd) // -Users-marcelo-work-github-com-squz-multimaze2
	path := writeJSONL(t, dir, encoded, "sess-mm", []map[string]any{
		cwdMsg("user", "kick off the multimaze work", now.Add(-5*time.Minute).Format(time.RFC3339), cwd),
		cwdMsg("assistant", "working on multimaze now", now.Add(-4*time.Minute).Format(time.RFC3339), cwd),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}
	// Newer on-disk content than the index.
	appendToFile(t, path,
		`{"type":"user","timestamp":"`+now.Format(time.RFC3339)+`","cwd":"`+cwd+`","message":{"content":"gold tick sprite landed"}}`+"\n")

	diag := s.IngestDiagnostics("multimaze2")
	if diag.Repo == nil {
		t.Fatalf("expected a repo_diagnostic for the filter")
	}
	var matched bool
	for _, d := range diag.Repo.MatchedProjectDirs {
		if strings.HasSuffix(d, encoded) {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("repo filter should map to the encoded dir %q, got %+v", encoded, diag.Repo.MatchedProjectDirs)
	}
	if diag.Repo.LatestOnDiskMtime == "" || diag.Repo.LatestIndexed == "" {
		t.Errorf("expected both indexed and on-disk timestamps, got %+v", diag.Repo)
	}
	if !strings.Contains(diag.Repo.Note, "NEWER") {
		t.Errorf("expected the note to flag on-disk newer than index, got %q", diag.Repo.Note)
	}

	// A filter that maps to nothing is explicit, not silently global.
	miss := s.IngestDiagnostics("totally-unindexed-repo-xyz")
	if miss.Repo == nil || !strings.Contains(miss.Repo.Note, "no transcript source maps") {
		t.Errorf("expected an explicit no-source note, got %+v", miss.Repo)
	}
}

// TestStatusKeepsStreamsAndAddsDiagnostics: the additive 🎯T75 block
// does not displace the existing `streams` field.
func TestStatusKeepsStreamsAndAddsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeJSONL(t, dir, "p", "sess-x", []map[string]any{
		msg("user", "a question with sufficient text", now.Add(-2*time.Minute).Format(time.RFC3339)),
	})
	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}
	s.recordBackfillStatus("targets", 8, 10)

	res, err := s.Status(7, "", 2, 6, 160)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Diagnostics == nil {
		t.Errorf("Status must include the 🎯T75 diagnostics block")
	}
	var hasTargets bool
	for _, st := range res.Streams {
		if st.Stream == "targets" {
			hasTargets = true
		}
	}
	if !hasTargets {
		t.Errorf("Status must still expose the existing streams field, got %+v", res.Streams)
	}
}
