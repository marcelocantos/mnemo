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

// WatcherConfig tunes the activity-driven Watcher (🎯T59, expanded
// 🎯T67). Zero values mean "use defaults".
type WatcherConfig struct {
	// ScanInterval is how often the watcher scans session_summary
	// for compaction candidates. Default: 1 minute.
	ScanInterval time.Duration

	// MinDeltaMessages is trigger A's threshold: a session qualifies
	// if it has accumulated this many substantive messages since its
	// latest compaction's entry_id_to (or since first_msg if no
	// prior compaction exists). Pre-T67 this was measured against
	// the session's lifetime substantive_msgs counter — the
	// implementation drift that left already-compacted sessions
	// stuck in the candidate set forever. Default: 50.
	MinDeltaMessages int

	// IdleTimeout is trigger B's idle threshold: a session with at
	// least one new substantive message AND last_msg older than
	// (now - IdleTimeout) qualifies. Captures small/one-shot
	// sessions that never reach MinDeltaMessages but go quiet long
	// enough to be worth summarising for restore. Default: 15
	// minutes.
	IdleTimeout time.Duration

	// RecencyWindow caps how stale a session's last_msg may be and
	// still be considered at all. Sessions whose latest message is
	// older than (now - RecencyWindow) are dropped, regardless of
	// trigger. Default: 24 hours — wide enough that an "idle for a
	// few hours" session still gets compacted via trigger B, but
	// narrow enough that long-dead sessions don't keep flowing
	// through the SQL.
	RecencyWindow time.Duration

	// TickTimeout caps wall-clock time for a single compactor.Compact
	// call. A stuck claudia subprocess, runaway query, or lock
	// contention that exceeds the timeout cancels the tick and the
	// watcher proceeds to the next candidate. Without this, a single
	// pathological tick wedges the entire scan loop indefinitely.
	// Default: 5 minutes.
	TickTimeout time.Duration

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

func (c *WatcherConfig) minDeltaMessages() int {
	if c.MinDeltaMessages > 0 {
		return c.MinDeltaMessages
	}
	return 50
}

func (c *WatcherConfig) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return 15 * time.Minute
}

func (c *WatcherConfig) recencyWindow() time.Duration {
	if c.RecencyWindow > 0 {
		return c.RecencyWindow
	}
	return 24 * time.Hour
}

func (c *WatcherConfig) tickTimeout() time.Duration {
	if c.TickTimeout > 0 {
		return c.TickTimeout
	}
	return 5 * time.Minute
}

func (c *WatcherConfig) maxCandidates() int {
	if c.MaxCandidates > 0 {
		return c.MaxCandidates
	}
	return 100
}

// storeSource is the narrow slice of the store the Watcher needs.
// Under the activity-driven model (🎯T59) — and the dual-trigger
// candidate filter added in 🎯T67 — selection scans session_summary
// directly with a delta-message and idle-time computation;
// daemon_connections is consulted only for best-effort connection_id
// attribution and is exposed via the CompactionCandidate already
// filled by the store.
type storeSource interface {
	// SelectCompactionCandidates returns sessions worth a tick under
	// the 🎯T67 dual-trigger model. See store.SelectCompactionCandidates
	// for the full semantics; in summary, a session qualifies when
	// last_msg is within recencyCutoff AND it has new substantive
	// messages since the latest compaction AND either delta >=
	// minDeltaMsgs OR last_msg <= idleCutoff. maxBudgetRatio
	// pre-filters budget-exhausted sessions.
	SelectCompactionCandidates(
		minDeltaMsgs int,
		idleCutoff time.Time,
		recencyCutoff time.Time,
		maxBudgetRatio float64,
		limit int,
	) ([]store.CompactionCandidate, error)
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

	mu              sync.Mutex
	lastN           int       // candidates returned by the last scan
	lastScanAt      time.Time // wall clock of the last scan return
	lastTickAt      time.Time // wall clock of the last tick completion
	lastTickOutcome tickOutcome
	inFlightSession string // session_id currently being ticked (or "")
	counts          map[tickOutcome]int64
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
		counts:     map[tickOutcome]int64{},
	}
}

// HealthSnapshot is the externally-visible state of the watcher,
// surfaced by the mnemo_compactor_status MCP tool. Lets agents (and
// humans) answer "is the compactor working, what was its last
// outcome, what is it doing right now?" without grepping the daemon
// log.
type HealthSnapshot struct {
	LastScanAt       time.Time
	LastScanCount    int
	LastTickAt       time.Time
	LastTickOutcome  string
	InFlightSession  string
	Counts           map[string]int64
	ScanInterval     time.Duration
	IdleTimeout      time.Duration
	RecencyWindow    time.Duration
	TickTimeout      time.Duration
	MinDeltaMessages int
	MaxTokenRatio    float64
}

// Health returns a snapshot of the watcher's runtime state.
func (w *Watcher) Health() HealthSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	counts := make(map[string]int64, len(w.counts))
	for k, v := range w.counts {
		counts[string(k)] = v
	}
	return HealthSnapshot{
		LastScanAt:       w.lastScanAt,
		LastScanCount:    w.lastN,
		LastTickAt:       w.lastTickAt,
		LastTickOutcome:  string(w.lastTickOutcome),
		InFlightSession:  w.inFlightSession,
		Counts:           counts,
		ScanInterval:     w.cfg.scanInterval(),
		IdleTimeout:      w.cfg.idleTimeout(),
		RecencyWindow:    w.cfg.recencyWindow(),
		TickTimeout:      w.cfg.tickTimeout(),
		MinDeltaMessages: w.cfg.minDeltaMessages(),
		MaxTokenRatio:    w.compactor.MaxTokenRatio(),
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
	idleCutoff := now.Add(-w.cfg.idleTimeout())
	recencyCutoff := now.Add(-w.cfg.recencyWindow())
	cands, err := w.src.SelectCompactionCandidates(
		w.cfg.minDeltaMessages(),
		idleCutoff,
		recencyCutoff,
		w.compactor.MaxTokenRatio(),
		w.cfg.maxCandidates(),
	)
	if err != nil {
		slog.Warn("compact: scan candidates failed", "err", err)
		return
	}
	w.mu.Lock()
	w.lastN = len(cands)
	w.lastScanAt = time.Now()
	w.mu.Unlock()

	for _, c := range cands {
		if ctx.Err() != nil {
			return
		}
		if w.excludeCWD != "" && strings.HasPrefix(c.CWD, w.excludeCWD) {
			w.recordOutcome(c.SessionID, outcomeSkippedSelf)
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
	outcomeTimeout          tickOutcome = "timeout"
	outcomeSkippedSelf      tickOutcome = "skipped_self"
)

// recordOutcome bumps the lifetime counter for outcome and records
// the most recent tick state. Safe to call concurrently.
func (w *Watcher) recordOutcome(sessionID string, outcome tickOutcome) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.counts == nil {
		w.counts = map[tickOutcome]int64{}
	}
	w.counts[outcome]++
	w.lastTickAt = time.Now()
	w.lastTickOutcome = outcome
	if sessionID != "" {
		w.inFlightSession = ""
	}
}

// markInFlight publishes which session is currently being ticked.
// Health() readers use this to distinguish "watcher is working" from
// "watcher is stuck".
func (w *Watcher) markInFlight(sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlightSession = sessionID
}

// tick runs one compaction attempt for one candidate session under a
// bounded timeout (🎯T67). The candidate's CWD has already been
// checked against excludeCWD at scan time; we still load the target
// graph from cwd to anchor the summariser prompt. A tick that
// exceeds w.cfg.TickTimeout is cancelled and recorded as
// outcomeTimeout — the watcher proceeds to the next candidate rather
// than wedging on a single stuck call.
func (w *Watcher) tick(ctx context.Context, c store.CompactionCandidate) {
	w.markInFlight(c.SessionID)

	tickCtx, cancel := context.WithTimeout(ctx, w.cfg.tickTimeout())
	defer cancel()

	start := time.Now()
	tc := w.loadTargetContext(c.SessionID)
	outcome, extra := w.runTick(tickCtx, c, tc)
	elapsed := time.Since(start)

	// runTick may have observed the context deadline as a generic
	// failure; reclassify so the log entry and metrics call it a
	// timeout, which the watcher treats as a non-fatal "skip and
	// move on".
	if outcome == outcomeFailed && tickCtx.Err() == context.DeadlineExceeded {
		outcome = outcomeTimeout
		extra = []any{"elapsed", elapsed}
	}

	w.recordOutcome(c.SessionID, outcome)

	level := slog.LevelDebug
	switch outcome {
	case outcomeCompacted:
		level = slog.LevelInfo
	case outcomeFailed, outcomeTimeout:
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
