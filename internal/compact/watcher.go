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
		quarantineThreshold int,
		quarantineSince time.Time,
		limit int,
	) ([]store.CompactionCandidate, int, error)
	// SessionCWD returns the cwd recorded for a session, used by
	// loadTargetContext to find a bullseye.yaml alongside the
	// session's working tree. Empty if unknown.
	SessionCWD(sessionID string) string
	// RecordCompactionFailure / ClearCompactionFailure maintain the
	// durable per-session quarantine (🎯T77): the watcher records a hard
	// failure or non-payload deferral and clears it on a clean tick, and
	// SelectCompactionCandidates excludes quarantined sessions.
	RecordCompactionFailure(sessionID, errMsg string) error
	ClearCompactionFailure(sessionID string) error
	// QuarantinedCount reports how many sessions are currently excluded
	// by the quarantine, for mnemo_compactor_status.
	QuarantinedCount(threshold int, since time.Time) int
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

	mu                  sync.Mutex
	lastN               int       // candidates returned by the last scan
	lastBacklog         int       // owed sessions before the per-scan limit (gap to fixed point)
	lastQuarantined     int       // sessions excluded by the durable quarantine at last scan (🎯T77)
	lastScanAt          time.Time // wall clock of the last scan return
	lastTickAt          time.Time // wall clock of the last tick completion
	lastTickOutcome     tickOutcome
	inFlightSession     string                // session_id currently being ticked (or "")
	counts              map[tickOutcome]int64 // lifetime tick-outcome tallies
	globalCooldownUntil time.Time             // watcher-wide pause after a rate limit (🎯T72)
}

// inGlobalCooldown reports whether the watcher is paused after a recent
// rate limit, and until when.
func (w *Watcher) inGlobalCooldown(now time.Time) (bool, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return now.Before(w.globalCooldownUntil), w.globalCooldownUntil
}

func (w *Watcher) setGlobalCooldown(until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if until.After(w.globalCooldownUntil) {
		w.globalCooldownUntil = until
	}
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
	Quarantined           int // sessions excluded by the durable quarantine at last scan (🎯T77)
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
		Quarantined:           w.lastQuarantined,
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
	quarantineSince := now.Add(-store.DefaultQuarantineCooldown)
	cands, backlog, err := w.src.SelectCompactionCandidates(
		w.cfg.addendaBudgetTokens(),
		w.compactor.MaxTokenRatio(),
		store.DefaultQuarantineThreshold,
		quarantineSince,
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
	quarantined := w.src.QuarantinedCount(store.DefaultQuarantineThreshold, quarantineSince)
	w.mu.Lock()
	w.lastN = len(cands)
	w.lastBacklog = backlog
	w.lastQuarantined = quarantined
	w.lastScanAt = time.Now()
	w.mu.Unlock()

	// A rate limit earlier put the whole watcher to sleep: skip ticking
	// entirely until the cooldown elapses (🎯T72) so we don't march every
	// owed session through the same wall and rack up failures.
	if cooling, until := w.inGlobalCooldown(now); cooling {
		slog.Info("compact: in global cooldown after a rate limit; skipping ticks",
			"until", until.Format(time.RFC3339))
		return
	}

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
		// Quarantined sessions are already excluded by the candidate
		// query (🎯T77), so every candidate here is eligible.
		outcome := w.tick(ctx, c)
		switch outcome {
		case outcomeRateLimited:
			// Account-wide condition: stop this scan and back the whole
			// watcher off rather than failing every remaining candidate.
			w.setGlobalCooldown(time.Now().Add(rateLimitCooldown))
			slog.Warn("compact: summariser rate-limited; backing off",
				"cooldown", rateLimitCooldown)
			return
		case outcomeFailed, outcomeTimeout, outcomeDeferred:
			// Durably record the failure (🎯T77) so a permanently-broken
			// session is quarantined across restarts. Deferrals (non-
			// payload replies) accrue toward quarantine too, even though
			// they are not hard failures in the ratio.
			if rerr := w.src.RecordCompactionFailure(c.SessionID, string(outcome)); rerr != nil {
				slog.Warn("compact: record failure", "session_id", c.SessionID, "err", rerr)
			}
		default:
			// compacted / nothing_to_compact / budget_exceeded — the
			// session is healthy; clear any durable quarantine.
			if cerr := w.src.ClearCompactionFailure(c.SessionID); cerr != nil {
				slog.Warn("compact: clear failure", "session_id", c.SessionID, "err", cerr)
			}
		}
		if outcome == outcomeCompacted {
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
	// outcomeRateLimited (🎯T72) marks a tick whose summariser hit a
	// transient external wall (usage/rate limit, API outage). It is NOT
	// a hard failure: it self-heals and triggers a global backoff rather
	// than being retried session-by-session.
	outcomeRateLimited tickOutcome = "rate_limited"
	// outcomeDeferred (🎯T77) marks a tick whose summariser returned a
	// non-payload — usually a conversational reply to the transcript
	// ("Understood — waiting for your direction") rather than the JSON
	// object. It is NOT a hard failure (it does not count in the failed
	// tally), but it accrues toward the durable quarantine so a session
	// that keeps producing non-payloads is eventually excluded.
	outcomeDeferred tickOutcome = "deferred"
)

// rateLimitCooldown is how long the whole watcher pauses after the
// summariser reports a usage/rate limit (🎯T72). Per-session failure
// throttling is now durable (🎯T77 quarantine in the candidate query),
// replacing the old in-memory exponential backoff.
const rateLimitCooldown = 10 * time.Minute

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
	case outcomeFailed, outcomeTimeout, outcomeRateLimited:
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
	case errors.Is(err, ErrLLMUnavailable):
		return outcomeRateLimited, []any{"reason", err.Error()}
	case errors.Is(err, ErrNoPayload):
		// The summariser replied but not with a payload (🎯T77) — not a
		// hard failure; defer the session and let it accrue toward
		// quarantine.
		return outcomeDeferred, []any{"reason", err.Error()}
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
