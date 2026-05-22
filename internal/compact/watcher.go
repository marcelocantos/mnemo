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

// WatcherConfig tunes the activity-driven Watcher (🎯T59). Zero
// values mean "use defaults".
type WatcherConfig struct {
	// ScanInterval is how often the watcher scans session_summary
	// for compaction candidates. Default: 1 minute.
	ScanInterval time.Duration

	// MinMessages is the minimum substantive_msgs for a session to
	// be considered. Sessions below this never get a tick.
	// Default: 50.
	MinMessages int

	// RecencyWindow caps how stale a session's last_msg may be and
	// still be considered. Sessions whose latest message is older
	// than (now - RecencyWindow) are skipped. Default: 4 hours.
	RecencyWindow time.Duration

	// MaxCandidates caps the number of sessions returned by one
	// scan, sorted most-recently-active first. Default: 100.
	MaxCandidates int
}

func (c *WatcherConfig) scanInterval() time.Duration {
	if c.ScanInterval > 0 {
		return c.ScanInterval
	}
	return time.Minute
}

func (c *WatcherConfig) minMessages() int {
	if c.MinMessages > 0 {
		return c.MinMessages
	}
	return 50
}

func (c *WatcherConfig) recencyWindow() time.Duration {
	if c.RecencyWindow > 0 {
		return c.RecencyWindow
	}
	return 4 * time.Hour
}

func (c *WatcherConfig) maxCandidates() int {
	if c.MaxCandidates > 0 {
		return c.MaxCandidates
	}
	return 100
}

// storeSource is the narrow slice of the store the Watcher needs.
// Under the activity-driven model (🎯T59), candidate selection
// scans session_summary directly; daemon_connections is consulted
// only for best-effort connection_id attribution and is exposed
// via the CompactionCandidate already filled by the store.
type storeSource interface {
	// SelectCompactionCandidates returns sessions worth a tick:
	// substantive_msgs ≥ minMsgs AND last_msg > recencyCutoff,
	// capped at limit rows, most-recently-active first. Each
	// candidate carries the session's CWD and best-effort
	// ConnectionID (empty if no binding exists).
	SelectCompactionCandidates(minMsgs int, recencyCutoff time.Time, limit int) ([]store.CompactionCandidate, error)
	// SessionCWD returns the cwd recorded for a session, used by
	// loadTargetContext to find a bullseye.yaml alongside the
	// session's working tree. Empty if unknown.
	SessionCWD(sessionID string) string
}

// Watcher periodically scans session activity and drives compactions
// for sessions whose substantive_msgs cleared the configured
// threshold within the recency window. Replaces the per-connection
// worker model (pre-🎯T59) which only compacted sessions that had
// called mnemo_self — a circular dependency that in practice
// produced zero compactions over the entire month of May.
//
// Each scan iterates candidates serially. compactor.Compact is
// cheap when there is nothing new (ErrNothingToCompact) or when
// the per-session token budget has been hit (ErrBudgetExceeded),
// so re-ticking already-summarised sessions on later scans is
// bounded. Serial scan also means a session is never re-entered
// concurrently — the implicit concurrency guard.
type Watcher struct {
	src        storeSource
	compactor  *Compactor
	cfg        WatcherConfig
	excludeCWD string

	mu    sync.Mutex
	lastN int // candidates returned by the last scan (for tests)
}

// NewWatcher builds an activity-driven watcher. excludeCWD is the
// path prefix that marks self-sessions (a mnemo summariser session
// running against mnemo's own source tree); matching candidates are
// skipped at scan time so the compactor never recursively summarises
// itself.
func NewWatcher(src storeSource, c *Compactor, cfg WatcherConfig, excludeCWD string) *Watcher {
	return &Watcher{
		src:        src,
		compactor:  c,
		cfg:        cfg,
		excludeCWD: excludeCWD,
	}
}

// Run scans session activity until ctx is cancelled. The first scan
// happens immediately; subsequent scans fire on ScanInterval.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.scanInterval())
	defer ticker.Stop()

	w.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scan(ctx)
		}
	}
}

// LastScanCount returns the number of candidates returned by the
// most recent scan. Exposed for tests.
func (w *Watcher) LastScanCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastN
}

func (w *Watcher) scan(ctx context.Context) {
	now := time.Now()
	cutoff := now.Add(-w.cfg.recencyWindow())
	cands, err := w.src.SelectCompactionCandidates(
		w.cfg.minMessages(), cutoff, w.cfg.maxCandidates())
	if err != nil {
		slog.Warn("compact: scan candidates failed", "err", err)
		return
	}
	w.mu.Lock()
	w.lastN = len(cands)
	w.mu.Unlock()

	for _, c := range cands {
		if ctx.Err() != nil {
			return
		}
		if w.excludeCWD != "" && strings.HasPrefix(c.CWD, w.excludeCWD) {
			slog.Debug("compact: tick",
				"session_id", c.SessionID,
				"outcome", string(outcomeSkippedSelf))
			continue
		}
		w.tick(ctx, c)
	}
}

// tickOutcome enumerates the distinct outcomes of a single watcher
// tick.
type tickOutcome string

const (
	outcomeCompacted        tickOutcome = "compacted"
	outcomeNothingToCompact tickOutcome = "nothing_to_compact"
	outcomeBudgetExceeded   tickOutcome = "budget_exceeded"
	outcomeFailed           tickOutcome = "failed"
	outcomeSkippedSelf      tickOutcome = "skipped_self"
)

// tick runs one compaction attempt for one candidate session. The
// candidate's CWD has already been checked against excludeCWD at
// scan time; we still load the target graph from cwd to anchor the
// summariser prompt.
func (w *Watcher) tick(ctx context.Context, c store.CompactionCandidate) {
	tc := w.loadTargetContext(c.SessionID)
	outcome, extra := w.runTick(ctx, c, tc)

	level := slog.LevelDebug
	switch outcome {
	case outcomeCompacted, outcomeBudgetExceeded:
		level = slog.LevelInfo
	case outcomeFailed:
		level = slog.LevelWarn
	}

	args := []any{
		"connection_id", c.ConnectionID,
		"session_id", c.SessionID,
		"outcome", string(outcome),
	}
	args = append(args, extra...)
	slog.Log(ctx, level, "compact: tick", args...)
}

func (w *Watcher) runTick(ctx context.Context, c store.CompactionCandidate, tc *TargetContext) (tickOutcome, []any) {
	comp, err := w.compactor.Compact(ctx, c.ConnectionID, c.SessionID, tc)
	switch {
	case err == nil:
		return outcomeCompacted, []any{
			"compaction_id", comp.ID,
			"entry_id_to", comp.EntryIDTo,
		}
	case errors.Is(err, ErrNothingToCompact):
		return outcomeNothingToCompact, nil
	case errors.Is(err, ErrBudgetExceeded):
		return outcomeBudgetExceeded, nil
	case errors.Is(err, exec.ErrNotFound):
		slog.Error("compact: claude subprocess spawn failed — executable not found in PATH",
			"err", err,
			"path", os.Getenv("PATH"),
			"session", c.SessionID,
		)
		return outcomeFailed, []any{"err", err.Error(), "path", os.Getenv("PATH")}
	default:
		return outcomeFailed, []any{"err", err.Error()}
	}
}

// loadTargetContext resolves the session's CWD and reads
// bullseye.yaml from that root if present. Errors degrade
// gracefully to nil (no target preface in the summariser prompt).
func (w *Watcher) loadTargetContext(sessionID string) *TargetContext {
	cwd := w.src.SessionCWD(sessionID)
	if cwd == "" {
		return nil
	}
	state, err := targets.LoadFromCWD(cwd)
	if err != nil {
		slog.Debug("compact: target graph load failed",
			"session", sessionID, "cwd", cwd, "err", err)
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
