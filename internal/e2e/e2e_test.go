// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the cold read, filesystem watch and SQLite-backed query through
// an isolated daemon. Complete a partial record, then replace its source.
func TestTranscriptCompletesAcrossDaemonReads(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "fixture")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "split-record-journey.jsonl")
	const first = `{"type":"user","uuid":"first","timestamp":"2026-04-01T10:00:00Z","message":{"content":"first journey record"}}` + "\n"
	const partial = `{"type":"user","uuid":"second","timestamp":"2026-04-01T10:00:01Z","message":{"content":"completed journey`
	const rest = ` record"}}`
	const third = `{"type":"user","uuid":"third","timestamp":"2026-04-01T10:00:02Z","message":{"content":"third journey record"}}` + "\n"
	if err := os.WriteFile(path, []byte(first+partial), 0o644); err != nil {
		t.Fatal(err)
	}
	d := Start(t, Options{Home: home})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	query := func(sql string) (string, error) {
		out, err := d.Call(ctx, "mnemo_query", map[string]any{"query": sql})
		return strings.TrimSpace(out), err
	}
	await := func(sql string, ready func(string) bool) string {
		t.Helper()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			out, err := query(sql)
			if err == nil && ready(out) {
				return out
			}
			select {
			case <-ctx.Done():
				t.Fatalf("query did not reach expected state, last output %q, err %v\n%s", out, err, d.Log())
			case <-tick.C:
			}
		}
	}
	appendBytes := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString(s)
		if closeErr := f.Close(); writeErr != nil || closeErr != nil {
			t.Fatalf("append: write=%v close=%v", writeErr, closeErr)
		}
	}
	const cursor = `SELECT offset FROM ingest_state WHERE path LIKE '%split-record-journey.jsonl'`
	if got, want := await(cursor, func(s string) bool { return strings.HasPrefix(s, "offset:") }), fmt.Sprintf("offset: %d", len(first)); got != want {
		t.Fatalf("cursor after split write is %q, want %q", got, want)
	}
	const completed = `SELECT COUNT(*) AS hits FROM messages_v
		WHERE session_id = 'split-record-journey' AND text = 'completed journey record'`
	awaitOne := func(q string) { await(q, func(s string) bool { return s == "hits: 1" }) }
	appendBytes(rest)
	awaitOne(completed)
	appendBytes("\n" + third)
	awaitOne(`SELECT COUNT(*) AS hits FROM messages_v
		WHERE session_id = 'split-record-journey' AND text = 'third journey record'`)
	if got, err := query(completed); err != nil || got != "hits: 1" {
		t.Fatalf("completed record count after next append = %q, err = %v", got, err)
	}

	// The replacement contains the first record, drops the next two,
	// and introduces two byte-identical records without native IDs.
	const repeated = `{"type":"user","timestamp":"2026-04-01T10:00:03Z","message":{"content":"repeated journey record"}}` + "\n"
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte(first+repeated+repeated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	await(`SELECT COUNT(*) AS hits FROM messages_v
		WHERE session_id = 'split-record-journey' AND text = 'repeated journey record'`,
		func(s string) bool { return s == "hits: 2" })
	const total = `SELECT COUNT(*) AS hits FROM messages_v WHERE session_id = 'split-record-journey'`
	if got, err := query(total); err != nil || got != "hits: 5" {
		t.Fatalf("replacement left %q, want 5 messages, err = %v", got, err)
	}
	awaitOne(completed)
	awaitOne(`SELECT COUNT(*) AS hits FROM messages_v
		WHERE session_id = 'split-record-journey' AND text = 'third journey record'`)
	awaitOne(`SELECT COUNT(*) AS hits FROM session_meta
		WHERE session_id = 'split-record-journey'
		  AND (source_status LIKE 'rewritten_at=%' OR source_status LIKE 'truncated_at=%')`)
}

// TestSmoke launches a daemon with an empty MNEMO_HOME tempdir,
// confirms the MCP transport is reachable, and round-trips one tool
// call (mnemo_stats — a read-only call that works against an empty
// index). Proves the harness end-to-end. Skip on -short.
func TestSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e smoke test skipped under -short")
	}
	d := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := d.Call(ctx, "mnemo_stats", nil)
	if err != nil {
		t.Fatalf("mnemo_stats: %v\n--- daemon log ---\n%s", err, d.Log())
	}
	if out == "" {
		t.Fatal("mnemo_stats returned empty body")
	}
	// mnemo_stats response should mention "sessions" or "entries"
	// somewhere in its rendering. Loose check; we're verifying the
	// channel works, not the tool's specific output shape.
	if !strings.Contains(strings.ToLower(out), "session") &&
		!strings.Contains(strings.ToLower(out), "entries") {
		t.Errorf("mnemo_stats output unexpected:\n%s", out)
	}
}

// TestIsolation verifies the 🎯T73 keystone invariant: a daemon
// launched with MNEMO_HOME=<tempdir> writes its state under that
// tempdir, NOT under the real ~/.mnemo. Belt-and-braces against the
// "test accidentally hits live data" failure mode.
func TestIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e isolation test skipped under -short")
	}
	d := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Issue any tool call so the daemon definitely touches its
	// data root; mnemo_stats is cheap.
	if _, err := d.Call(ctx, "mnemo_stats", nil); err != nil {
		t.Fatalf("mnemo_stats: %v\n%s", err, d.Log())
	}
	d.Stop() // flush + close DB so the file is observable

	// The daemon's database must live under Home/.mnemo/mnemo.db.
	dbPath := filepath.Join(d.Home, ".mnemo", "mnemo.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected daemon DB at %q (under MNEMO_HOME): %v", dbPath, err)
	}
}
