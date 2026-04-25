// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/targets"
)

// WatcherConfig tunes the Watcher. Zero values mean "use defaults".
type WatcherConfig struct {
	// PollInterval is how often the daemon's open-connection list is
	// sampled to discover new or closed connections. Default: 30s.
	PollInterval time.Duration
	// CompactInterval is how often each per-connection goroutine
	// attempts a compaction. Default: 5 minutes.
	CompactInterval time.Duration
}

func (c *WatcherConfig) pollInterval() time.Duration {
	if c.PollInterval > 0 {
		return c.PollInterval
	}
	return 30 * time.Second
}

func (c *WatcherConfig) compactInterval() time.Duration {
	if c.CompactInterval > 0 {
		return c.CompactInterval
	}
	return 5 * time.Minute
}

// connSource is the narrow slice of the store the Watcher needs.
// Under the connection-identity model, connection_id is the anchor —
// the Watcher spawns one goroutine per live proxy connection and
// reaps it when the connection closes, rather than chasing session
// IDs around across /clear events.
type connSource interface {
	// OpenConnections returns currently-open proxy connections.
	OpenConnections() ([]store.DaemonConnection, error)
	// CurrentSessionForConnection returns the session_id most
	// recently observed on the connection, or "" if none has been
	// recorded yet (proxy connected but hasn't called a session-
	// resolving tool).
	CurrentSessionForConnection(connectionID string) (string, error)
	// SessionCWD returns the working directory recorded for a session,
	// used for self-exclusion of the summariser's own sessions.
	SessionCWD(sessionID string) string
}

// Watcher polls the daemon's open-connection list and drives periodic
// compactions via a Compactor. Each live connection gets its own
// goroutine; connections that disappear from OpenConnections (i.e.,
// the proxy has disconnected — Claude Code exited, ctrl-c, etc.) are
// reaped immediately.
type Watcher struct {
	src        connSource
	compactor  *Compactor
	cfg        WatcherConfig
	excludeCWD string // path prefix that marks a mnemo/summariser session

	mu      sync.Mutex
	workers map[string]*connWorker // connection_id → worker
}

// NewWatcher creates a Watcher. excludeCWD is the path (or path prefix)
// used to identify self-sessions (summariser sessions driven by mnemo).
// A connection whose current session's cwd starts with excludeCWD is
// skipped.
func NewWatcher(src connSource, c *Compactor, cfg WatcherConfig, excludeCWD string) *Watcher {
	return &Watcher{
		src:        src,
		compactor:  c,
		cfg:        cfg,
		excludeCWD: excludeCWD,
		workers:    make(map[string]*connWorker),
	}
}

// Run polls open connections until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.pollInterval())
	defer ticker.Stop()

	// Run once immediately, then on each tick.
	w.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

// ActiveCount returns the number of currently tracked per-connection
// workers. Exposed for testing.
func (w *Watcher) ActiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.workers)
}

func (w *Watcher) poll(ctx context.Context) {
	open, err := w.src.OpenConnections()
	if err != nil {
		slog.Warn("compact: poll open connections failed", "err", err)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	live := make(map[string]struct{}, len(open))
	for _, c := range open {
		live[c.ConnectionID] = struct{}{}
		if _, ok := w.workers[c.ConnectionID]; ok {
			continue
		}
		worker := newConnWorker(c.ConnectionID, w.src, w.compactor, w.cfg, w.excludeCWD)
		w.workers[c.ConnectionID] = worker
		go worker.run(ctx)
	}

	// Reap workers for connections no longer in the open list.
	for id, worker := range w.workers {
		if _, ok := live[id]; ok {
			continue
		}
		slog.Info("compact: reaping closed connection", "conn", id)
		worker.cancel()
		delete(w.workers, id)
	}
}

// connWorker runs compaction for a single live connection. On each
// tick it resolves the connection's current session_id and invokes
// the compactor on that session, tagging the resulting compaction
// with the connection_id.
type connWorker struct {
	connectionID  string
	src           connSource
	compactor     *Compactor
	cfg           WatcherConfig
	excludeCWD    string
	cancel        context.CancelFunc
	budgetWarned  bool
	targetsWarned bool
	lastSessionID string
}

func newConnWorker(connectionID string, src connSource, c *Compactor, cfg WatcherConfig, excludeCWD string) *connWorker {
	return &connWorker{
		connectionID: connectionID,
		src:          src,
		compactor:    c,
		cfg:          cfg,
		excludeCWD:   excludeCWD,
	}
}

func (w *connWorker) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	defer cancel()

	ticker := time.NewTicker(w.cfg.compactInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// loadTargetContext resolves the session's CWD and reads bullseye.yaml
// from that root if present. Failures are logged once and treated as
// "no graph available"; the compactor's nil-tolerant prompt builder
// then produces the pre-🎯T1.4 payload shape.
func (w *connWorker) loadTargetContext(sessionID string) *TargetContext {
	cwd := w.src.SessionCWD(sessionID)
	if cwd == "" {
		return nil
	}
	state, err := targets.LoadFromCWD(cwd)
	if err != nil {
		if !w.targetsWarned {
			slog.Warn("compact: target graph load failed",
				"conn", w.connectionID, "session", sessionID, "cwd", cwd, "err", err)
			w.targetsWarned = true
		}
		return nil
	}
	if state == nil {
		return nil
	}
	tc := &TargetContext{
		RepoRoot:    state.RepoRoot,
		FrontierIDs: append([]string(nil), state.FrontierIDs...),
	}
	for _, t := range state.Active {
		tc.Active = append(tc.Active, TargetSnapshot{
			ID: t.ID, Name: t.Name, Status: string(t.Status),
		})
	}
	for _, t := range state.Achieved {
		tc.Achieved = append(tc.Achieved, TargetSnapshot{
			ID: t.ID, Name: t.Name, Status: string(t.Status),
		})
	}
	return tc
}

// tickOutcome enumerates the distinct outcomes of a single watcher tick.
type tickOutcome string

const (
	outcomeCompacted        tickOutcome = "compacted"
	outcomeNothingToCompact tickOutcome = "nothing_to_compact"
	outcomeBudgetExceeded   tickOutcome = "budget_exceeded"
	outcomeFailed           tickOutcome = "failed"
	outcomeSkippedSelf      tickOutcome = "skipped_self"
	outcomeSkippedNoSession tickOutcome = "skipped_no_session"
)

func (w *connWorker) tick(ctx context.Context) {
	outcome, extra := w.runTick(ctx)

	level := slog.LevelDebug
	switch outcome {
	case outcomeCompacted:
		level = slog.LevelInfo
	case outcomeBudgetExceeded:
		level = slog.LevelInfo
	case outcomeFailed:
		level = slog.LevelWarn
	}

	args := []any{
		"connection_id", w.connectionID,
		"session_id", w.lastSessionID,
		"outcome", string(outcome),
	}
	args = append(args, extra...)
	slog.Log(ctx, level, "compact: tick", args...)
}

// runTick executes one compaction attempt for this connection and
// returns the outcome along with any extra log fields.
func (w *connWorker) runTick(ctx context.Context) (tickOutcome, []any) {
	sessionID, err := w.src.CurrentSessionForConnection(w.connectionID)
	if err != nil {
		slog.Warn("compact: current session lookup failed",
			"conn", w.connectionID, "err", err)
		return outcomeFailed, []any{"err", err.Error()}
	}
	if sessionID == "" {
		return outcomeSkippedNoSession, nil
	}
	if w.excludeCWD != "" {
		cwd := w.src.SessionCWD(sessionID)
		if cwd != "" && strings.HasPrefix(cwd, w.excludeCWD) {
			w.lastSessionID = sessionID
			return outcomeSkippedSelf, nil
		}
	}
	w.lastSessionID = sessionID

	tc := w.loadTargetContext(sessionID)
	comp, err := w.compactor.Compact(ctx, w.connectionID, sessionID, tc)
	switch {
	case err == nil:
		return outcomeCompacted, []any{
			"compaction_id", comp.ID,
			"entry_id_to", comp.EntryIDTo,
		}
	case errors.Is(err, ErrNothingToCompact):
		return outcomeNothingToCompact, nil
	case errors.Is(err, ErrBudgetExceeded):
		if !w.budgetWarned {
			w.budgetWarned = true
		}
		return outcomeBudgetExceeded, nil
	case errors.Is(err, exec.ErrNotFound):
		// Distinct ERROR-level log for spawn-not-found (🎯T37) — the
		// per-tick outcome line below at WARN is the routine
		// observability surface; this extra ERROR line carries the
		// resolved PATH so the failure is debuggable from one log
		// alone, even when the user isn't tailing for outcomes.
		slog.Error("compact: claude subprocess spawn failed — executable not found in PATH",
			"err", err,
			"path", os.Getenv("PATH"),
			"conn", w.connectionID,
			"session", sessionID,
		)
		return outcomeFailed, []any{"err", err.Error(), "path", os.Getenv("PATH")}
	default:
		return outcomeFailed, []any{"err", err.Error()}
	}
}
