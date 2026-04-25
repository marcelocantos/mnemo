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

func (w *connWorker) tick(ctx context.Context) {
	sessionID, err := w.src.CurrentSessionForConnection(w.connectionID)
	if err != nil {
		slog.Warn("compact: current session lookup failed",
			"conn", w.connectionID, "err", err)
		return
	}
	if sessionID == "" {
		return // proxy connected but hasn't resolved a session yet
	}
	if w.excludeCWD != "" {
		cwd := w.src.SessionCWD(sessionID)
		if cwd != "" && strings.HasPrefix(cwd, w.excludeCWD) {
			return // summariser itself — skip to avoid recursion
		}
	}
	w.lastSessionID = sessionID

	tc := w.loadTargetContext(sessionID)
	_, err = w.compactor.Compact(ctx, w.connectionID, sessionID, tc)
	switch {
	case err == nil, errors.Is(err, ErrNothingToCompact):
		// normal idle ticks
	case errors.Is(err, ErrBudgetExceeded):
		if !w.budgetWarned {
			slog.Info("compact: connection over budget, skipping further compactions",
				"conn", w.connectionID, "session", sessionID)
			w.budgetWarned = true
		}
	case errors.Is(err, exec.ErrNotFound):
		slog.Error("compact: claude subprocess spawn failed — executable not found in PATH",
			"err", err,
			"path", os.Getenv("PATH"),
			"conn", w.connectionID,
			"session", sessionID,
		)
	default:
		slog.Warn("compact: compaction failed",
			"conn", w.connectionID, "session", sessionID, "err", err)
	}
}
