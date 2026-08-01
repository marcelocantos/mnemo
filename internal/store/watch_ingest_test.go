// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store/fswatch"
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

// TestSafetyPollRecoversSilentAppend proves the shipped safetyPoll path
// recovers open/write/close growth with NO tree events and NO NoteEvent
// (Grok-style updates.jsonl). Drives Store.safetyPoll + PollTracker only —
// does not start FSEvents (verification plan: poll path separate from events).
func TestSafetyPollRecoversSilentAppend(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "proj", "decoy-a", []map[string]any{
		msg("user", "decoy a seed text enough", "2026-08-01T11:00:00Z"),
	})
	writeJSONL(t, projectDir, "proj", "decoy-b", []map[string]any{
		msg("user", "decoy b seed text enough", "2026-08-01T11:00:00Z"),
	})
	path := writeJSONL(t, projectDir, "proj", "silent-session", []map[string]any{
		msg("user", "silent seed content text enough", "2026-08-01T11:00:00Z"),
	})
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Silent append: no Watch, no NoteEvent.
	appendJSONL(t, projectDir, "proj", "silent-session", []map[string]any{
		msg("user", "silentpollt142marker enough text", "2026-08-01T11:00:02Z"),
	})

	// maxPerTick=1 forces rotation across decoys before silent-session.
	poller := fswatch.NewPollTracker(time.Minute, 1)

	var mu sync.Mutex
	var onNeedCount int
	for tick := 0; tick < 20; tick++ {
		s.safetyPoll(poller, func(p string) {
			mu.Lock()
			onNeedCount++
			mu.Unlock()
			s.ingestJSONLPath(p)
		})
		hits, err := s.Search("silentpollt142marker", 5, "all", "", 0, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) > 0 {
			mu.Lock()
			n := onNeedCount
			mu.Unlock()
			if n == 0 {
				t.Fatal("search hit without safetyPoll onNeed (poll path not exercised)")
			}
			if !poller.Stated(path) {
				// Offsets may use a non-EvalSymlinks spelling; require some path stated.
				if poller.StatCallCount() == 0 {
					t.Fatal("poller never stated any path")
				}
			}
			return
		}
	}
	t.Fatalf("silent append not recovered by safetyPoll in 20 ticks; onNeed=%d stated=%v",
		onNeedCount, poller.Stated(path))
}
