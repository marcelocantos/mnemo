// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package vault writes mnemo's knowledge graph as Markdown notes to a
// directory compatible with Obsidian and Logseq.
//
// When vault_path is set in ~/.mnemo/config.json, mnemo continuously
// materialises its SQLite knowledge graph into a tree of Markdown files
// that humans can read, annotate, and extend in any note-taking tool
// that supports the CommonMark / wiki-link conventions used by Obsidian
// and Logseq.
//
// The vault is bidirectional: mnemo writes notes on every ingest cycle
// and adds the vault directory as a SynthesisRoot so that human-added
// content (new files, annotations below the fence marker) is picked up
// by the FTS5 index and flows back into mnemo_search and all other
// query tools.
//
// Human edits are preserved across re-syncs via a <!-- mnemo:generated -->
// fence: generated content lives above the fence, human notes live below
// and are never overwritten.
//
// Vault structure:
//
//	<vault_path>/
//	├── index.md               — root index (all repos, total sessions)
//	├── repos/<repo>.md        — per-repo index with recent sessions + decisions
//	├── sessions/<repo>/       — one note per session (full conversation)
//	├── decisions/<repo>/      — one note per extracted decision
//	├── memories/              — project memory notes (flat, globally unique names)
//	├── skills/                — skill procedure notes from ~/.claude/skills/
//	├── configs/               — CLAUDE.md project instruction notes
//	├── plans/<repo>/          — planning documents
//	├── targets/<repo>/        — convergence targets
//	├── ci/<repo>/             — CI run summaries
//	└── prs/<repo>/            — PR and issue notes
package vault

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// generatedFence separates mnemo-generated content (above the line) from
// human-added content (below the line). Re-syncing rewrites everything
// above this marker while leaving everything below untouched.
const generatedFence = "<!-- mnemo:generated -->"

// Exporter writes mnemo's knowledge graph as Markdown vault notes.
type Exporter struct {
	backend store.Backend
	path    string
}

// New creates a new Exporter rooted at path. The directory is created if
// it does not exist. path must already be ~ expanded (use
// Config.ResolvedVaultPath).
func New(backend store.Backend, path string) (*Exporter, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("vault: create root %s: %w", path, err)
	}
	return &Exporter{backend: backend, path: path}, nil
}

// Path returns the vault root directory.
func (e *Exporter) Path() string { return e.path }

// Sync performs a full vault synchronisation: sessions, decisions, memories,
// plans, targets, CI runs, PRs, per-repo indices, and the root index.
//
// Session notes whose vault file mtime is newer than the session's last
// message timestamp are skipped so that repeated syncs are fast after the
// initial run.
func (e *Exporter) Sync(ctx context.Context) error {
	slog.Info("vault: sync starting", "path", e.path)
	start := time.Now()

	// Sessions must be synced first: the path map they produce is needed
	// by decisions (for back-links) and repo indices (for forward-links).
	sessionPaths, err := e.syncSessions(ctx)
	var firstErr error
	setErr := func(e error) {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}
	setErr(err)
	setErr(e.syncDecisions(ctx, sessionPaths))
	setErr(e.syncMemories(ctx))
	setErr(e.syncPlans(ctx))
	setErr(e.syncTargets(ctx))
	setErr(e.syncCI(ctx))
	setErr(e.syncPRs(ctx))
	setErr(e.syncSkills(ctx))
	setErr(e.syncConfigs(ctx))
	// Repo indices and root index are written last so they reflect the
	// session paths already materialised above. Fetch repos once and
	// pass to both so we avoid two identical ListRepos queries.
	repos, err := e.backend.ListRepos("")
	setErr(err)
	setErr(e.syncRepoIndices(ctx, repos, sessionPaths))
	setErr(e.syncRootIndex(repos))

	slog.Info("vault: sync complete",
		"elapsed", time.Since(start).Round(time.Millisecond),
		"err", firstErr)
	return firstErr
}

// syncSessions writes a vault note for every session and returns a map of
// sessionID → vault-relative note path for use by later sync passes.
func (e *Exporter) syncSessions(ctx context.Context) (map[string]string, error) {
	sessions, err := e.backend.ListSessions("all", 1, 100000, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("vault: list sessions: %w", err)
	}

	pathMap := make(map[string]string, len(sessions))
	written, skipped := 0, 0

	for _, s := range sessions {
		if ctx.Err() != nil {
			break
		}
		relPath := sessionPath(s)
		pathMap[s.SessionID] = relPath
		absPath := filepath.Join(e.path, relPath)

		if !needsUpdate(absPath, s.LastMsg) {
			skipped++
			continue
		}

		msgs, err := readAllMessages(e.backend, s.SessionID)
		if err != nil {
			slog.Warn("vault: read session messages failed",
				"session", shortID(s.SessionID), "err", err)
			continue
		}

		if err := writeNote(absPath, renderSession(s, msgs)); err != nil {
			slog.Warn("vault: write session note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: sessions synced", "written", written, "skipped", skipped)
	return pathMap, nil
}

// syncDecisions writes a vault note for every detected decision.
func (e *Exporter) syncDecisions(ctx context.Context, sessionPaths map[string]string) error {
	// days=36500 effectively means "all time".
	decisions, err := e.backend.SearchDecisions("", "", 36500, 100000)
	if err != nil {
		return fmt.Errorf("vault: search decisions: %w", err)
	}

	written, skipped := 0, 0
	for _, d := range decisions {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, decisionPath(d))
		if !needsUpdate(absPath, d.Timestamp) {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderDecision(d, sessionPaths[d.SessionID])); err != nil {
			slog.Warn("vault: write decision note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: decisions synced", "written", written, "skipped", skipped)
	return nil
}

// syncMemories writes a vault note for every indexed memory file.
func (e *Exporter) syncMemories(ctx context.Context) error {
	memories, err := e.backend.SearchMemories("", "", "", 100000)
	if err != nil {
		return fmt.Errorf("vault: search memories: %w", err)
	}

	written, skipped := 0, 0
	for _, m := range memories {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, memoryPath(m))
		if !needsUpdate(absPath, m.UpdatedAt) {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderMemory(m)); err != nil {
			slog.Warn("vault: write memory note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: memories synced", "written", written, "skipped", skipped)
	return nil
}

// syncPlans writes a vault note for every indexed plan.
func (e *Exporter) syncPlans(ctx context.Context) error {
	plans, err := e.backend.SearchPlans("", "", 100000)
	if err != nil {
		return fmt.Errorf("vault: search plans: %w", err)
	}

	written, skipped := 0, 0
	for _, p := range plans {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, planPath(p))
		if !needsUpdate(absPath, p.UpdatedAt) {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderPlan(p)); err != nil {
			slog.Warn("vault: write plan note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: plans synced", "written", written, "skipped", skipped)
	return nil
}

// syncTargets writes a vault note for every indexed convergence target.
func (e *Exporter) syncTargets(ctx context.Context) error {
	targets, err := e.backend.SearchTargets("", "", "", 100000)
	if err != nil {
		return fmt.Errorf("vault: search targets: %w", err)
	}

	written, skipped := 0, 0
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, targetPath(t))
		// Targets carry no reliable last-modified timestamp. Notes are
		// written once and not refreshed until the file is deleted.
		// targetPath encodes the target ID, not the status, so a status
		// change does not rename the file — human annotations survive.
		if _, err := os.Stat(absPath); err == nil {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderTarget(t)); err != nil {
			slog.Warn("vault: write target note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: targets synced", "written", written, "skipped", skipped)
	return nil
}

// syncCI writes a vault note for every indexed CI run.
func (e *Exporter) syncCI(ctx context.Context) error {
	runs, err := e.backend.SearchCI("", "", "", 36500, 100000)
	if err != nil {
		return fmt.Errorf("vault: search CI runs: %w", err)
	}

	written, skipped := 0, 0
	for _, r := range runs {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, ciRunPath(r))
		if !needsUpdate(absPath, r.CompletedAt) {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderCIRun(r)); err != nil {
			slog.Warn("vault: write CI run note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: CI runs synced", "written", written, "skipped", skipped)
	return nil
}

// syncPRs writes a vault note for every indexed PR and issue.
func (e *Exporter) syncPRs(ctx context.Context) error {
	prs, err := e.backend.SearchGitHubActivity("", "", "", "", "all", 36500, 100000)
	if err != nil {
		return fmt.Errorf("vault: search PRs: %w", err)
	}

	written, skipped := 0, 0
	for _, r := range prs {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, prPath(r))
		if !needsUpdate(absPath, r.UpdatedAt) {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderPR(r)); err != nil {
			slog.Warn("vault: write PR note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: PRs/issues synced", "written", written, "skipped", skipped)
	return nil
}

// syncRepoIndices writes a per-repo index note for every known repository.
func (e *Exporter) syncRepoIndices(ctx context.Context, repos []store.RepoInfo, sessionPaths map[string]string) error {
	for _, repo := range repos {
		if ctx.Err() != nil {
			break
		}
		sessions, err := e.backend.ListSessions("interactive", 1, 20, "", repo.Repo, "")
		if err != nil {
			slog.Warn("vault: list sessions for repo index failed", "repo", repo.Repo, "err", err)
		}
		decisions, err := e.backend.SearchDecisions("", repo.Repo, 36500, 20)
		if err != nil {
			slog.Warn("vault: search decisions for repo index failed", "repo", repo.Repo, "err", err)
		}
		content := renderRepoIndex(repo, sessions, decisions, sessionPaths)
		absPath := filepath.Join(e.path, repoIndexPath(repo.Repo))
		if err := writeNote(absPath, content); err != nil {
			slog.Warn("vault: write repo index failed", "repo", repo.Repo, "err", err)
		}
	}
	return nil
}

// syncSkills writes a vault note for every indexed skill file.
func (e *Exporter) syncSkills(ctx context.Context) error {
	skills, err := e.backend.SearchSkills("", 100000)
	if err != nil {
		return fmt.Errorf("vault: search skills: %w", err)
	}

	written, skipped := 0, 0
	for _, s := range skills {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, skillPath(s))
		if !needsUpdate(absPath, s.UpdatedAt) {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderSkill(s)); err != nil {
			slog.Warn("vault: write skill note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: skills synced", "written", written, "skipped", skipped)
	return nil
}

// syncConfigs writes a vault note for every indexed CLAUDE.md config file.
func (e *Exporter) syncConfigs(ctx context.Context) error {
	configs, err := e.backend.SearchClaudeConfigs("", "", 100000)
	if err != nil {
		return fmt.Errorf("vault: search configs: %w", err)
	}

	written, skipped := 0, 0
	for _, c := range configs {
		if ctx.Err() != nil {
			break
		}
		absPath := filepath.Join(e.path, configPath(c))
		if !needsUpdate(absPath, c.UpdatedAt) {
			skipped++
			continue
		}
		if err := writeNote(absPath, renderConfig(c)); err != nil {
			slog.Warn("vault: write config note failed", "path", absPath, "err", err)
			continue
		}
		written++
	}

	slog.Info("vault: configs synced", "written", written, "skipped", skipped)
	return nil
}

// syncRootIndex writes the vault root index.md.
func (e *Exporter) syncRootIndex(repos []store.RepoInfo) error {
	stats, _ := e.backend.Stats()
	return writeNote(filepath.Join(e.path, "index.md"), renderRootIndex(repos, stats))
}

// needsUpdate returns true when the vault file at absPath does not exist or
// its modification time is before ts (RFC3339). An empty ts always returns
// true so that callers that have no reliable timestamp force a write.
func needsUpdate(absPath, ts string) bool {
	if ts == "" {
		return true
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return true // file absent or unreadable
	}
	// Parse ts: try nanosecond, then second precision, then date-only.
	var t time.Time
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, ts); err == nil {
			t = parsed
			break
		}
	}
	if t.IsZero() {
		return true // unparseable timestamp → always regenerate
	}
	return info.ModTime().Before(t)
}

// writeNote writes generated content to absPath, preserving any
// human-added content that follows the generatedFence marker in an
// existing file. The fence is always written; human content (if any)
// is appended after it.
func writeNote(absPath, generated string) error {
	// Harvest human content from the existing file, if any.
	human := ""
	if existing, err := os.ReadFile(absPath); err == nil {
		raw := string(existing)
		if idx := strings.LastIndex(raw, generatedFence); idx >= 0 {
			after := strings.TrimLeft(raw[idx+len(generatedFence):], "\n")
			if after != "" {
				human = after
			}
		}
	}

	var out strings.Builder
	out.WriteString(strings.TrimRight(generated, "\n"))
	out.WriteString("\n\n")
	out.WriteString(generatedFence)
	if human != "" {
		out.WriteString("\n")
		out.WriteString(human)
	}
	if !strings.HasSuffix(out.String(), "\n") {
		out.WriteString("\n")
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("vault: mkdir %s: %w", filepath.Dir(absPath), err)
	}
	return os.WriteFile(absPath, []byte(out.String()), 0o644)
}

// readAllMessages fetches all messages for a session, paginating until
// the backend returns fewer than pageSize results.
func readAllMessages(backend store.Backend, sessionID string) ([]store.SessionMessage, error) {
	const pageSize = 500
	var all []store.SessionMessage
	for offset := 0; ; offset += pageSize {
		page, err := backend.ReadSession(sessionID, "", offset, pageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
	}
	return all, nil
}
