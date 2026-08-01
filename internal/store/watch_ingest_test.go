// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestWatchIngestsAppend drives the shipped Store.Watch path: start Watch,
// append an allowlisted transcript line, assert Search sees it (🎯T142).
func TestWatchIngestsAppend(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "proj", "watch-session-1", []map[string]any{
		msg("user", "hello watch seed text here enough", "2026-08-01T10:00:00Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Watch(ctx) }()
	time.Sleep(400 * time.Millisecond)

	// Use FTS-friendly tokens (hyphenated strings are split by the tokenizer).
	appendJSONL(t, projectDir, "proj", "watch-session-1", []map[string]any{
		msg("user", "hello watch append uniquet142marker enough", "2026-08-01T10:00:01Z"),
	})

	deadline := time.Now().Add(8 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		hits, err := s.Search("uniquet142marker", 5, "all", "", 0, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) > 0 {
			found = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Watch returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return after cancel")
	}
	if !found {
		t.Fatal("appended transcript line not ingested via Store.Watch")
	}
}

// TestWatchPollOrEventRecoversAppend proves append recovery on the shipped
// Watch loop (tree event and/or 5s safety poll).
func TestWatchPollOrEventRecoversAppend(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "proj", "poll-session", []map[string]any{
		msg("user", "poll seed content text enough", "2026-08-01T11:00:00Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Watch(ctx) }()
	time.Sleep(300 * time.Millisecond)

	appendJSONL(t, projectDir, "proj", "poll-session", []map[string]any{
		msg("user", "pollrecoveryt142marker enough text", "2026-08-01T11:00:02Z"),
	})

	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		hits, err := s.Search("pollrecoveryt142marker", 5, "all", "", 0, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) > 0 {
			cancel()
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	cancel()
	t.Fatal("append not recovered by event or safety poll within 12s")
}
