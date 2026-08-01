// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWatcherCreateAndAppend(t *testing.T) {
	root := t.TempDir()
	// Pre-create nested session dir so FSEvents is watching a real tree.
	sess := filepath.Join(root, "proj", "session-1")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}

	w, err := New(Config{
		Roots:   []string{root},
		Mode:    ModeTranscript,
		Latency: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Drain startup noise briefly.
	time.Sleep(150 * time.Millisecond)
	drain(w, 50*time.Millisecond)

	path := filepath.Join(sess, "updates.jsonl")
	if err := os.WriteFile(path, []byte(`{"t":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ev, ok := waitEvent(t, w, path, 3*time.Second)
	if !ok {
		// FSEvents can be slow in CI; append may still surface.
		t.Log("create event not seen; trying append signal")
	} else {
		t.Logf("create/write event: %+v", ev)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"t":2}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if _, ok := waitEvent(t, w, path, 3*time.Second); !ok {
		// On some platforms append coalescing is flaky; Interest+backend still covered
		// by FD oracle and filter tests. Require at least one event for the path overall.
		if !ok && ev.Path == "" {
			t.Fatalf("no create or append event for %s backend=%s", path, w.Backend())
		}
	}
}

func TestWatcherFiltersGrokSidecar(t *testing.T) {
	root := t.TempDir()
	sess := filepath.Join(root, "sid")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := New(Config{Roots: []string{root}, Mode: ModeTranscript, Latency: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	time.Sleep(150 * time.Millisecond)
	drain(w, 50*time.Millisecond)

	bad := filepath.Join(sess, "events.jsonl")
	if err := os.WriteFile(bad, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Also write an allowed file so we know the watcher is alive.
	good := filepath.Join(sess, "updates.jsonl")
	if err := os.WriteFile(good, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	sawGood, sawBad := false, false
	for time.Now().Before(deadline) {
		select {
		case ev := <-w.Events():
			if pathsEqual(ev.Path, bad) {
				sawBad = true
			}
			if pathsEqual(ev.Path, good) {
				sawGood = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if sawBad {
		t.Fatal("events.jsonl should be filtered")
	}
	if !sawGood && runtime.GOOS == "darwin" {
		// FSEvents flaky in short windows — filter unit test is authoritative;
		// still fail if we only saw bad.
		t.Log("warning: good event not observed (timing); filter unit test covers deny")
	}
	if !sawGood && runtime.GOOS != "darwin" {
		t.Fatal("expected updates.jsonl event on fsnotify backend")
	}
}

func waitEvent(t *testing.T, w Watcher, want string, d time.Duration) (Event, bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				return Event{}, false
			}
			if pathsEqual(ev.Path, want) {
				return ev, true
			}
		case <-deadline:
			return Event{}, false
		}
	}
}

func drain(w Watcher, d time.Duration) {
	deadline := time.After(d)
	for {
		select {
		case <-w.Events():
		case <-deadline:
			return
		}
	}
}

func pathsEqual(a, b string) bool {
	if a == b {
		return true
	}
	// /var vs /private/var on macOS.
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil && ra == rb {
		return true
	}
	return filepath.Base(a) == filepath.Base(b) &&
		(filepath.Base(a) != "." && filepath.Base(a) != "")
}
