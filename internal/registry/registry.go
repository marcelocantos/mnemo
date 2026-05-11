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
type userEntry struct {
	store   *store.Store
	vault   *vault.Exporter // nil when vault_path is not configured
	workers sync.WaitGroup
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

	e := &userEntry{store: s, vault: vaultExp}
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
		if e.vault != nil {
			e.workers.Add(1)
			go func() {
				defer e.workers.Done()
				logger.Info("vault: initial sync starting")
				if err := e.vault.Sync(r.baseCtx); err != nil && !errors.Is(err, vault.ErrSyncInFlight) {
					logger.Warn("vault: initial sync failed", "err", err)
				}
			}()
		}
		if err := e.store.Watch(); err != nil {
			logger.Error("watcher failed", "err", err)
		}
	}()

	if e.vault != nil {
		// Vault periodic sync: materialise new transcript entities as
		// Markdown every 5 minutes. Does NOT call IngestSynthesis here —
		// the file watcher below picks up those writes within ~2 seconds.
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-r.baseCtx.Done():
					return
				case <-ticker.C:
					if err := e.vault.Sync(r.baseCtx); err != nil && !errors.Is(err, vault.ErrSyncInFlight) {
						logger.Warn("vault: periodic sync failed", "err", err)
					}
				}
			}
		}()

		// Vault file watcher: re-indexes human annotations (content below the
		// <!-- mnemo:generated --> fence) within ~2 seconds of any .md save.
		// IngestVaultAnnotations extracts only below-fence content, so
		// generated blocks are never re-ingested and there is no feedback loop.
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			vaultPath := e.vault.Path()
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
				case <-r.baseCtx.Done():
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
		_ = e.store.Close()
	}
}
