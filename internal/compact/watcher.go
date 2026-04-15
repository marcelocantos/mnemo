// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// WatcherConfig tunes the Watcher. Zero values mean "use defaults".
type WatcherConfig struct {
	// PollInterval is how often LiveSessions is queried to discover active
	// sessions. Default: 30s.
	PollInterval time.Duration
	// CompactInterval is how often each per-session goroutine attempts a
	// compaction. Default: 5 minutes.
	CompactInterval time.Duration
	// IdleTimeout is how long a session may be absent from LiveSessions
	// before its compaction goroutine is reaped. Default: 10 minutes.
	IdleTimeout time.Duration
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

func (c *WatcherConfig) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return 10 * time.Minute
}

// sessionSource is the narrow slice of the store the Watcher needs.
type sessionSource interface {
	// LiveSessions returns session ID → PID for sessions with open JSONL files.
	LiveSessions() map[string]int
	// SessionCWD returns the working directory recorded for the session, or ""
	// if not known. Used for self-exclusion.
	SessionCWD(sessionID string) string
}

// Watcher polls for active Claude Code sessions and drives periodic
// compactions via a Compactor. Each active session gets its own goroutine;
// sessions absent from LiveSessions for longer than IdleTimeout are reaped.
type Watcher struct {
	src        sessionSource
	compactor  *Compactor
	cfg        WatcherConfig
	excludeCWD string // path prefix that marks a mnemo/summariser session

	mu       sync.Mutex
	sessions map[string]*sessionWorker // session ID → worker
}

// NewWatcher creates a Watcher. excludeCWD is the path (or path prefix) used
// to identify self-sessions (summariser sessions driven by mnemo). Any session
// whose recorded cwd starts with excludeCWD is skipped.
func NewWatcher(src sessionSource, c *Compactor, cfg WatcherConfig, excludeCWD string) *Watcher {
	return &Watcher{
		src:        src,
		compactor:  c,
		cfg:        cfg,
		excludeCWD: excludeCWD,
		sessions:   make(map[string]*sessionWorker),
	}
}

// Run polls for live sessions until ctx is cancelled.
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

// ActiveCount returns the number of currently tracked session workers.
// Exposed for testing.
func (w *Watcher) ActiveCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sessions)
}

func (w *Watcher) poll(ctx context.Context) {
	live := w.src.LiveSessions()

	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()

	// Mark seen sessions, spawn new ones.
	for sid := range live {
		if w.shouldExclude(sid) {
			continue
		}
		if sw, ok := w.sessions[sid]; ok {
			sw.lastSeen = now
		} else {
			sw := newSessionWorker(sid, w.compactor, w.cfg)
			w.sessions[sid] = sw
			go sw.run(ctx)
		}
	}

	// Reap idle sessions.
	idleLimit := w.cfg.idleTimeout()
	for sid, sw := range w.sessions {
		if now.Sub(sw.lastSeen) > idleLimit {
			slog.Info("compact: reaping idle session", "session", sid)
			sw.cancel()
			delete(w.sessions, sid)
		}
	}
}

// shouldExclude returns true if the session should not be compacted.
// Must be called with w.mu held.
func (w *Watcher) shouldExclude(sessionID string) bool {
	if w.excludeCWD == "" {
		return false
	}
	cwd := w.src.SessionCWD(sessionID)
	if cwd == "" {
		return false
	}
	// Exclude sessions whose cwd is the mnemo repo (the summariser itself).
	return len(cwd) >= len(w.excludeCWD) && cwd[:len(w.excludeCWD)] == w.excludeCWD
}

// sessionWorker runs compaction for a single session.
type sessionWorker struct {
	sessionID    string
	compactor    *Compactor
	cfg          WatcherConfig
	lastSeen     time.Time
	cancel       context.CancelFunc
	budgetWarned bool
}

func newSessionWorker(sessionID string, c *Compactor, cfg WatcherConfig) *sessionWorker {
	return &sessionWorker{
		sessionID: sessionID,
		compactor: c,
		cfg:       cfg,
		lastSeen:  time.Now(),
	}
}

func (sw *sessionWorker) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	sw.cancel = cancel
	defer cancel()

	ticker := time.NewTicker(sw.cfg.compactInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := sw.compactor.Compact(ctx, sw.sessionID)
			switch {
			case err == nil, err == ErrNothingToCompact:
				// normal idle ticks
			case err == ErrBudgetExceeded:
				if !sw.budgetWarned {
					slog.Info("compact: session over budget, skipping further compactions", "session", sw.sessionID)
					sw.budgetWarned = true
				}
			default:
				slog.Warn("compact: compaction failed", "session", sw.sessionID, "err", err)
			}
		}
	}
}
