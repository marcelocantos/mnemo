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

// fakeStoreSource implements storeSource for tests. Candidate lists
// are explicit; cwd lookups are taken from the candidates' CWD
// field (good enough for the watcher's loadTargetContext path —
// the tests don't exercise target graph loading).
type fakeStoreSource struct {
	mu         sync.Mutex
	candidates []store.CompactionCandidate
}

func (f *fakeStoreSource) SelectCompactionCandidates(
	minDeltaMsgs int,
	idleCutoff time.Time,
	maxBudgetRatio float64,
	limit int,
) ([]store.CompactionCandidate, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.CompactionCandidate, len(f.candidates))
	copy(out, f.candidates)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, len(f.candidates), nil
}

func (f *fakeStoreSource) SessionCWD(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.candidates {
		if c.SessionID == sessionID {
			return c.CWD
		}
	}
	return ""
}

func (f *fakeStoreSource) setCandidates(cs ...store.CompactionCandidate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candidates = append(f.candidates[:0], cs...)
}

// countingStore satisfies the compactor's storeBackend with enough
// behaviour to exercise a tick. ReadSession yields one user message;
// LatestCompaction returns nil so every Compact call covers new
// ground.
type countingStore struct {
	fakeStore
}

func newCountingStore() *countingStore {
	return &countingStore{fakeStore: fakeStore{session: "any"}}
}

func (c *countingStore) ReadSessionAfter(sessionID string, afterID int64, limit int) ([]store.SessionMessage, error) {
	// Always yield one fresh message past the cursor so every tick
	// produces a compaction (the watcher tests assert on compaction
	// counts, not on convergence).
	return []store.SessionMessage{{ID: int(afterID) + 1, Role: "user", Text: "hello"}}, nil
}

func (c *countingStore) LatestCompaction(sessionID string) (*store.Compaction, error) {
	return nil, nil
}

// stubNopLLM returns a valid empty payload without hitting the LLM.
// Records the (connection_id) tag of every compaction it ends up
// producing — tests assert on these to verify attribution.
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

// captureSlog installs a text-handler logger that writes into a
// buffer for the duration of the test, returning the buffer and a
// restore function. All levels are enabled so DEBUG lines land.
func captureSlog() (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	return &buf, func() { slog.SetDefault(old) }
}

// runUntil starts the watcher, polls `until` until it returns true
// (or a hard deadline elapses), and then stops it cleanly. Use this
// to wait for a specific observable effect — LLM calls, compactions
// written, log lines — rather than racing on "scan started".
func runUntil(t *testing.T, w *Watcher, until func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if until() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// TestWatcherScansAndCompactsSession verifies the activity-driven
// watcher selects sessions from the store and drives a compaction
// for each, with no daemon_connections involvement.
func TestWatcherScansAndCompactsSession(t *testing.T) {
	src := &fakeStoreSource{}
	src.setCandidates(
		store.CompactionCandidate{SessionID: "sess-A", CWD: "/work/proj-a"},
		store.CompactionCandidate{SessionID: "sess-B", CWD: "/work/proj-b"},
	)

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "/mnemo-repo")

	runUntil(t, w, func() bool { return llm.calls.Load() >= 2 })

	if got := llm.calls.Load(); got < 2 {
		t.Fatalf("expected at least 2 LLM calls (one per candidate), got %d", got)
	}
	if got := w.LastScanCount(); got != 2 {
		t.Fatalf("expected LastScanCount=2, got %d", got)
	}
}

// TestWatcherPerScanCap verifies 🎯T68.1's throughput bound: a single
// scan performs at most MaxCompactionsPerScan real compactions even
// when more sessions are owed, and the backlog is surfaced via
// Health(). The remainder is left for subsequent scans (here the scan
// interval is long enough that only the first scan fires during the
// test), so a large historical backlog never floods the LLM in one
// pass.
func TestWatcherPerScanCap(t *testing.T) {
	src := &fakeStoreSource{}
	src.setCandidates(
		store.CompactionCandidate{SessionID: "s1", CWD: "/work/a"},
		store.CompactionCandidate{SessionID: "s2", CWD: "/work/b"},
		store.CompactionCandidate{SessionID: "s3", CWD: "/work/c"},
		store.CompactionCandidate{SessionID: "s4", CWD: "/work/d"},
		store.CompactionCandidate{SessionID: "s5", CWD: "/work/e"},
	)

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	// Long scan interval: only the immediate first scan runs in-test.
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval:          10 * time.Second,
		MaxCompactionsPerScan: 2,
	}, "")

	runUntil(t, w, func() bool { return llm.calls.Load() >= 2 })

	if got := llm.calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 compactions (per-scan cap), got %d", got)
	}
	hs := w.Health()
	if hs.Backlog != 5 {
		t.Errorf("expected Backlog=5 (all owed sessions), got %d", hs.Backlog)
	}
	if hs.MaxCompactionsPerScan != 2 {
		t.Errorf("expected MaxCompactionsPerScan=2 in health, got %d", hs.MaxCompactionsPerScan)
	}
}

// TestWatcherUnboundSessionCompacts verifies that a candidate with
// no ConnectionID still produces a compaction — the core 🎯T59 fix.
// The compaction row stores NULL/empty for connection_id while the
// summariser output lands normally.
func TestWatcherUnboundSessionCompacts(t *testing.T) {
	src := &fakeStoreSource{}
	src.setCandidates(store.CompactionCandidate{
		SessionID: "sess-unbound", CWD: "/work/unbound", ConnectionID: "",
	})

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "")

	runUntil(t, w, func() bool { return llm.calls.Load() >= 1 })

	if llm.calls.Load() == 0 {
		t.Fatal("expected LLM call for unbound session, got 0")
	}
	if len(cs.compacts) == 0 {
		t.Fatal("expected a compaction to be written, got none")
	}
	if cs.compacts[0].ConnectionID != "" {
		t.Errorf("expected empty ConnectionID on unbound compaction, got %q", cs.compacts[0].ConnectionID)
	}
}

// TestWatcherBoundSessionTagsConnection verifies that when a
// candidate carries a non-empty ConnectionID, the resulting
// compaction is tagged with it (best-effort attribution).
func TestWatcherBoundSessionTagsConnection(t *testing.T) {
	src := &fakeStoreSource{}
	src.setCandidates(store.CompactionCandidate{
		SessionID: "sess-bound", CWD: "/work/proj", ConnectionID: "conn-xyz",
	})

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "")

	runUntil(t, w, func() bool { return llm.calls.Load() >= 1 })

	if len(cs.compacts) == 0 {
		t.Fatal("expected a compaction, got none")
	}
	if cs.compacts[0].ConnectionID != "conn-xyz" {
		t.Errorf("expected ConnectionID=conn-xyz, got %q", cs.compacts[0].ConnectionID)
	}
}

// TestWatcherSkipsSelfSession verifies that a candidate whose CWD
// starts with excludeCWD is skipped at scan time (the LLM is never
// called for self-sessions).
func TestWatcherSkipsSelfSession(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	src := &fakeStoreSource{}
	src.setCandidates(
		store.CompactionCandidate{SessionID: "sess-self", CWD: "/mnemo-repo/sub"},
		store.CompactionCandidate{SessionID: "sess-user", CWD: "/work/proj"},
	)

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "/mnemo-repo")

	runUntil(t, w, func() bool {
		return llm.calls.Load() >= 1 && strings.Contains(buf.String(), string(outcomeSkippedSelf))
	})

	if llm.calls.Load() != 1 {
		t.Errorf("expected exactly 1 LLM call (self skipped, user compacted), got %d", llm.calls.Load())
	}
	got := buf.String()
	if !strings.Contains(got, string(outcomeSkippedSelf)) {
		t.Errorf("expected outcome=%s in log for self-session, got: %s", outcomeSkippedSelf, got)
	}
}

// TestTickLogCompacted verifies a successful compaction emits an
// INFO line with outcome=compacted and the resolved IDs.
func TestTickLogCompacted(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	src := &fakeStoreSource{}
	src.setCandidates(store.CompactionCandidate{
		SessionID: "sess-ok", CWD: "/work/proj", ConnectionID: "conn-1",
	})

	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "")

	runUntil(t, w, func() bool { return strings.Contains(buf.String(), string(outcomeCompacted)) })

	got := buf.String()
	if !strings.Contains(got, string(outcomeCompacted)) {
		t.Errorf("expected outcome=%s in log, got: %s", outcomeCompacted, got)
	}
	if !strings.Contains(got, "compaction_id=") {
		t.Errorf("expected compaction_id field in log, got: %s", got)
	}
	if !strings.Contains(got, "level=INFO") {
		t.Errorf("compacted should be INFO, got: %s", got)
	}
}

// TestTickLogBudgetExceededIsDebug verifies 🎯T67's log-level
// demotion: budget_exceeded ticks log at DEBUG, not INFO, so the
// default daemon log no longer accumulates thousands of
// budget_exceeded INFO lines per day.
func TestTickLogBudgetExceededIsDebug(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	// fakeStore configured so checkBudget returns ErrBudgetExceeded
	// on the very first call (compTokens=100, sessTokens=100, ratio
	// 1.0 >> 0.10).
	// Seed: session tokens 100 in / 0 out, plus a prior compaction
	// of 80 + 20 = 100 tokens. The ratio (100/100 = 1.0) exceeds
	// the default 0.10 budget, so the next Compact call returns
	// ErrBudgetExceeded.
	fs := &fakeStore{
		session:   "sess-broke",
		msgs:      []store.SessionMessage{{ID: 1, Role: "user", Text: "hi"}},
		sessionIn: 100,
	}
	if _, err := fs.PutCompaction(store.Compaction{
		SessionID:    "sess-broke",
		PromptTokens: 80,
		OutputTokens: 20,
	}); err != nil {
		t.Fatal(err)
	}
	src := &fakeStoreSource{}
	src.setCandidates(store.CompactionCandidate{
		SessionID: "sess-broke", CWD: "/work/proj",
	})
	llm := &stubNopLLM{}
	compactor := New(fs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "")

	runUntil(t, w, func() bool { return strings.Contains(buf.String(), string(outcomeBudgetExceeded)) })

	got := buf.String()
	if !strings.Contains(got, string(outcomeBudgetExceeded)) {
		t.Errorf("expected outcome=%s in log, got: %s", outcomeBudgetExceeded, got)
	}
	// The pre-T67 implementation logged this at INFO. The 🎯T67 fix
	// demotes it to DEBUG; the only INFO lines we should see are from
	// the test harness itself (vault, etc.) — there should be no
	// compact: tick INFO entry for budget_exceeded.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "compact: tick") &&
			strings.Contains(line, string(outcomeBudgetExceeded)) &&
			strings.Contains(line, "level=INFO") {
			t.Errorf("budget_exceeded should be DEBUG, got INFO line: %s", line)
		}
	}
}

// TestWatcherHealthSnapshot verifies the runtime introspection
// surface added in 🎯T67: Health() reflects scan/tick activity and
// the lifetime outcome counters increment correctly.
func TestWatcherHealthSnapshot(t *testing.T) {
	src := &fakeStoreSource{}
	src.setCandidates(store.CompactionCandidate{
		SessionID: "sess-ok", CWD: "/work/proj",
	})
	llm := &stubNopLLM{}
	cs := newCountingStore()
	compactor := New(cs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "")

	runUntil(t, w, func() bool { return llm.calls.Load() >= 1 })

	hs := w.Health()
	if hs.LastScanAt.IsZero() {
		t.Errorf("expected LastScanAt to be set after at least one scan")
	}
	if hs.LastTickAt.IsZero() {
		t.Errorf("expected LastTickAt to be set after at least one tick")
	}
	if hs.LastTickOutcome != string(outcomeCompacted) {
		t.Errorf("expected LastTickOutcome=%s, got %q", outcomeCompacted, hs.LastTickOutcome)
	}
	if hs.Counts[string(outcomeCompacted)] < 1 {
		t.Errorf("expected compacted count >= 1, got %d", hs.Counts[string(outcomeCompacted)])
	}
	if hs.ScanInterval == 0 {
		t.Errorf("expected ScanInterval to be reported")
	}
	if hs.MinDeltaMessages == 0 {
		t.Errorf("expected MinDeltaMessages default to be reported")
	}
}

// TestTickLogNothingToCompact verifies that an idle tick (no new
// messages past the latest compaction) emits DEBUG with
// outcome=nothing_to_compact.
func TestTickLogNothingToCompact(t *testing.T) {
	buf, restore := captureSlog()
	defer restore()

	// Seed a compaction covering the only available message so
	// filterNew returns empty → ErrNothingToCompact.
	fs := &fakeStore{
		session: "sess-idle",
		msgs:    []store.SessionMessage{{ID: 1, Role: "user", Text: "hi"}},
	}
	if err := insertSeed(fs, 1); err != nil {
		t.Fatal(err)
	}
	src := &fakeStoreSource{}
	src.setCandidates(store.CompactionCandidate{
		SessionID: "sess-idle", CWD: "/some-project",
	})
	llm := &stubNopLLM{}
	compactor := New(fs, llm, Config{})
	w := NewWatcher(src, compactor, WatcherConfig{
		ScanInterval: 10 * time.Millisecond,
	}, "")

	runUntil(t, w, func() bool { return strings.Contains(buf.String(), string(outcomeNothingToCompact)) })

	got := buf.String()
	if !strings.Contains(got, string(outcomeNothingToCompact)) {
		t.Errorf("expected outcome=%s in log, got: %s", outcomeNothingToCompact, got)
	}
	// The per-tick "nothing_to_compact" outcome must stay at DEBUG so
	// idle ticks don't spam the log. The per-scan elapsed line (🎯T70.4)
	// runs at INFO regardless of tick outcomes — only tick lines are
	// scrutinised here.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.Contains(line, `msg="compact: tick"`) {
			continue
		}
		if strings.Contains(line, "level=INFO") || strings.Contains(line, "level=WARN") {
			t.Errorf("nothing_to_compact tick should be DEBUG, got: %s", line)
		}
	}
}
