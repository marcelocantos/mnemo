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
	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/reviewer"
	"github.com/marcelocantos/mnemo/internal/store"
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
	mu             sync.Mutex
	baseCtx        context.Context
	cancel         context.CancelFunc
	stores         map[string]*userEntry
	cfg            store.Config
	mnemoRepoDir   string
	compactorModel string
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
	store        *store.Store
	vault        *vault.Exporter // nil when vault_path is not configured
	workers      sync.WaitGroup
	vaultCancel  context.CancelFunc // cancels the vault sub-context; nil when vault disabled
	vaultWorkers sync.WaitGroup     // tracks only vault goroutines, so reload can wait for them
	homeDir      string             // remembered for Reload's ~/ expansion
}

// NewRegistry builds an empty Registry. The baseCtx is cancelled on
// Close and is the parent of every per-user worker context.
// mnemoRepoDir is passed to the compactor watcher (it's the same for
// every user — the compactor spawns claudia against mnemo's source
// tree regardless of whose transcripts are being compacted).
func NewRegistry(parent context.Context, cfg store.Config, mnemoRepoDir string) *Registry {
	ctx, cancel := context.WithCancel(parent)
	return &Registry{
		baseCtx:        ctx,
		cancel:         cancel,
		stores:         map[string]*userEntry{},
		cfg:            cfg,
		mnemoRepoDir:   mnemoRepoDir,
		compactorModel: "sonnet",
	}
}

// ForUser returns the Store for the given username, creating it on
// first access. The empty username resolves to the process's home
// directory (useful for foreground / brew-services runs where the
// default identity is implicit).
//
// Callers that must never silently index SYSTEM's profile should
// reject the empty username up-front via DefaultUsername.
func (r *Registry) ForUser(username string) (*store.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stores == nil {
		return nil, fmt.Errorf("registry is closed")
	}
	if e, ok := r.stores[username]; ok {
		return e.store, nil
	}

	home, err := store.ResolveHomeFor(username)
	if err != nil {
		return nil, err
	}

	projectDir := filepath.Join(home, ".claude", "projects")
	dbPath := filepath.Join(home, ".mnemo", "mnemo.db")

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	s, err := store.New(dbPath, projectDir)
	if err != nil {
		return nil, fmt.Errorf("open store for %q: %w", username, err)
	}
	s.SetWorkspaceRoots(r.cfg.ResolvedWorkspaceRoots())
	s.SetExtraProjectDirs(r.cfg.ExtraProjectDirs)

	synthRoots := r.cfg.ResolvedSynthesisRoots()
	var vaultExp *vault.Exporter
	if vaultPath := r.cfg.ResolvedVaultPath(home); vaultPath != "" {
		exp, err := vault.New(s, vaultPath)
		if err != nil {
			slog.Warn("vault: exporter creation failed", "path", vaultPath, "err", err)
		} else {
			vaultExp = exp
		}
	}
	s.SetSynthesisRoots(synthRoots)

	e := &userEntry{store: s, vault: vaultExp, homeDir: home}
	r.stores[username] = e
	r.startWorkers(username, projectDir, e)
	return s, nil
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

// startWorkers kicks off the per-user ingest / watcher / compactor /
// CI-poll goroutines. Each goroutine runs until r.baseCtx is
// cancelled (Registry.Close) or until it hits a terminal error.
func (r *Registry) startWorkers(username, projectDir string, e *userEntry) {
	logger := slog.Default().With("user", username)

	// Ingest + watcher + image workers + repo-level ingest streams.
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
		e.store.StartImageDescriber()
		e.store.StartImageOCR()
		e.store.StartImageEmbedder()
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
		// Initial vault sync: materialise all knowledge-graph entities as
		// Markdown notes. Spawned in its own goroutine so Watch() starts
		// immediately and live JSONL ingestion is not delayed. The SQLite
		// index is fully populated at this point (all Ingest* calls above
		// have completed), so the sync goroutine reads a consistent snapshot.
		r.mu.Lock()
		vp := e.vault
		r.mu.Unlock()
		if vp != nil {
			e.workers.Add(1)
			go func() {
				defer e.workers.Done()
				logger.Info("vault: initial sync starting")
				if err := vp.Sync(r.baseCtx); err != nil && !errors.Is(err, vault.ErrSyncInFlight) {
					logger.Warn("vault: initial sync failed", "err", err)
				}
			}()
		}
		if err := e.store.Watch(); err != nil {
			logger.Error("watcher failed", "err", err)
		}
	}()

	r.startVaultWorkers(username, e)

	// Compaction watcher.
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		caller := compact.NewClaudiaCaller(r.mnemoRepoDir, r.compactorModel)
		compactor := compact.New(e.store, caller, compact.Config{})
		watcher := compact.NewWatcher(e.store, compactor, compact.WatcherConfig{}, r.mnemoRepoDir)
		logger.Info("compact: watcher starting")
		watcher.Run(r.baseCtx)
	}()

	// CLAUDE.md summary review worker (🎯T41). Same claudia.Task
	// path as the compactor but a different cadence and trigger
	// (cheap-signal entry-count gate, see store.ShouldReview).
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		caller := compact.NewClaudiaCaller(r.mnemoRepoDir, r.compactorModel)
		rev := reviewer.New(e.store, llmAdapter{caller})
		reviewer.Run(r.baseCtx, rev)
	}()

	// CI polling.
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			if err := e.store.PollCI(); err != nil {
				logger.Warn("CI poll failed", "err", err)
			}
			select {
			case <-r.baseCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// Anthropic Admin API cost reconciler (🎯T45).
	// StartReconciler is a no-op when ANTHROPIC_ADMIN_API_KEY is absent.
	e.workers.Add(1)
	go func() {
		defer e.workers.Done()
		e.store.StartReconciler(r.baseCtx)
	}()
}

// startVaultWorkers launches the per-user vault periodic-sync and
// file-watcher goroutines under a vault-specific sub-context, so the
// mnemo_config tool can stop just those goroutines when vault_path
// changes without disturbing transcript ingest or the compactor.
//
// No-op when e.vault is nil. The vault pointer is captured locally so a
// concurrent hot-swap that replaces e.vault does not race with the
// goroutines already running against the previous exporter.
func (r *Registry) startVaultWorkers(username string, e *userEntry) {
	r.mu.Lock()
	vp := e.vault
	if vp == nil {
		r.mu.Unlock()
		return
	}
	vctx, vcancel := context.WithCancel(r.baseCtx)
	e.vaultCancel = vcancel
	r.mu.Unlock()

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
				if err := e.store.IngestVaultAnnotations(vaultPath); err != nil {
					logger.Warn("vault: annotation ingest failed", "err", err)
				}
				logger.Info("vault: annotations indexed from file change")
			}
		}
	}()
}

// CurrentConfig returns a snapshot of the live Config. Safe to call
// concurrently.
func (r *Registry) CurrentConfig() store.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
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
func (r *Registry) Reload(newCfg store.Config) ReloadReport {
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
	if old.VaultPath != newCfg.VaultPath {
		report.Changed = append(report.Changed, "vault_path")
		for username, e := range entries {
			r.swapVault(username, e, newCfg.ResolvedVaultPath(e.homeDir))
		}
		report.Adopted = append(report.Adopted, "vault_path")
	}
	if !linkedInstancesEqual(old.LinkedInstances, newCfg.LinkedInstances) {
		report.Changed = append(report.Changed, "linked_instances")
		report.RequiresRestart = append(report.RequiresRestart, "linked_instances")
	}
	return report
}

// swapVault tears down e's current vault workers, swaps in a fresh
// Exporter at newPath (or nil when newPath is ""), and starts new vault
// workers. Safe to call on a userEntry that currently has no vault: it
// will simply build one and start workers. Logs warnings rather than
// returning errors — partial success (e.g. the exporter built but a
// later sync failed) should not roll back the on-disk config.
func (r *Registry) swapVault(username string, e *userEntry, newPath string) {
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
		logger.Info("vault: disabled by reload (vault_path cleared)")
		return
	}

	exp, err := vault.New(e.store, newPath)
	if err != nil {
		logger.Warn("vault: exporter creation failed on reload", "path", newPath, "err", err)
		return
	}
	r.mu.Lock()
	e.vault = exp
	r.mu.Unlock()

	r.startVaultWorkers(username, e)
	logger.Info("vault: workers restarted with new path", "path", newPath)

	// Kick off an initial sync against the new path so notes appear
	// without waiting for the 5-minute ticker. Detached from the
	// caller's request context so the MCP call returns promptly.
	go func() {
		if err := exp.Sync(r.baseCtx); err != nil && !errors.Is(err, vault.ErrSyncInFlight) {
			logger.Warn("vault: post-reload sync failed", "err", err)
		}
	}()
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
func (r *Registry) Close() {
	r.mu.Lock()
	r.cancel()
	entries := make([]*userEntry, 0, len(r.stores))
	for _, e := range r.stores {
		entries = append(entries, e)
	}
	r.stores = nil
	r.mu.Unlock()

	for _, e := range entries {
		e.workers.Wait()
		e.vaultWorkers.Wait()
		_ = e.store.Close()
	}
}
