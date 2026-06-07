// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
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

	// AddendaBudgetTokens is the token-volume threshold that doubles as
	// the size floor and the re-compaction trigger (🎯T72). A session is
	// owed a compaction once the addenda token volume past its latest
	// cursor — SUM(output_tokens + cache_creation_tokens) over assistant
	// entries after the compacted span, or the whole session when none
	// has been compacted — reaches this value. A session whose whole
	// volume is below it has nothing dense to compress and is never
	// compacted: its raw entries are its retrieval form. Replaces the
	// message-count (trigger A) and idle-timeout (trigger B) heuristics,
	// which proxied content volume by message count. Default: 50k.
	AddendaBudgetTokens int64

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

	// MaxCompactionsPerScan bounds how many *successful* compactions
	// (real LLM calls) one scan performs before stopping early and
	// leaving the rest of the backlog for the next scan (🎯T68.1).
	// Cheap no-op ticks (nothing-to-compact, budget-exceeded,
	// skipped-self) do not count against it. Because candidates are
	// ordered most-recently-active first, live sessions are always
	// serviced before the cap is hit, so draining a large historical
	// backlog never starves current work or blows the LLM budget in a
	// single scan. Default: 20.
	MaxCompactionsPerScan int
}

func (c *WatcherConfig) scanInterval() time.Duration {
	if c.ScanInterval > 0 {
		return c.ScanInterval
	}
	return time.Minute
}

func (c *WatcherConfig) addendaBudgetTokens() int64 {
	if c.AddendaBudgetTokens > 0 {
		return c.AddendaBudgetTokens
	}
	return store.DefaultAddendaBudgetTokens
}

func (c *WatcherConfig) maxCompactionsPerScan() int {
	if c.MaxCompactionsPerScan > 0 {
		return c.MaxCompactionsPerScan
	}
	return 20
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
// Selection scans session_summary directly, computing each session's
// addenda token volume against the budget (🎯T72); daemon_connections
// is consulted only for best-effort connection_id attribution, exposed
// via the CompactionCandidate already filled by the store.
type storeSource interface {
	// SelectCompactionCandidates returns the sessions owed a
	// compaction under the token-volume predicate (🎯T72) plus the
	// total backlog (owed count before the limit). See
	// store.SelectCompactionCandidates for the full semantics; in
	// summary, a session is owed when the addenda token volume past its
	// latest cursor meets budgetTokens (the same metric over the whole
	// session is the size floor) and it is not a compactor-internal
	// session. There is no recency floor — recency is the ORDER BY
	// priority only. maxBudgetRatio is a runaway backstop.
	SelectCompactionCandidates(
		budgetTokens int64,
		maxBudgetRatio float64,
		limit int,
	) ([]store.CompactionCandidate, int, error)
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
	src       storeSource
	compactor *Compactor
	cfg       WatcherConfig

	mu              sync.Mutex
	lastN           int       // candidates returned by the last scan
	lastBacklog     int       // owed sessions before the per-scan limit (gap to fixed point)
	lastScanAt      time.Time // wall clock of the last scan return
	lastTickAt      time.Time // wall clock of the last tick completion
	lastTickOutcome tickOutcome
	inFlightSession string // session_id currently being ticked (or "")
	counts          map[tickOutcome]int64
}

// NewWatcher builds an activity-driven watcher. Self-sessions (claudia
// summariser runs) are no longer excluded here by cwd prefix; they are
// flagged at ingest (session_meta.compactor_internal) and filtered out
// inside SelectCompactionCandidates, so a genuine dev session sharing
// the mnemo repo cwd stays eligible (🎯T72).
func NewWatcher(src storeSource, c *Compactor, cfg WatcherConfig) *Watcher {
	return &Watcher{
		src:       src,
		compactor: c,
		cfg:       cfg,
		counts:    map[tickOutcome]int64{},
	}
}

// HealthSnapshot is the externally-visible state of the watcher,
// surfaced by the mnemo_compactor_status MCP tool. Lets agents (and
// humans) answer "is the compactor working, what was its last
// outcome, what is it doing right now?" without grepping the daemon
// log.
type HealthSnapshot struct {
	LastScanAt            time.Time
	LastScanCount         int
	Backlog               int // owed-but-uncompacted sessions at last scan (gap to fixed point)
	LastTickAt            time.Time
	LastTickOutcome       string
	InFlightSession       string
	Counts                map[string]int64
	ScanInterval          time.Duration
	TickTimeout           time.Duration
	AddendaBudgetTokens   int64
	MaxCompactionsPerScan int
	MaxTokenRatio         float64
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
		LastScanAt:            w.lastScanAt,
		LastScanCount:         w.lastN,
		Backlog:               w.lastBacklog,
		LastTickAt:            w.lastTickAt,
		LastTickOutcome:       string(w.lastTickOutcome),
		InFlightSession:       w.inFlightSession,
		Counts:                counts,
		ScanInterval:          w.cfg.scanInterval(),
		TickTimeout:           w.cfg.tickTimeout(),
		AddendaBudgetTokens:   w.cfg.addendaBudgetTokens(),
		MaxCompactionsPerScan: w.cfg.maxCompactionsPerScan(),
		MaxTokenRatio:         w.compactor.MaxTokenRatio(),
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
	cands, backlog, err := w.src.SelectCompactionCandidates(
		w.cfg.addendaBudgetTokens(),
		w.compactor.MaxTokenRatio(),
		w.cfg.maxCandidates(),
	)
	elapsed := time.Since(now)
	if err != nil {
		slog.Warn("compact: scan candidates failed", "err", err, "elapsed", elapsed.Round(time.Millisecond))
		return
	}
	// 🎯T70.4: log every scan so a slow candidate-selection query is
	// visible without a debug-level flip. WARN once elapsed crosses the
	// soak threshold so the regression we hit on 2026-05-30 (~30 min
	// scans masked by silent ticks) is caught at INFO+1 next time.
	const slowScanThreshold = 5 * time.Second
	logFn := slog.Info
	if elapsed >= slowScanThreshold {
		logFn = slog.Warn
	}
	logFn("compact: scan",
		"candidates", len(cands),
		"backlog", backlog,
		"elapsed", elapsed.Round(time.Millisecond),
	)
	w.mu.Lock()
	w.lastN = len(cands)
	w.lastBacklog = backlog
	w.lastScanAt = time.Now()
	w.mu.Unlock()

	// Per-scan compaction cap (🎯T68.1): bound the number of real
	// compactions one scan performs so draining a large historical
	// backlog never starves live sessions (which sort first) or
	// exhausts the LLM budget in a single pass. Cheap no-op outcomes
	// (nothing/budget) do not count. The remainder is left for the next
	// scan; the cap is logged, never a silent truncation. Compactor-
	// internal sessions are no longer skipped here — they are filtered
	// out of the candidate set at selection time (🎯T72).
	perScanCap := w.cfg.maxCompactionsPerScan()
	compacted := 0
	for _, c := range cands {
		if ctx.Err() != nil {
			return
		}
		if w.tick(ctx, c) == outcomeCompacted {
			compacted++
			if compacted >= perScanCap {
				slog.Info("compact: per-scan cap reached; deferring backlog to next scan",
					"compacted", compacted,
					"backlog", backlog,
					"max_per_scan", perScanCap)
				return
			}
		}
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
// bounded timeout (🎯T67). We load the target graph from the session's
// cwd to anchor the summariser prompt. A tick that exceeds
// w.cfg.TickTimeout is cancelled and recorded as
// outcomeTimeout — the watcher proceeds to the next candidate rather
// than wedging on a single stuck call. Returns the outcome so the
// scan loop can count successful compactions against the per-scan cap.
func (w *Watcher) tick(ctx context.Context, c store.CompactionCandidate) tickOutcome {
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
	return outcome
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
