// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package registry owns the per-user Store lifecycle.
//
// Each incoming MCP request carries an implicit or explicit user
// identity (via ?user=<name> or the process owner). Registry maps
// those identities to lazily-created Store instances and per-user
// background workers (ingest, watcher, compactor, CI polling).
//
// Registry lives in its own package rather than inside internal/store
// because it imports internal/compact, which imports internal/store —
// a store-owned Registry would create a dependency cycle.
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/mnemo/internal/boot"
	"github.com/marcelocantos/mnemo/internal/breaker"
	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/plugin"
	"github.com/marcelocantos/mnemo/internal/reviewer"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/streamseg"
	"github.com/marcelocantos/mnemo/internal/throttle"
	"github.com/marcelocantos/mnemo/internal/upgrade"
	"github.com/marcelocantos/mnemo/internal/vault"
)

// llmAdapter bridges compact.LLMCaller to reviewer.LLMCaller. The
// two interfaces have the same shape; the type alias would create
// an import cycle since reviewer can't import compact.
type llmAdapter struct {
	c *compact.ClaudiaCaller
}

func (a llmAdapter) Call(ctx context.Context, sys, user string) (reviewer.LLMResult, error) {
	res, err := a.c.Call(ctx, sys, user)
	if err != nil {
		return reviewer.LLMResult{}, err
	}
	return reviewer.LLMResult{
		Text:         res.Text,
		Model:        res.Model,
		PromptTokens: res.PromptTokens,
		OutputTokens: res.OutputTokens,
		CostUSD:      res.CostUSD,
	}, nil
}

// Registry holds per-user Store instances plus their background
// workers. Stores are created lazily on first access via ForUser —
// this keeps a Windows-Service mnemo daemon (running as LocalSystem)
// idle until a request arrives carrying `?user=<name>`, at which
// point that user's transcript tree, database, and workers spin up.
//
// Multiple concurrent requests for the same user share a single
// Store instance. Registry.Close waits for every user's workers to
// drain and closes every Store.
type Registry struct {
	mu sync.Mutex
	// reloadMu serializes Reload calls. Holding it across the entire
	// Reload flow (snapshot → adopt → swap) prevents two concurrent
	// reloads from racing through swapVault and orphaning workers /
	// leaving the registry with two live exporters per user. mu is
	// still acquired in fine-grained sections inside Reload; reloadMu
	// is the coarse-grained guard.
	reloadMu sync.Mutex
	baseCtx  context.Context
	cancel   context.CancelFunc
	stores   map[string]*userEntry
	// creating is single-flight for in-progress ForUser constructions.
	// store.New (schema + pre-migration backup) can take minutes on a
	// large DB; we must not hold mu across that window or /health and
	// other registry methods block for the whole backup.
	creating          map[string]chan struct{}
	cfg               store.Config
	summariserWorkDir string
	compactorModel    string
	// upgradeDetector and lease are optional 🎯T97 wiring; set from main
	// after construction. nil means the corresponding diag checks report
	// "not configured" and background workers always start.
	upgradeDetector *upgrade.Detector
	lease           *upgrade.Lease
	// plugins is the process-wide plugin registry (🎯T102.2). nil until
	// SetPluginManager; Reload no-ops plugins when unset (tests).
	plugins *plugin.Manager
	// signals evaluates config signal_sources (🎯T102.8).
	signals *plugin.SignalEvaluator
	// governor throttles the agents mnemo invokes itself once a budget's
	// soft limit is breached (🎯T136). Never nil — an unconfigured budget
	// yields a governor that permits everything, so callers need no
	// nil check on a hot path.
	governor *throttle.Governor
}

// userEntry tracks one user's Store, optional vault Exporter, and
// background goroutines. workers lets Close wait for them to drain
// before the Store is closed.
//
// Vault workers are tracked separately (vaultCancel + vaultWorkers) so
// the mnemo_config tool can hot-swap vault_path: cancel the old vault
// sub-context, wait for its goroutines to drain, then start fresh ones
// against the new vault path. Non-vault workers (ingest, compactor, CI
// poller, reconciler) all continue uninterrupted, since the Store and
// transcript ingest pipeline are unaffected by a vault path change.
type userEntry struct {
	store             *store.Store
	vault             *vault.Exporter  // nil when vault_path is not configured
	compactWatcher    *compact.Watcher // background compaction watcher; nil before startWorkers
	workers           sync.WaitGroup
	vaultCancel       context.CancelFunc // cancels the vault sub-context; nil when vault disabled
	vaultWorkers      sync.WaitGroup     // tracks only vault goroutines, so reload can wait for them
	reconcilerCancel  context.CancelFunc // cancels the reconciler sub-context; nil when disabled
	reconcilerWorkers sync.WaitGroup     // tracks reconciler goroutine for hot-reload
	homeDir           string             // remembered for Reload's ~/ expansion
	bgStarted         bool               // true after startWorkers launched singleton bg work
	projectDir        string             // transcript root for deferred startWorkers
}

// NewRegistry builds an empty Registry. The baseCtx is cancelled on
// Close and is the parent of every per-user worker context.
// summariserWorkDir is the cwd for the compactor/reviewer `claude -p`
// subprocesses (the same for every user — a neutral scratch dir, not a
// per-user path). Empty disables summarisation (🎯T82).
func NewRegistry(parent context.Context, cfg store.Config, summariserWorkDir string) *Registry {
	ctx, cancel := context.WithCancel(parent)
	return &Registry{
		baseCtx:           ctx,
		cancel:            cancel,
		stores:            map[string]*userEntry{},
		creating:          map[string]chan struct{}{},
		cfg:               cfg,
		summariserWorkDir: summariserWorkDir,
		compactorModel:    "sonnet",
		governor:          throttle.New(mnemoDir()),
	}
}

// mnemoDir resolves ~/.mnemo for durable throttle state, falling back to
// a temp dir so a governor is always constructible. A throttle that
// cannot persist is worse than one that persists imperfectly: it silently
// resets on every restart, and the auto-upgrade path restarts the daemon
// on its own schedule.
func mnemoDir() string {
	home, err := store.EffectiveHome()
	if err != nil {
		return os.TempDir()
	}
	dir := filepath.Join(home, ".mnemo")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// Governor exposes the budget throttle for the health report and for
// evaluation from the scheduler.
func (r *Registry) Governor() *throttle.Governor { return r.governor }

// EvaluateThrottle re-reads the budget and updates the throttle level
// (🎯T136). Called on the diag scheduler's full pass: a budget does not
// change on a three-minute timescale, and re-running several usage
// aggregations more often than that would itself be a cost.
func (r *Registry) EvaluateThrottle(defaultUser string) {
	r.mu.Lock()
	e, ok := r.stores[defaultUser]
	cfg := r.cfg
	r.mu.Unlock()
	if !ok || e.store == nil {
		return
	}
	b, err := e.store.BudgetStatusNow(cfg.Budget, time.Now())
	if err != nil {
		return
	}
	r.governor.Evaluate(throttle.BudgetView{
		Priced:       b.Priced,
		CapUSD:       b.CapUSD,
		SpentPct:     b.SpentPct,
		ProjectedPct: b.ProjectedPct,
		WarnPct:      cfg.Budget.EffectiveWarnPct(),
	})
}

// SetPluginManager wires the 🎯T102 plugin registry. Call once from
// main after construction; the first Reconcile runs with the startup
// config so enabled plugins come up without waiting for a hot-reload.
// Also registers plugin home paths on existing stores for the T52
// loop-safety fence (🎯T102.12).
func (r *Registry) SetPluginManager(m *plugin.Manager) {
	r.mu.Lock()
	r.plugins = m
	cfg := r.cfg
	entries := make([]*userEntry, 0, len(r.stores))
	for _, e := range r.stores {
		entries = append(entries, e)
	}
	r.mu.Unlock()
	if m != nil {
		m.Reconcile(r.baseCtx, cfg.Plugins)
		// Fence plugin homes so ingest never indexes plugin output.
		for _, home := range m.PluginHomes(cfg.Plugins) {
			for _, e := range entries {
				e.store.RegisterExcludedPath(home, "plugin_home")
			}
		}
	}
}

// PluginManager returns the wired plugin manager, or nil.
func (r *Registry) PluginManager() *plugin.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.plugins
}

// SetSignalEvaluator wires declarative signal sources (🎯T102.8).
func (r *Registry) SetSignalEvaluator(e *plugin.SignalEvaluator) {
	r.mu.Lock()
	r.signals = e
	r.mu.Unlock()
}

// SignalEvaluator returns the wired evaluator, or nil.
func (r *Registry) SignalEvaluator() *plugin.SignalEvaluator {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.signals
}

// ForUser returns the Store for the given username, creating it on
// first access. The empty username resolves to the process's home
// directory (useful for foreground / brew-services runs where the
// default identity is implicit).
//
// Callers that must never silently index SYSTEM's profile should
// reject the empty username up-front via DefaultUsername.
//
// Heavy work (store.New → applySchema → pre-migration backup) runs
// *outside* r.mu so /health and other registry methods stay responsive
// during multi-minute startups. Concurrent first-access for the same
// username single-flights via r.creating.
func (r *Registry) ForUser(username string) (*store.Store, error) {
	for {
		r.mu.Lock()
		if r.stores == nil {
			r.mu.Unlock()
			return nil, fmt.Errorf("registry is closed")
		}
		if e, ok := r.stores[username]; ok {
			s := e.store
			r.mu.Unlock()
			return s, nil
		}
		if wait, ok := r.creating[username]; ok {
			r.mu.Unlock()
			<-wait
			continue // re-check stores / closed after peer finishes
		}
		done := make(chan struct{})
		r.creating[username] = done
		cfg := r.cfg
		r.mu.Unlock()

		entry, err := r.openUserStore(username, cfg)
		r.mu.Lock()
		delete(r.creating, username)
		close(done)
		if r.stores == nil {
			r.mu.Unlock()
			if entry != nil && entry.store != nil {
				entry.store.Close()
			}
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("registry is closed")
		}
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		// Another path cannot have inserted the same user while we held
		// the creating slot; still guard closed-races above.
		r.stores[username] = entry
		boot.Set(boot.PhaseStartingWorkers, "starting per-user workers for "+username)
		r.startWorkers(username, entry.projectDir, entry)
		st := entry.store
		r.mu.Unlock()
		return st, nil
	}
}

// openUserStore builds a store + vault entry without holding r.mu.
// The caller owns single-flight and the insert into r.stores.
func (r *Registry) openUserStore(username string, cfg store.Config) (*userEntry, error) {
	home, err := store.ResolveHomeFor(username)
	if err != nil {
		return nil, err
	}

	projectDir := filepath.Join(home, ".claude", "projects")
	// SQLite stays at ~/.mnemo/mnemo.db (daemon state). The Obsidian vault
	// is a separate tree (vault_path / vault_layout); do not relocate the DB
	// under vault/ — that was out of scope for 🎯T64.2.
	dbPath := filepath.Join(home, ".mnemo", "mnemo.db")

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	s, err := store.New(dbPath, projectDir)
	if err != nil {
		return nil, fmt.Errorf("open store for %q: %w", username, err)
	}
	s.SetWorkspaceRoots(cfg.ResolvedWorkspaceRoots())
	s.SetExtraProjectDirs(cfg.ExtraProjectDirs)
	s.SetCodexRoots(store.CodexRootsFor(home)) // 🎯T99: index ~/.codex rollouts
	s.SetGrokRoots(store.GrokRootsFor(home))   // 🎯T110: index ~/.grok sessions
	s.SetTodoGlobs(cfg.TodoGlobs)

	synthRoots := cfg.ResolvedSynthesisRoots()
	var vaultExp *vault.Exporter
	if vaultPath := cfg.ResolvedVaultPath(home); vaultPath != "" {
		// Exclude the vault path from ingest walkers before any
		// Ingest* call runs. Without this, a vault sitting inside a
		// synthesis root or repo docs/ tree would have its generated
		// content re-ingested on every Sync, growing the docs index
		// without bound.
		s.RegisterExcludedPath(vaultPath, "vault_path")
		s.SetVaultPath(vaultPath) // 🎯T68.6: vault divergence + GC machinery needs the path
		exp, err := vault.New(s, vaultPath, vault.Options{
			Layout:        cfg.ResolvedVaultLayout(vaultPath),
			SoakWarnAfter: cfg.ResolvedVaultLayoutSoakWarnAfter(),
		})
		if err != nil {
			slog.Warn("vault: exporter creation failed", "path", vaultPath, "err", err)
		} else {
			vaultExp = exp
		}
	}
	s.SetSynthesisRoots(synthRoots)

	return &userEntry{store: s, vault: vaultExp, homeDir: home, projectDir: projectDir}, nil
}

// SetUpgradeDetector wires the 🎯T97.2 release detector for diag checks.
func (r *Registry) SetUpgradeDetector(d *upgrade.Detector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upgradeDetector = d
}

// UpgradeDetector returns the detector set via SetUpgradeDetector.
func (r *Registry) UpgradeDetector() *upgrade.Detector {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upgradeDetector
}

// SetLease wires the 🎯T97.4 singleton background lease.
func (r *Registry) SetLease(l *upgrade.Lease) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lease = l
}

// Lease returns the lease set via SetLease.
func (r *Registry) Lease() *upgrade.Lease {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease
}

// ReleaseLease drops the background lease if held (drain path 🎯T97.4).
func (r *Registry) ReleaseLease() {
	r.mu.Lock()
	l := r.lease
	r.mu.Unlock()
	if l != nil {
		_ = l.Release()
	}
}

// EnsureBackgroundWorkers starts deferred per-user workers when this
// process holds the lease (handoff after another backend released).
func (r *Registry) EnsureBackgroundWorkers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lease != nil && !r.lease.Held() {
		return
	}
	for username, e := range r.stores {
		if e.bgStarted {
			continue
		}
		r.startWorkers(username, e.projectDir, e)
	}
}

// VaultFor returns the vault Exporter for username, or nil when vault is
// not configured or the user has not yet been initialised. Safe to call
// concurrently.
func (r *Registry) VaultFor(username string) *vault.Exporter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.stores[username]; ok {
		return e.vault
	}
	return nil
}

// CompactWatcherFor returns the compaction Watcher for username, or
// nil when the user has not yet been initialised. Used by the
// mnemo_compactor_status MCP tool (🎯T67) to surface watcher health
// — last scan / tick timestamps, in-flight session, lifetime tick
// counts — without grepping the daemon log.
func (r *Registry) CompactWatcherFor(username string) *compact.Watcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.stores[username]; ok {
		return e.compactWatcher
	}
	return nil
}

// startWorkers kicks off the per-user ingest / watcher / compactor /
// CI-poll goroutines. Each goroutine runs until r.baseCtx is
// cancelled (Registry.Close) or until it hits a terminal error.
//
// When a background lease is configured and this process does not hold
// it, workers are deferred (🎯T97.4) so a second backend during upgrade
// cannot double-ingest. EnsureBackgroundWorkers starts them after
// lease acquisition.
func (r *Registry) startWorkers(username, projectDir string, e *userEntry) {
	logger := slog.Default().With("user", username)

	if e.bgStarted {
		return
	}
	if r.lease != nil && !r.lease.Held() {
		logger.Info("deferring background workers: not lease holder")
		return
	}
	e.bgStarted = true
	if r.lease != nil {
		r.lease.SetRunningBackground(true)
	}

	// Realtime transcript watcher. Start this before the cold catch-up
	// backlog so new appends are indexed with stack-like priority.
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		if err := e.store.Watch(r.baseCtx); err != nil {
			logger.Error("watcher failed", "err", err)
		}
	}()

	// Ingest + image workers + repo-level ingest streams.
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		logger.Info("ingesting transcripts", "dir", projectDir)
		if err := e.store.IngestAll(); err != nil {
			logger.Error("initial ingest failed", "err", err)
		}
		if stats, err := e.store.Stats(); err == nil {
			logger.Info("ingest complete",
				"sessions", stats.TotalSessions,
				"messages", stats.TotalMessages)
		}
		// 🎯T64.10: structural topic segments in the background so they
		// never delay docs/todos/plans backfill stamps (ingest.backfill
		// health check keys off ingest_status.last_backfill).
		// Recompute session provenance from ingest paths (🎯T127). Cheap —
		// it walks ingest_state, not session_meta — and idempotent, so it
		// costs nothing once the index is clean. It runs every start
		// because the data it repairs was written by code that no longer
		// exists, and nothing else will ever revisit those rows: ingest is
		// offset-based, so a consumed file is never re-parsed.
		if retagged, removed, err := e.store.RepairSessionSources(); err != nil {
			logger.Warn("session provenance repair failed", "err", err)
		} else if retagged > 0 || removed > 0 {
			logger.Info("session provenance repaired",
				"retagged", retagged, "phantoms_removed", removed)
		}
		go func() {
			// Wait out any deferred schema upgrade first (🎯T114.1): the
			// pass projects compactions into spans via topic_segments.
			// compaction_id, a column a pending migration may still be
			// adding while the store serves on the old schema.
			e.store.AwaitSchemaUpgrade()
			if err := e.store.SegmentAllSessions(); err != nil {
				logger.Warn("segment backfill failed", "err", err)
			} else {
				logger.Info("segment backfill complete")
			}
		}()
		// Also gated on the deferred upgrade (🎯T114.1). The embedder
		// records each attempt in image_embedding_attempts (🎯T121), a
		// table a pending migration may still be adding — and an attempt
		// that cannot be recorded is retried forever, which is the very
		// thing that table exists to stop. Observed on the upgrade boot:
		// an embed failure at 23:47:15 went unrecorded because the table
		// did not land until 23:57:11. Descriptions and OCR ride along so
		// all image work starts from one settled schema.
		go func() {
			e.store.AwaitSchemaUpgrade()
			e.store.StartImageDescriber()
			e.store.StartImageOCR()
			e.store.StartImageEmbedder()
		}()
		if err := e.store.IngestMemories(); err != nil {
			logger.Error("memory ingest failed", "err", err)
		}
		if err := e.store.IngestSkills(); err != nil {
			logger.Error("skill ingest failed", "err", err)
		}
		if err := e.store.IngestClaudeConfigs(); err != nil {
			logger.Error("claude config ingest failed", "err", err)
		}
		if err := e.store.IngestAuditLogs(); err != nil {
			logger.Error("audit log ingest failed", "err", err)
		}
		if err := e.store.IngestTargets(); err != nil {
			logger.Error("target ingest failed", "err", err)
		}
		if err := e.store.IngestPlans(); err != nil {
			logger.Error("plan ingest failed", "err", err)
		}
		if err := e.store.IngestDocs(); err != nil {
			logger.Error("doc ingest failed", "err", err)
		}
		if err := e.store.IngestSynthesis(); err != nil {
			logger.Error("synthesis ingest failed", "err", err)
		}
		if err := e.store.IngestTodos(); err != nil {
			logger.Error("todo ingest failed", "err", err)
		}
		// 🎯T93: refresh planner statistics once the initial ingest has
		// landed its bulk writes. On a fresh install (which skips the
		// migration ANALYZE) this gives the planner its first stats so
		// covering indexes are used; on an upgrade it keeps them current
		// after the startup catch-up. Cheap and self-tuning.
		e.store.Optimize()
		// Initial vault sync: materialise all knowledge-graph entities as
		// Markdown notes. Spawned in its own goroutine so Watch() starts
		// immediately and live JSONL ingestion is not delayed. The SQLite
		// index is fully populated at this point (all Ingest* calls above
		// have completed), so the sync goroutine reads a consistent snapshot.
		//
		// Tracked under vaultWorkers (not workers) so a concurrent
		// mnemo_config vault_path swap waits for it to drain before
		// starting the new exporter, guaranteeing the old exporter
		// finishes writing to the old path before workers spin up
		// against the new one.
		r.mu.Lock()
		vp := e.vault
		if vp != nil {
			e.vaultWorkers.Add(1)
		}
		r.mu.Unlock()
		if vp != nil {
			go func() {
				defer e.vaultWorkers.Done()
				logger.Info("vault: initial sync starting")
				if err := vp.Sync(r.baseCtx); err != nil && !errors.Is(err, vault.ErrSyncInFlight) {
					logger.Warn("vault: initial sync failed", "err", err)
				}
			}()
		}
	}()

	r.startVaultWorkers(username, e)

	// Summariser-backed workers (compactor, CLAUDE.md reviewer) only
	// start when there is a usable working directory for the `claude -p`
	// subprocess. An empty summariserWorkDir means even the temp dir
	// couldn't be created at startup (🎯T82); rather than spawn into a
	// missing cwd and fail every tick, we skip these workers entirely
	// and log once. Ingest and the other workers below run regardless.
	if r.summariserWorkDir == "" {
		logger.Warn("compaction and CLAUDE.md review disabled: no usable summariser workdir")
	} else {
		// Compaction watcher.
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			caller := compact.NewClaudiaCaller(r.summariserWorkDir, r.compactorModel)
			compactor := compact.New(e.store, caller, compact.Config{})
			watcher := compact.NewWatcher(e.store, compactor, compact.WatcherConfig{})
			// Budget throttle (🎯T136).
			watcher.Allow = func() (bool, time.Duration) {
				return r.governor.Allow(throttle.Compaction)
			}
			e.compactWatcher = watcher
			logger.Info("compact: watcher starting")
			watcher.Run(r.baseCtx)
		}()

		// CLAUDE.md summary review worker (🎯T41). Same claudia.Task
		// path as the compactor but a different cadence and trigger
		// (cheap-signal entry-count gate, see store.ShouldReview).
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			caller := compact.NewClaudiaCaller(r.summariserWorkDir, r.compactorModel)
			rev := reviewer.New(e.store, llmAdapter{caller})
			reviewer.Run(r.baseCtx, rev)
		}()
	}

	// Planner-statistics maintenance (🎯T93): periodically run
	// `PRAGMA optimize` so the query planner keeps choosing the right
	// indexes as the DB grows over a long-running daemon's lifetime.
	// Self-tuning and cheap (analyses only tables whose stats have
	// drifted). The post-ingest call covers startup; this covers the days
	// a daemon stays up between restarts.
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-r.baseCtx.Done():
				return
			case <-ticker.C:
				e.store.Optimize()
			}
		}
	}()

	// External mirror reconciler (🎯T68.5): divergence-driven reconcile
	// of the mirror streams (CI today; GitHub/commits as they convert).
	// Ticks every minute but reconciles a repo's stream only when its
	// mirror_status cursor is missing or older than the stream's
	// interval, so a newly-seen repo is picked up promptly while fresh
	// repos are skipped. Replaces the fixed 5-minute PollCI loop.
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		// Per-stream circuit breakers (🎯T84): a reconciler that keeps
		// erroring (e.g. gh down, a wedged query) trips after 5 consecutive
		// failures and is skipped for 10m, so one broken stream can't
		// retry hot every minute forever.
		breakers := map[string]*breaker.Breaker{}
		for {
			now := time.Now()
			// 🎯T68.7 capstone: drive every registered periodic stream
			// through the StreamReconciler abstraction. Adding a new
			// stream is one entry in Store.StreamReconcilers(); this
			// loop stays the same.
			//
			// 🎯T102.7: plugin reconcile facets are thin HTTP adapters
			// (POST …/reconcile) returned by Manager.StreamReconcilers.
			// They share this single worker and the same T84 breakers —
			// no per-plugin tick loop. Facet HTTP calls use a short
			// timeout so a hung plugin cannot wedge the dispatcher.
			reconcilers := e.store.StreamReconcilers()
			if pm := r.PluginManager(); pm != nil {
				reconcilers = append(reconcilers, pm.StreamReconcilers()...)
			}
			for _, sr := range reconcilers {
				b := breakers[sr.Name()]
				if b == nil {
					b = breaker.New(5, 10*time.Minute)
					breakers[sr.Name()] = b
				}
				if !b.Allow(now) {
					continue
				}
				n, err := sr.Reconcile(r.baseCtx, now)
				if err != nil {
					b.Record(time.Now(), false, err.Error())
					logger.Warn("reconcile failed", "stream", sr.Name(), "err", err)
				} else {
					b.Record(time.Now(), true, "")
					if n > 0 {
						logger.Info("reconciled", "stream", sr.Name(), "count", n)
					}
				}
			}
			select {
			case <-r.baseCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// Anthropic Admin API cost reconciler (🎯T45, 🎯T63). Opt-in via
	// config.cost_reconciliation.enabled — disabled by default so that
	// no outbound Admin API call is made unless the operator
	// explicitly says so. Tracked per-userEntry so Reload can start
	// and stop the goroutine when the flag flips.
	r.startReconcilerWorker(e)

	// Periodic backup worker (🎯T61). Opted in by default; opt out via
	// {"backup": {"disabled": true}} in config.json.
	r.startBackupWorker(username, e, logger)

	// Daemon connection sweeper (🎯T60). Marks daemon_connections rows
	// closed once last_seen_at falls outside the idle threshold. The
	// HTTP MCP transport has no reliable disconnect signal, so this
	// sweep is the authoritative reaper. Opted in by default; opt out
	// via {"connection_sweep": {"disabled": true}}.
	r.startConnectionSweeper(e, logger)
}

// startConnectionSweeper spawns the per-user daemon_connections
// sweeper goroutine. On each tick it calls
// Store.MarkStaleConnectionsClosed; rows whose last_seen_at fell
// outside the idle threshold are marked closed. No-ops when the
// sweeper is disabled in config.
func (r *Registry) startConnectionSweeper(e *userEntry, logger *slog.Logger) {
	cfg := r.cfg.ConnectionSweep
	if !cfg.IsEnabled() {
		logger.Info("connection sweeper: disabled by config")
		return
	}
	interval, err := cfg.EffectiveInterval()
	if err != nil {
		logger.Warn("connection sweeper: bad interval, falling back to 1m", "err", err)
		interval = time.Minute
	}
	stale, err := cfg.EffectiveStaleAfter()
	if err != nil {
		logger.Warn("connection sweeper: bad stale_after, falling back to 10m", "err", err)
		stale = 10 * time.Minute
	}

	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			n, err := e.store.MarkStaleConnectionsClosed(stale, time.Now())
			if err != nil {
				logger.Warn("connection sweeper: failed", "err", err)
			} else if n > 0 {
				logger.Info("connection sweeper: closed stale rows",
					"count", n, "stale_after", stale)
			}
			select {
			case <-r.baseCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// startReconcilerWorker spawns (or no-ops) the per-user Anthropic
// Admin API reconciler goroutine. Reads the latest cfg.CostReconciliation
// flag and tracks the cancel func + waitgroup on e so a subsequent
// Reload can stop the goroutine cleanly when the flag flips off.
//
// Caller must hold no locks; this method serialises against r.mu only
// when writing the cancel func into e. Safe to call from both
// startWorkers (initial bring-up) and Reload (config flip).
func (r *Registry) startReconcilerWorker(e *userEntry) {
	enabled := r.cfg.CostReconciliation.IsEnabled()
	if !enabled {
		// Run the gated entry-point once anyway to surface the
		// "disabled" log line at the same level/cadence as before.
		// StartReconciler is a synchronous no-op in this branch.
		e.store.StartReconciler(r.baseCtx, false)
		return
	}
	ctx, cancel := context.WithCancel(r.baseCtx)
	r.mu.Lock()
	e.reconcilerCancel = cancel
	r.mu.Unlock()
	e.reconcilerWorkers.Add(1)
	go func() {
		defer e.reconcilerWorkers.Done()
		e.store.StartReconciler(ctx, true)
		// StartReconciler spawns its own inner goroutine and returns;
		// keep this outer goroutine alive until ctx is cancelled so
		// reconcilerWorkers.Wait() in Reload covers both layers.
		<-ctx.Done()
	}()
}

// stopReconcilerWorker cancels the per-user reconciler goroutine (if
// any) and waits for it to drain. Idempotent — safe to call when the
// reconciler is already stopped.
func (r *Registry) stopReconcilerWorker(e *userEntry) {
	r.mu.Lock()
	cancel := e.reconcilerCancel
	e.reconcilerCancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		e.reconcilerWorkers.Wait()
	}
}

// startBackupWorker resolves the backup config and launches the daily
// snapshot goroutine if backups are enabled. Misconfiguration (bad
// window times, bad quiescence duration) logs a warning and skips the
// worker — backup failures should never block the rest of the daemon.
func (r *Registry) startBackupWorker(username string, e *userEntry, logger *slog.Logger) {
	bcfg := r.cfg.Backup
	if !bcfg.IsEnabled() {
		logger.Info("backup: disabled by config")
		return
	}
	winStart, winEnd, err := bcfg.EffectiveWindow()
	if err != nil {
		logger.Warn("backup: invalid window, worker not started", "err", err)
		return
	}
	quiescence, err := bcfg.EffectiveQuiescenceMin()
	if err != nil {
		logger.Warn("backup: invalid quiescence_min, worker not started", "err", err)
		return
	}
	dir := bcfg.EffectiveDir(e.homeDir)
	keep := bcfg.EffectiveKeepDailies()

	w, err := backup.NewWorker(backup.Config{
		SrcPath:     e.store.DBPath(),
		Dir:         dir,
		Keep:        keep,
		WindowStart: winStart,
		WindowEnd:   winEnd,
		Quiescence:  quiescence,
		Activity:    e.store,
	})
	if err != nil {
		logger.Warn("backup: NewWorker failed, worker not started", "err", err)
		return
	}
	logger.Info("backup: worker starting",
		"dir", dir, "keep", keep,
		"window_start", winStart, "window_end", winEnd,
		"quiescence_min", quiescence)
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		w.Run(r.baseCtx)
	}()

	// Reclaim the -wal after write bursts. Started alongside the backup
	// worker because the two share the quiescence signal, and because the
	// backup's own VACUUM INTO is the single longest reader on the
	// system — the thing that lets the WAL reach its high-water mark in
	// the first place.
	e.store.StartWALMaintenance(r.baseCtx)

	r.startStreamSegWatcher(e)
	r.startStructuralRetirementBackfill(e)
}

// startStructuralRetirementBackfill demotes structural spans that a better
// span already covers, once, in the background (🎯T132.4).
//
// Retirement otherwise fires only when a compaction is written, so every
// session compacted BEFORE this shipped would keep its structural spans
// winning retrieval forever — a compacted session is not owed again, so
// nothing would ever revisit it. That makes the one-shot pass a
// correctness requirement rather than an accelerator.
//
// Idempotent: it only touches rows whose superseded_by is still NULL, so
// re-running costs one indexed UPDATE that matches nothing.
func (r *Registry) startStructuralRetirementBackfill(e *userEntry) {
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		// The column this writes arrives with a deferred migration
		// (🎯T114.1); running before it lands would fail on a column
		// that does not exist yet.
		e.store.AwaitSchemaUpgrade()
		n, err := e.store.RetireStructuralSpansCovered("")
		if err != nil {
			slog.Warn("structural retirement backfill failed", "err", err)
			return
		}
		if n > 0 {
			slog.Info("retired structural spans covered by better ones", "count", n)
		}
	}()
}

// startStreamSegWatcher launches the live topic-span watcher (🎯T132.2),
// but only when the user has asked for it.
//
// The gate is not a formality. Enabling this runs a PERSISTENT Claude
// Code agent per live session, so it is continuous subscription spend
// proportional to how much the user is working, and it attaches a second
// agent to sessions they did not ask it to watch. Same posture as
// cost reconciliation and image embeddings: an ambient capability is not
// consent. With the config section absent, LiveSessions is never polled
// and no summariser process is ever spawned.
func (r *Registry) startStreamSegWatcher(e *userEntry) {
	full, err := store.LoadConfig()
	if err != nil {
		return // unreadable config is not consent
	}
	cfg := full.StreamingSegmentation
	if !cfg.Enabled {
		return
	}
	workDir, mkErr := os.MkdirTemp("", "mnemo-streamseg-")
	if mkErr != nil {
		slog.Warn("streaming segmentation not started: no working directory", "err", mkErr)
		return
	}
	w := &streamseg.Watcher{
		Live:          e.store,
		Store:         e.store,
		WorkDir:       workDir,
		DripSize:      cfg.DripSize,
		MaxConcurrent: cfg.MaxConcurrent,
		Model:         cfg.Model,
		NewSummariser: func(string) streamseg.Summariser {
			return streamseg.NewClaudiaSummariser(workDir, cfg.Model)
		},
		// Budget throttle (🎯T136): this tier pauses rather than slows.
		Paused: func() bool { return r.governor.Paused(throttle.Segmenter) },
	}
	slog.Info("streaming segmentation enabled",
		"model", cfg.Model, "drip_size", cfg.DripSize, "max_concurrent", cfg.MaxConcurrent)
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		// The schema carries method='stream' and superseded_by; a
		// watcher that outran a deferred upgrade would write into
		// columns that do not exist yet (the 🎯T114.1 hazard).
		e.store.AwaitSchemaUpgrade()
		w.Run(r.baseCtx)
	}()
}

// startVaultWorkers launches the per-user vault periodic-sync and
// file-watcher goroutines under a vault-specific sub-context, so the
// mnemo_config tool can stop just those goroutines when vault_path
// changes without disturbing transcript ingest or the compactor.
//
// Returns the vault sub-context so callers wanting to spawn additional
// vault-scoped goroutines (e.g. the post-reload initial sync) can tie
// them to the same cancellation as the periodic-sync/watcher pair.
// Returns nil when e.vault is nil (no workers started).
//
// PRECONDITION: caller MUST hold r.mu. ForUser holds it only around
// startWorkers (not during store.New); swapVault re-acquires it after
// building the new exporter. Re-acquiring inside this function would
// self-deadlock those paths.
//
// The vault pointer is captured locally so a concurrent hot-swap that
// replaces e.vault does not race with the goroutines already running
// against the previous exporter.
func (r *Registry) startVaultWorkers(username string, e *userEntry) context.Context {
	vp := e.vault
	if vp == nil {
		return nil
	}
	vctx, vcancel := context.WithCancel(r.baseCtx)
	e.vaultCancel = vcancel

	logger := slog.Default().With("user", username)

	// Vault periodic sync: materialise new transcript entities as
	// Markdown every 5 minutes. Does NOT call IngestSynthesis here —
	// the file watcher below picks up those writes within ~2 seconds.
	e.vaultWorkers.Add(1)
	go func() {
		defer e.vaultWorkers.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-vctx.Done():
				return
			case <-ticker.C:
				if err := vp.Sync(vctx); err != nil && !errors.Is(err, vault.ErrSyncInFlight) {
					logger.Warn("vault: periodic sync failed", "err", err)
				}
			}
		}
	}()

	// Vault file watcher: re-indexes human annotations (content below the
	// <!-- mnemo:generated --> fence) within ~2 seconds of any .md save.
	// IngestVaultAnnotations extracts only below-fence content, so
	// generated blocks are never re-ingested and there is no feedback loop.
	e.vaultWorkers.Add(1)
	go func() {
		defer e.vaultWorkers.Done()
		vaultPath := vp.Path()
		// vault.New already called os.MkdirAll; the directory exists.

		fw, err := fsnotify.NewWatcher()
		if err != nil {
			logger.Warn("vault: file watcher init failed", "err", err)
			return
		}
		defer fw.Close()

		// Add vault root and all existing subdirectories.
		// fsnotify v1.9 does not expose a public WithRecursive option
		// on all platforms, so we walk and add manually, then
		// re-add any newly created subdirectory on CREATE events.
		// Hidden dirs (.obsidian/, .git/, .trash/) are skipped to
		// avoid wasting inotify slots on Linux and to skip Obsidian
		// internal-state churn that has no signal for mnemo.
		addVaultDirs := func(root string) {
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil || !d.IsDir() {
					return nil
				}
				if p != root && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				_ = fw.Add(p)
				return nil
			})
		}
		addVaultDirs(vaultPath)
		logger.Info("vault: file watcher started", "path", vaultPath)

		const quietPeriod = 2 * time.Second
		debounce := time.NewTimer(quietPeriod)
		debounce.Stop()
		defer debounce.Stop()

		for {
			select {
			case <-vctx.Done():
				return
			case ev, ok := <-fw.Events:
				if !ok {
					return
				}
				// Watch newly created non-hidden subdirectories so notes
				// written into new sections are also picked up.
				if ev.Has(fsnotify.Create) {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() &&
						!strings.HasPrefix(filepath.Base(ev.Name), ".") {
						_ = fw.Add(ev.Name)
					}
				}
				if strings.HasSuffix(ev.Name, ".md") {
					debounce.Reset(quietPeriod)
				}
			case err, ok := <-fw.Errors:
				if !ok {
					return
				}
				logger.Warn("vault: watcher error", "err", err)
			case <-debounce.C:
				opts := r.vaultIndexingOptionsFor(vaultPath)
				if err := e.store.IngestVaultAnnotations(vaultPath, opts); err != nil {
					logger.Warn("vault: annotation ingest failed", "err", err)
				}
				logger.Info("vault: annotations indexed from file change", "scope", opts.Scope)
			}
		}
	}()

	return vctx
}

// CurrentConfig returns a snapshot of the live Config. Safe to call
// concurrently.
func (r *Registry) CurrentConfig() store.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// vaultIndexingOptionsFor builds the VaultIndexingOptions struct
// IngestVaultAnnotations expects, resolving any auto-default scope
// against the live vault tree (🎯T64.1). Reads the live config under
// the registry mutex so a concurrent Reload that swaps the indexing
// fields doesn't observe a half-updated struct.
func (r *Registry) vaultIndexingOptionsFor(resolvedVaultPath string) store.VaultIndexingOptions {
	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()
	return store.VaultIndexingOptions{
		Scope:      cfg.ResolvedVaultIndexingScope(resolvedVaultPath),
		Includes:   cfg.VaultIndexingIncludes,
		IgnoreFile: cfg.ResolvedVaultIndexingIgnoreFile(),
	}
}

// ReloadReport summarises what changed during a Reload call and which
// of those changes were adopted in-process versus deferred to the next
// daemon restart. The MCP tool surfaces this verbatim so the caller
// (most often a Claude Code agent on behalf of the user) can see at a
// glance whether the running daemon already reflects the new config.
type ReloadReport struct {
	// Changed lists the JSON keys whose values differ between the
	// previous and incoming Config (e.g. "vault_path").
	Changed []string
	// Adopted lists keys whose new values were applied to the running
	// daemon without a restart (a subset of Changed).
	Adopted []string
	// RequiresRestart lists keys whose values changed but cannot be
	// applied in-process (currently: "linked_instances"). These will
	// take effect only after the daemon is restarted.
	RequiresRestart []string
	// Warnings lists per-user adoption failures that happened despite
	// the config write itself succeeding. The classic case: vault.New
	// fails because the new vault_path points at a regular file (not
	// a directory). The config-on-disk is the new value, the old
	// vault workers are torn down, but the new exporter never came
	// up. Surfacing this here lets the MCP caller see the divergence
	// instead of believing the field was cleanly adopted.
	Warnings []string
}

// Reload swaps the Registry's active config for newCfg and adopts the
// changes across every already-initialised per-user entry. The caller
// is responsible for having validated newCfg (mnemo_config delegates to
// store.WriteConfig, which runs the same validation as LoadConfig).
//
// Adoption per field:
//   - workspace_roots, extra_project_dirs, synthesis_roots — applied
//     via the matching Store setters. New ingest passes will pick up
//     the new roots; already-indexed content is untouched.
//   - vault_path — the per-user vault sub-context is cancelled, its
//     goroutines drain, and fresh vault workers are started against
//     the new path (or vault is fully disabled if the new path is
//     empty). The initial sync against the new vault is kicked off in
//     the background; this call returns once the swap is complete.
//   - linked_instances — flagged as requires-restart. Federation peers
//     are wired up at startup against a process-wide http.Client; a
//     mid-run swap would need to tear down and rebuild every fan-out
//     handler, which is out of scope for this tool.
//   - plugins — the plugin Manager reconciles enable/disable/params
//     live (🎯T102.2); enable starts an instance, disable tears one
//     down. No daemon restart.
func (r *Registry) Reload(newCfg store.Config) ReloadReport {
	// Serialize reloads end-to-end. Two concurrent Reload calls would
	// otherwise both pass through the swapVault stages, with one
	// racing the other's vaultCancel/vault assignment under r.mu and
	// the result depending on goroutine interleavings. The MCP entry
	// point is the only caller in practice (single agent), but
	// nothing in the type signature enforces that, so guard it here.
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	r.mu.Lock()
	old := r.cfg
	r.cfg = newCfg
	entries := make(map[string]*userEntry, len(r.stores))
	for u, e := range r.stores {
		entries[u] = e
	}
	r.mu.Unlock()

	report := ReloadReport{}

	if !stringSlicesEqual(old.WorkspaceRoots, newCfg.WorkspaceRoots) {
		report.Changed = append(report.Changed, "workspace_roots")
		for _, e := range entries {
			e.store.SetWorkspaceRoots(newCfg.ResolvedWorkspaceRoots())
		}
		report.Adopted = append(report.Adopted, "workspace_roots")
	}
	if !stringSlicesEqual(old.ExtraProjectDirs, newCfg.ExtraProjectDirs) {
		report.Changed = append(report.Changed, "extra_project_dirs")
		for _, e := range entries {
			e.store.SetExtraProjectDirs(newCfg.ExtraProjectDirs)
		}
		report.Adopted = append(report.Adopted, "extra_project_dirs")
	}
	if !stringSlicesEqual(old.SynthesisRoots, newCfg.SynthesisRoots) {
		report.Changed = append(report.Changed, "synthesis_roots")
		for _, e := range entries {
			e.store.SetSynthesisRoots(newCfg.ResolvedSynthesisRoots())
		}
		report.Adopted = append(report.Adopted, "synthesis_roots")
	}
	if !stringSlicesEqual(old.TodoGlobs, newCfg.TodoGlobs) {
		report.Changed = append(report.Changed, "todo_globs")
		for _, e := range entries {
			e.store.SetTodoGlobs(newCfg.TodoGlobs)
		}
		report.Adopted = append(report.Adopted, "todo_globs")
	}
	if old.VaultPath != newCfg.VaultPath {
		report.Changed = append(report.Changed, "vault_path")
		anyFailure := false
		for username, e := range entries {
			if err := r.swapVault(username, e, newCfg.ResolvedVaultPath(e.homeDir)); err != nil {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("vault_path: user %q: %v", username, err))
				anyFailure = true
			}
		}
		// Even one failure means at least one Store does not have
		// the new vault active; refrain from claiming live adoption
		// in that case. The warning carries the detail.
		if !anyFailure {
			report.Adopted = append(report.Adopted, "vault_path")
		}
	}
	if !linkedInstancesEqual(old.LinkedInstances, newCfg.LinkedInstances) {
		report.Changed = append(report.Changed, "linked_instances")
		report.RequiresRestart = append(report.RequiresRestart, "linked_instances")
	}
	if old.CostReconciliation.IsEnabled() != newCfg.CostReconciliation.IsEnabled() {
		report.Changed = append(report.Changed, "cost_reconciliation.enabled")
		for _, e := range entries {
			// Tear down the existing goroutine (no-op when previously
			// disabled) then start a fresh one if the new state opts
			// in. startReconcilerWorker reads r.cfg, which was already
			// swapped above under r.mu.
			r.stopReconcilerWorker(e)
			r.startReconcilerWorker(e)
		}
		report.Adopted = append(report.Adopted, "cost_reconciliation.enabled")
	}
	if !pluginsEqual(old.Plugins, newCfg.Plugins) {
		report.Changed = append(report.Changed, "plugins")
		r.mu.Lock()
		pm := r.plugins
		r.mu.Unlock()
		if pm != nil {
			pm.Reconcile(r.baseCtx, newCfg.Plugins)
			// Keep T52 fence in sync with configured plugin homes.
			for _, e := range entries {
				for _, home := range pm.PluginHomes(newCfg.Plugins) {
					e.store.RegisterExcludedPath(home, "plugin_home")
				}
			}
			report.Adopted = append(report.Adopted, "plugins")
		} else {
			// No manager wired (tests / early startup) — config is
			// still the source of truth on disk; mark as restart so
			// the caller is not told the live set changed.
			report.RequiresRestart = append(report.RequiresRestart, "plugins")
		}
	}
	if !signalSourcesEqual(old.SignalSources, newCfg.SignalSources) {
		report.Changed = append(report.Changed, "signal_sources")
		home := ""
		for _, e := range entries {
			home = e.homeDir
			break
		}
		if home == "" {
			if h, err := store.EffectiveHome(); err == nil {
				home = h
			}
		}
		r.SetSignalEvaluator(plugin.NewSignalEvaluator(home, newCfg.SignalSources))
		report.Adopted = append(report.Adopted, "signal_sources")
	}
	return report
}

func signalSourcesEqual(a, b []store.SignalSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pluginsEqual reports whether two plugin lists are equal for Reload
// change detection. Order-sensitive: a reordering is a change.
func pluginsEqual(a, b []store.PluginEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].Enabled != b[i].Enabled ||
			a[i].Transport != b[i].Transport ||
			a[i].Command != b[i].Command ||
			a[i].URL != b[i].URL ||
			a[i].Script != b[i].Script ||
			!stringSlicesEqual(a[i].Args, b[i].Args) ||
			!paramsEqual(a[i].Params, b[i].Params) {
			return false
		}
	}
	return true
}

func paramsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	// Cheap structural compare via fmt for nested JSON-ish values.
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if fmt.Sprint(va) != fmt.Sprint(vb) {
			return false
		}
	}
	return true
}

// swapVault tears down e's current vault workers, swaps in a fresh
// Exporter at newPath (or nil when newPath is ""), and starts new vault
// workers. Safe to call on a userEntry that currently has no vault: it
// will simply build one and start workers. Logs warnings rather than
// returning errors — partial success (e.g. the exporter built but a
// later sync failed) should not roll back the on-disk config.
//
// Reload serializes calls to swapVault via reloadMu; without that, two
// concurrent reloads could clear each other's vaultCancel funcs and
// leave the entry with stale workers running against an abandoned
// exporter.
func (r *Registry) swapVault(username string, e *userEntry, newPath string) error {
	logger := slog.Default().With("user", username)

	r.mu.Lock()
	oldCancel := e.vaultCancel
	e.vaultCancel = nil
	oldVault := e.vault
	e.vault = nil
	r.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
		e.vaultWorkers.Wait()
		logger.Info("vault: workers stopped for reload", "previous_path", safePath(oldVault))
	}

	if newPath == "" {
		e.store.SetVaultPath("") // 🎯T68.6 clear so the vault divergence gatherer reports unknown
		logger.Info("vault: disabled by reload (vault_path cleared)")
		return nil
	}

	exp, err := vault.New(e.store, newPath, vault.Options{
		Layout:        r.cfg.ResolvedVaultLayout(newPath),
		SoakWarnAfter: r.cfg.ResolvedVaultLayoutSoakWarnAfter(),
	})
	if err != nil {
		logger.Warn("vault: exporter creation failed on reload", "path", newPath, "err", err)
		return fmt.Errorf("vault.New(%q): %w", newPath, err)
	}
	e.store.SetVaultPath(newPath) // 🎯T68.6 mirror new vault path for divergence + GC
	r.mu.Lock()
	e.vault = exp
	vctx := r.startVaultWorkers(username, e)
	// Track the post-reload initial sync under vaultWorkers so a
	// subsequent swap waits for it (no two syncs against the same
	// exporter racing through writeNote) and Close blocks on its
	// completion before closing the Store. Bound to vctx (not
	// r.baseCtx) so cascaded reloads (A→B→C) abort the in-flight
	// B-sync via oldCancel() at the next swap instead of forcing C
	// to block on B-sync's natural completion against a path the
	// user has already moved away from.
	e.vaultWorkers.Add(1)
	r.mu.Unlock()

	logger.Info("vault: workers restarted with new path", "path", newPath)

	go func() {
		defer e.vaultWorkers.Done()
		if err := exp.Sync(vctx); err != nil && !errors.Is(err, vault.ErrSyncInFlight) {
			logger.Warn("vault: post-reload sync failed", "err", err)
		}
	}()
	return nil
}

func safePath(v *vault.Exporter) string {
	if v == nil {
		return ""
	}
	return v.Path()
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// linkedInstancesEqual compares element-by-element with ==. All
// LinkedInstance fields must remain comparable for this to work: adding
// a slice field would surface as a compile error (caught), but adding a
// map field would compile and panic at runtime when both sides are
// non-nil. If LinkedInstance ever gains a non-comparable field, switch
// to reflect.DeepEqual on the element (or slices.EqualFunc with an
// explicit comparator updated alongside the struct).
func linkedInstancesEqual(a, b []store.LinkedInstance) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Close cancels every worker context and closes every Store. Safe to
// call once.
//
// Acquires reloadMu before r.mu so that an in-flight swapVault — which
// drops r.mu between teardown and the post-Wait re-entry that spawns
// new vault workers — cannot interleave with Close. Without this guard
// Close could observe vaultWorkers at zero, return from Wait, and then
// see swapVault's re-entry Add() new workers against a Store that
// Close is about to close (closed-DB log noise on shutdown, and a
// WaitGroup contract that depends on Wait's previous return value
// being final).
func (r *Registry) Close() {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	r.mu.Lock()
	r.cancel()
	entries := make([]*userEntry, 0, len(r.stores))
	for _, e := range r.stores {
		entries = append(entries, e)
	}
	r.stores = nil
	pm := r.plugins
	r.plugins = nil
	r.mu.Unlock()

	if pm != nil {
		pm.Close()
	}
	for _, e := range entries {
		// Wait for workers, but never unconditionally (🎯T122). r.cancel()
		// above signals them, yet not every worker is actually
		// cancellable: the mirror streams shell out to `gh` and `git log`
		// via exec.Command with no context, so a subprocess mid-flight
		// runs to completion no matter what shutdown wants. Blocking on
		// that starved the step below — Store.Close is the only caller of
		// the WAL checkpoint, and before this it never ran once in
		// practice, leaving a 2.3 GB -wal and a crash recovery on every
		// start.
		//
		// Durability beats tidiness here: the process is exiting, so an
		// abandoned subprocess is harmless (it is reparented and dies on
		// its own), whereas a skipped checkpoint is not.
		if !waitFor(&e.workers, workerDrainGrace) {
			slog.Warn("drain: workers did not stop in time; checkpointing anyway",
				"grace", workerDrainGrace)
		}
		if !waitFor(&e.vaultWorkers, workerDrainGrace) {
			slog.Warn("drain: vault workers did not stop in time; checkpointing anyway",
				"grace", workerDrainGrace)
		}
		_ = e.store.Close()
	}
}

// workerDrainGrace is how long Close waits for a user's worker
// goroutines to observe cancellation before proceeding to checkpoint
// without them (🎯T122). Generous enough that a cancellable worker
// finishes the iteration it is on, short enough that an un-cancellable
// subprocess cannot consume the shutdown budget.
const workerDrainGrace = 3 * time.Second

// waitFor waits on wg for at most d, reporting whether it completed.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
