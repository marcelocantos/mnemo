// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// fakeConnSource implements connSource for tests. Connections are
// tracked with their current session_id; removeConnection simulates a
// proxy disconnecting.
type fakeConnSource struct {
	mu    sync.Mutex
	conns map[string]*fakeConn // connection_id → conn
}

type fakeConn struct {
	pid            int
	currentSession string
	cwd            string
}

func newFakeConnSource() *fakeConnSource {
	return &fakeConnSource{conns: make(map[string]*fakeConn)}
}

func (f *fakeConnSource) OpenConnections() ([]store.DaemonConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.DaemonConnection, 0, len(f.conns))
	for id, c := range f.conns {
		out = append(out, store.DaemonConnection{ConnectionID: id, PID: c.pid})
	}
	return out, nil
}

func (f *fakeConnSource) CurrentSessionForConnection(connectionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.conns[connectionID]; ok {
		return c.currentSession, nil
	}
	return "", nil
}

func (f *fakeConnSource) SessionCWD(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		if c.currentSession == sessionID {
			return c.cwd
		}
	}
	return ""
}

func (f *fakeConnSource) setConnection(id string, pid int, sessionID, cwd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[id] = &fakeConn{pid: pid, currentSession: sessionID, cwd: cwd}
}

func (f *fakeConnSource) removeConnection(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conns, id)
}

// countingStore satisfies the compactor's storeBackend with enough
// behaviour to exercise a tick.
type countingStore struct {
	fakeStore
}

func newCountingStore() *countingStore {
	return &countingStore{fakeStore: fakeStore{session: "any"}}
}

func (c *countingStore) ReadSession(sessionID, role string, offset, limit int) ([]store.SessionMessage, error) {
	return []store.SessionMessage{{ID: 1, Role: "user", Text: "hello"}}, nil
}

func (c *countingStore) LatestCompaction(sessionID string) (*store.Compaction, error) {
	return nil, nil
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

// TestWatcherSpawnsWorkers checks that the watcher creates one worker
// per live proxy connection and that ActiveCount reflects them.
func TestWatcherSpawnsWorkers(t *testing.T) {
	src := newFakeConnSource()
	src.setConnection("conn-A", 100, "sess-A", "/work/some-project")
	src.setConnection("conn-B", 101, "sess-B", "/work/other-project")

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 1 * time.Hour, // don't actually compact in this test
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

// TestWatcherSelfExclusion verifies that a connection whose current
// session has the excluded cwd is tracked (worker spawned) but its
// tick is a no-op. Under the new model, self-exclusion happens at
// compact-time, not spawn-time — a connection's session can change
// over its lifetime, and we only know whether to skip at the moment
// of compaction.
func TestWatcherSelfExclusion(t *testing.T) {
	src := newFakeConnSource()
	src.setConnection("conn-mnemo", 200, "sess-mnemo", "/mnemo-repo") // self
	src.setConnection("conn-user", 201, "sess-user", "/other-project")

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 15 * time.Millisecond, // force ticks
	}
	watcher := NewWatcher(src, compactor, cfg, "/mnemo-repo")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	// Let several compact-ticks happen.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	// Both connections are tracked as workers (self-exclusion is at
	// tick time), but only conn-user's LLM calls should have fired.
	if got := watcher.ActiveCount(); got != 2 {
		t.Fatalf("expected 2 active workers (spawn happens regardless; exclusion is per-tick), got %d", got)
	}
	if llm.calls.Load() == 0 {
		t.Fatal("expected some LLM calls from the non-excluded connection")
	}
	// We cannot cheaply assert "exactly none for conn-mnemo" without
	// attributing calls to connections, but if self-exclusion is
	// broken the LLM count would be roughly double.
}

// TestWatcherIdleReap verifies that workers are removed when their
// connection disappears from OpenConnections (i.e. the proxy
// disconnected — Claude Code exit, ctrl-c, crash).
func TestWatcherIdleReap(t *testing.T) {
	src := newFakeConnSource()
	src.setConnection("conn-idle", 300, "sess-idle", "/some-project")

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 1 * time.Hour,
	}
	watcher := NewWatcher(src, compactor, cfg, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

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

	// Simulate the proxy disconnecting.
	src.removeConnection("conn-idle")

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

// TestWatcherNoDoubleSpawn verifies that polling twice for the same
// connection does not create duplicate workers.
func TestWatcherNoDoubleSpawn(t *testing.T) {
	src := newFakeConnSource()
	src.setConnection("conn-once", 400, "sess-once", "/project")

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 1 * time.Hour,
	}
	watcher := NewWatcher(src, compactor, cfg, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if got := watcher.ActiveCount(); got != 1 {
		t.Fatalf("expected exactly 1 worker for one connection, got %d", got)
	}
}

// captureSlog installs a text-handler logger that writes into a buffer
// for the duration of the test, returning the buffer and a restore
// function. All levels are enabled so DEBUG lines are captured.
func captureSlog() (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	return &buf, func() { slog.SetDefault(old) }
}

// makeWorker builds a connWorker wired to a fakeConnSource + compactor.
// The compactor uses a countingStore whose LLM is a stubNopLLM, giving
// one pre-seeded session message so the first Compact call succeeds.
func makeWorker(connID, sessionID, cwd, excludeCWD string) (*connWorker, *stubNopLLM) {
	src := newFakeConnSource()
	if sessionID != "" {
		src.setConnection(connID, 1, sessionID, cwd)
	} else {
		src.setConnection(connID, 1, "", "")
	}
	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	w := newConnWorker(connID, src, compactor, WatcherConfig{}, excludeCWD)
	return w, llm
}

// TestTickLogSkippedNoSession checks that a tick with no session emits a
// DEBUG "compact: tick" line with outcome=skipped_no_session.
func TestTickLogSkippedNoSession(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	w, _ := makeWorker("conn-pre", "", "", "")
	w.tick(context.Background())

	got := buf.String()
	if !strings.Contains(got, "compact: tick") {
		t.Errorf("expected 'compact: tick' in log, got: %s", got)
	}
	if !strings.Contains(got, string(outcomeSkippedNoSession)) {
		t.Errorf("expected outcome=%s in log, got: %s", outcomeSkippedNoSession, got)
	}
	// DEBUG lines should NOT contain level=INFO or level=WARN.
	if strings.Contains(got, "level=INFO") || strings.Contains(got, "level=WARN") {
		t.Errorf("skipped_no_session should be DEBUG, got: %s", got)
	}
}

// TestTickLogSkippedSelf checks that a self-session tick emits DEBUG with
// outcome=skipped_self.
func TestTickLogSkippedSelf(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	w, _ := makeWorker("conn-mnemo", "sess-mnemo", "/mnemo-repo/sub", "/mnemo-repo")
	w.tick(context.Background())

	got := buf.String()
	if !strings.Contains(got, string(outcomeSkippedSelf)) {
		t.Errorf("expected outcome=%s in log, got: %s", outcomeSkippedSelf, got)
	}
	if strings.Contains(got, "level=INFO") || strings.Contains(got, "level=WARN") {
		t.Errorf("skipped_self should be DEBUG, got: %s", got)
	}
}

// TestTickLogNothingToCompact checks that an idle tick (nothing new)
// emits DEBUG with outcome=nothing_to_compact.
func TestTickLogNothingToCompact(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	// Use fakeStore directly so LatestCompaction respects seeded data.
	// fakeStore has one message at ID=1; seeding a compaction covering
	// entry 1 makes filterNew return an empty slice → ErrNothingToCompact.
	fs := &fakeStore{
		session: "sess-idle",
		msgs:    []store.SessionMessage{{ID: 1, Role: "user", Text: "hi"}},
	}
	if err := insertSeed(fs, 1); err != nil {
		t.Fatal(err)
	}
	src := newFakeConnSource()
	src.setConnection("conn-idle", 1, "sess-idle", "/some-project")
	llm := &stubNopLLM{}
	compactor := New(fs, llm, Config{})
	w := newConnWorker("conn-idle", src, compactor, WatcherConfig{}, "")

	w.tick(context.Background())

	got := buf.String()
	if !strings.Contains(got, string(outcomeNothingToCompact)) {
		t.Errorf("expected outcome=%s in log, got: %s", outcomeNothingToCompact, got)
	}
	// nothing_to_compact is DEBUG, not INFO.
	if strings.Contains(got, "level=INFO") || strings.Contains(got, "level=WARN") {
		t.Errorf("nothing_to_compact should be DEBUG, got: %s", got)
	}
}

// TestTickLogCompacted checks that a successful compaction emits an INFO
// line with outcome=compacted, compaction_id, and entry_id_to.
func TestTickLogCompacted(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	w, _ := makeWorker("conn-ok", "sess-ok", "/some-project", "")
	w.tick(context.Background())

	got := buf.String()
	if !strings.Contains(got, string(outcomeCompacted)) {
		t.Errorf("expected outcome=%s in log, got: %s", outcomeCompacted, got)
	}
	if !strings.Contains(got, "compaction_id=") {
		t.Errorf("expected compaction_id field in log, got: %s", got)
	}
	if !strings.Contains(got, "entry_id_to=") {
		t.Errorf("expected entry_id_to field in log, got: %s", got)
	}
	if !strings.Contains(got, "level=INFO") {
		t.Errorf("compacted should be INFO, got: %s", got)
	}
}

// TestTickLogConnectionID checks that connection_id and session_id are
// always present in the tick log line.
func TestTickLogConnectionID(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	w, _ := makeWorker("conn-xyz", "sess-abc", "/project", "")
	w.tick(context.Background())

	got := buf.String()
	if !strings.Contains(got, "connection_id=conn-xyz") {
		t.Errorf("expected connection_id=conn-xyz in log, got: %s", got)
	}
	if !strings.Contains(got, "session_id=sess-abc") {
		t.Errorf("expected session_id=sess-abc in log, got: %s", got)
	}
}

// TestWatcherNoSessionYet verifies that a connection that has
// handshook but not yet resolved any session_id gets a worker spawned
// but its tick is a no-op.
func TestWatcherNoSessionYet(t *testing.T) {
	src := newFakeConnSource()
	src.setConnection("conn-pre", 500, "", "") // no session recorded yet

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})

	cfg := WatcherConfig{
		PollInterval:    10 * time.Millisecond,
		CompactInterval: 15 * time.Millisecond,
	}
	watcher := NewWatcher(src, compactor, cfg, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if watcher.ActiveCount() != 1 {
		t.Fatalf("expected 1 worker even for session-less connection, got %d", watcher.ActiveCount())
	}
	if llm.calls.Load() != 0 {
		t.Fatalf("no LLM calls should fire until a session is resolved, got %d", llm.calls.Load())
	}
}
