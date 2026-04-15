// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// fakeSessionSource implements sessionSource for tests.
type fakeSessionSource struct {
	mu       sync.Mutex
	sessions map[string]int // session ID → PID
	cwds     map[string]string
}

func newFakeSessionSource() *fakeSessionSource {
	return &fakeSessionSource{
		sessions: make(map[string]int),
		cwds:     make(map[string]string),
	}
}

func (f *fakeSessionSource) LiveSessions() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.sessions))
	for k, v := range f.sessions {
		out[k] = v
	}
	return out
}

func (f *fakeSessionSource) SessionCWD(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cwds[sessionID]
}

func (f *fakeSessionSource) setSession(id string, pid int, cwd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = pid
	if cwd != "" {
		f.cwds[id] = cwd
	}
}

func (f *fakeSessionSource) removeSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
}

// countingStore counts Compact calls per session.
type countingStore struct {
	fakeStore
	mu     sync.Mutex
	counts map[string]*atomic.Int64
}

func newCountingStore() *countingStore {
	return &countingStore{
		fakeStore: fakeStore{session: "any"},
		counts:    make(map[string]*atomic.Int64),
	}
}

func (c *countingStore) ReadSession(sessionID, role string, offset, limit int) ([]store.SessionMessage, error) {
	// Return a minimal message so Compact has something to compact.
	return []store.SessionMessage{
		{ID: 1, Role: "user", Text: "hello"},
	}, nil
}

func (c *countingStore) LatestCompaction(sessionID string) (*store.Compaction, error) {
	return nil, nil // always no prior compaction so Compact never returns ErrNothingToCompact
}

// stubNopLLM returns a valid empty payload without hitting the LLM.
type stubNopLLM struct {
	calls atomic.Int64
}

func (s *stubNopLLM) Call(ctx context.Context, sys, user string) (LLMResult, error) {
	s.calls.Add(1)
	return LLMResult{
		Text:  `{"targets":[],"decisions":[],"files":[],"open_threads":[],"summary":"nop"}`,
		Model: "stub",
	}, nil
}

// TestWatcherSpawnsWorkers checks that the watcher creates workers for new
// live sessions and that ActiveCount reflects them.
func TestWatcherSpawnsWorkers(t *testing.T) {
	src := newFakeSessionSource()
	src.setSession("sess-A", 100, "/work/some-project")
	src.setSession("sess-B", 101, "/work/other-project")

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 1 * time.Hour, // don't actually compact in this test
		IdleTimeout:     1 * time.Hour,
	}
	watcher := NewWatcher(src, compactor, cfg, "/mnemo-repo")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	// Wait for workers to spawn.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if watcher.ActiveCount() == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := watcher.ActiveCount(); got != 2 {
		t.Fatalf("expected 2 active workers, got %d", got)
	}
}

// TestWatcherSelfExclusion verifies that sessions with the excluded cwd are
// never spawned.
func TestWatcherSelfExclusion(t *testing.T) {
	src := newFakeSessionSource()
	src.setSession("sess-mnemo", 200, "/mnemo-repo")    // should be excluded
	src.setSession("sess-other", 201, "/other-project") // should be included

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 1 * time.Hour,
		IdleTimeout:     1 * time.Hour,
	}
	watcher := NewWatcher(src, compactor, cfg, "/mnemo-repo")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if watcher.ActiveCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := watcher.ActiveCount(); got != 1 {
		t.Fatalf("expected 1 active worker (self-excluded mnemo session), got %d", got)
	}
}

// TestWatcherIdleReap verifies that workers are removed after IdleTimeout.
func TestWatcherIdleReap(t *testing.T) {
	src := newFakeSessionSource()
	src.setSession("sess-idle", 300, "/some-project")

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 1 * time.Hour,
		IdleTimeout:     50 * time.Millisecond, // very short for test
	}
	watcher := NewWatcher(src, compactor, cfg, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	// Wait for worker to spawn.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if watcher.ActiveCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if watcher.ActiveCount() != 1 {
		t.Fatal("worker did not spawn")
	}

	// Remove session from live list so idle timeout triggers.
	src.removeSession("sess-idle")

	// Wait for reap.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if watcher.ActiveCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := watcher.ActiveCount(); got != 0 {
		t.Fatalf("expected 0 workers after reap, got %d", got)
	}
}

// TestWatcherNoDoubleSpawn verifies that polling twice for the same session
// does not create duplicate workers.
func TestWatcherNoDoubleSpawn(t *testing.T) {
	src := newFakeSessionSource()
	src.setSession("sess-once", 400, "/project")

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 1 * time.Hour,
		IdleTimeout:     1 * time.Hour,
	}
	watcher := NewWatcher(src, compactor, cfg, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	// Let several poll cycles run.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if got := watcher.ActiveCount(); got != 1 {
		t.Fatalf("expected exactly 1 worker for one session, got %d", got)
	}
}
