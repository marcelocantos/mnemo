// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// LiveSource reports which sessions are being written to right now.
// Satisfied by *store.Store via LiveSessions (🎯T9.5.1), which reads
// transcript file handles — Claude Code holds its JSONL open for the
// life of a session, so an open handle is authoritative liveness rather
// than a heuristic.
type LiveSource interface {
	LiveSessions() map[string]int
}

// Watcher follows every live session, running one Runner per session and
// retiring it when the session goes quiet.
type Watcher struct {
	Live  LiveSource
	Store SpanStore
	// NewSummariser builds a summariser for a session. Injected so the
	// watcher can be tested without spawning Claude processes, and so
	// the sweep in 🎯T132.4 can vary model and effort per run.
	NewSummariser func(sessionID string) Summariser
	Cfg           Config
	DripSize      int
	// Poll is how often the live set is re-read. LiveSessions is itself
	// TTL-cached, so this is cheap.
	Poll time.Duration
	// MaxConcurrent bounds how many sessions are watched at once. A
	// machine with a dozen live sessions should not spawn a dozen
	// Claude processes; the busiest sessions are the ones worth
	// following, and the batch tier covers the rest at close.
	MaxConcurrent int

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

const (
	defaultPoll          = 20 * time.Second
	defaultMaxConcurrent = 3
)

// Run follows live sessions until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	if w.Poll <= 0 {
		w.Poll = defaultPoll
	}
	if w.MaxConcurrent <= 0 {
		w.MaxConcurrent = defaultMaxConcurrent
	}
	w.running = map[string]context.CancelFunc{}
	defer w.stopAll()

	tick := time.NewTicker(w.Poll)
	defer tick.Stop()
	w.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.reconcile(ctx)
		}
	}
}

// reconcile starts watchers for newly-live sessions and stops those whose
// session has ended.
//
// Stopping on session end is not merely tidiness. The runner's spans are
// provisional, and the batch pass at session close is what redraws them
// with hindsight (🎯T132.3); leaving a watcher running against a dead
// session would keep paying a summariser to re-examine a transcript that
// can no longer change.
func (w *Watcher) reconcile(ctx context.Context) {
	live := w.Live.LiveSessions()

	w.mu.Lock()
	defer w.mu.Unlock()

	for id, cancel := range w.running {
		if _, still := live[id]; !still {
			cancel()
			delete(w.running, id)
			slog.Info("stream segmentation stopped: session ended", "session", id)
		}
	}

	for id := range live {
		if _, already := w.running[id]; already {
			continue
		}
		if len(w.running) >= w.MaxConcurrent {
			break
		}
		sctx, cancel := context.WithCancel(ctx)
		w.running[id] = cancel

		r := &Runner{
			SessionID: id,
			Store:     w.Store,
			Summ:      w.NewSummariser(id),
			Cfg:       w.Cfg,
			DripSize:  w.DripSize,
		}
		slog.Info("stream segmentation started", "session", id)
		go func(id string) {
			defer r.Summ.Close()
			if err := r.Run(sctx, 0); err != nil {
				slog.Warn("stream segmentation ended with error", "session", id, "err", err)
			}
		}(id)
	}
}

func (w *Watcher) stopAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, cancel := range w.running {
		cancel()
		delete(w.running, id)
	}
}
