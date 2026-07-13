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
// and a dedicated IngestVaultAnnotations pass re-indexes the content
// below the <!-- mnemo:generated --> fence so human-added annotations
// appear in mnemo_search results alongside transcript messages.
//
// Human edits are preserved across re-syncs: generated content lives
// above the fence, human notes live below and are never overwritten.
//
// Vault structure (layout-dependent; vault_layout = v1 | both | v2):
//
//	v1 / both root (raw signal; suppressed under pure v2 — 🎯T64.4):
//	  sessions/, repos/, ci/, prs/, decisions/, memories/
//	v2 / both wing (🎯T64.2–T64.3):
//	  _mnemo/{index,README,MIGRATION}.md
//	  _mnemo/decisions/, _mnemo/memories/  (+ later: patterns, themes, …)
//	Always (all layouts):
//	  index.md, plans/, targets/, skills/, configs/
package vault

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// ErrSyncInFlight is returned by Sync when another Sync is already running.
// Periodic and initial-sync callers can ignore this; the human-triggered MCP
// path checks for it to report honestly that the call did nothing rather
// than falsely claiming a successful 0s sync.
var ErrSyncInFlight = errors.New("vault: sync already in flight")

// generatedFence separates mnemo-generated content (above the line) from
// human-added content (below the line). Re-syncing rewrites everything
// above this marker while leaving everything below untouched.
const generatedFence = "<!-- mnemo:generated -->"

// fenceLineIndex returns the byte offset of the line break ending the LAST
// line that exactly equals the generated fence (modulo trailing whitespace),
// or -1 when no such line exists.
//
// Line-anchored matching avoids a subtle data-loss bug: a plain LastIndex
// of the fence string would also match if the user pasted the literal fence
// into their annotations (e.g. quoting mnemo's docs). Detecting the fence
// only on its own line keeps user-typed instances of the string safely
// inside human content.
func fenceLineIndex(raw string) int {
	end := len(raw)
	// Walk backwards line by line.
	for end > 0 {
		start := strings.LastIndexByte(raw[:end], '\n') + 1 // 0 if no earlier newline
		line := raw[start:end]
		// Strip a trailing CR + spaces/tabs (but not the leading content).
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == generatedFence {
			return end // offset of (or just past) the line's terminating newline
		}
		if start == 0 {
			break
		}
		end = start - 1 // step over the '\n'
	}
	return -1
}

// Exporter writes mnemo's knowledge graph as Markdown vault notes.
//
// Sync calls are serialised via syncMu: if a Sync is already running when
// another is invoked, the second returns immediately without rerunning.
// This prevents the periodic Sync ticker from racing the initial Sync on
// large vaults and protects writeNote's read-modify-write of fenced files.
type Exporter struct {
	backend store.Backend
	path    string
	syncMu  sync.Mutex
	syncing bool

	// layout is the active vault layout — "v1", "both", or "v2". Empty
	// is treated as "v2". The wing writers (_mnemo/index.md etc.) run
	// for "both" and "v2"; "v1" suppresses them. (🎯T64.2)
	layout string

	// soakWarn is the duration past which a "both"-layout vault gets
	// the weekly "opt into v2" warning. Zero means use the default
	// (720h). (🎯T64.2)
	soakWarn time.Duration

	// statePath is the absolute path to the daemon-managed
	// state.json sidecar. Empty falls back to ~/.mnemo/state.json.
	// Injectable for tests. (🎯T64.2)
	statePath string
}

// Options carries optional Exporter wiring. Each zero-valued field
// triggers a documented default. Passing Options{} is equivalent to
// the pre-🎯T64.2 New(backend, path) shape.
type Options struct {
	// Layout selects the active vault_layout. Empty defaults to "v2".
	// Use store.ResolvedVaultLayout to compute the right value from
	// the current Config + on-disk vault shape.
	Layout string

	// SoakWarnAfter is the duration past which a "both"-layout vault
	// gets the weekly soak warning. Zero defaults to 720h.
	SoakWarnAfter time.Duration

	// StatePath overrides the ~/.mnemo/state.json sidecar location.
	// Tests pass a tempdir path; production wiring leaves this empty.
	StatePath string
}

// New creates a new Exporter rooted at path. The directory is created if
// it does not exist. path must already be ~ expanded (use
// Config.ResolvedVaultPath).
func New(backend store.Backend, path string, opts Options) (*Exporter, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("vault: create root %s: %w", path, err)
	}
	return &Exporter{
		backend:   backend,
		path:      path,
		layout:    opts.Layout,
		soakWarn:  opts.SoakWarnAfter,
		statePath: opts.StatePath,
	}, nil
}

// Path returns the vault root directory.
func (e *Exporter) Path() string { return e.path }

// Sync performs a full vault synchronisation: sessions, decisions, memories,
// plans, targets, CI runs, PRs, per-repo indices, and the root index.
//
// Concurrent calls are coalesced: if a Sync is already in flight, the second
// call returns ErrSyncInFlight immediately. The other in-flight goroutine's
// completion is unrelated to the caller's request, so we surface the skip
// rather than falsely report success.
//
// Session notes whose recorded entity timestamp matches the session's last
// message timestamp are skipped so that repeated syncs are fast after the
// initial run.
func (e *Exporter) Sync(ctx context.Context) error {
	e.syncMu.Lock()
	if e.syncing {
		e.syncMu.Unlock()
		slog.Info("vault: sync already in flight; skipping")
		return ErrSyncInFlight
	}
	e.syncing = true
	e.syncMu.Unlock()
	defer func() {
		e.syncMu.Lock()
		e.syncing = false
		e.syncMu.Unlock()
	}()

	slog.Info("vault: sync starting", "path", e.path)
	start := time.Now()

	layout := e.effectiveLayout()
	rawSignal := writesRawSignalReports(layout)

	var firstErr error
	setErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Sessions (raw signal) first when layout allows: the path map is
	// used for decision back-links and repo indices. Under pure v2 the
	// pages are not written (🎯T64.4); decision notes then cite session
	// IDs only.
	var sessionPaths map[string]string
	if rawSignal {
		paths, err := e.syncSessions(ctx)
		setErr(err)
		sessionPaths = paths
	} else {
		sessionPaths = map[string]string{}
	}

	// Decisions + memories: layout-selected paths (v1 and/or _mnemo/). (🎯T64.3)
	setErr(e.syncDecisions(ctx, sessionPaths, layout))
	setErr(e.syncMemories(ctx, layout))

	setErr(e.syncPlans(ctx))
	setErr(e.syncTargets(ctx))
	// CI / PR / repo indices are raw signal — suppress under pure v2. (🎯T64.4)
	if rawSignal {
		setErr(e.syncCI(ctx))
		setErr(e.syncPRs(ctx))
	}
	setErr(e.syncSkills(ctx))
	setErr(e.syncConfigs(ctx))
	repos, err := e.backend.ListRepos("")
	setErr(err)
	if rawSignal {
		setErr(e.syncRepoIndices(ctx, repos, sessionPaths))
	}
	// Root index is a thin rollup (not listed as raw-signal noise in T64.4);
	// keep writing under every layout so the vault always has an entry point.
	setErr(e.syncRootIndex(repos))
	// Library-wing (_mnemo/) index/README/MIGRATION + state.json + soak. (🎯T64.2)
	e.syncMnemoWing(ctx, time.Now())

	slog.Info("vault: sync complete",
		"elapsed", time.Since(start).Round(time.Millisecond),
		"layout", layout,
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

		content := renderSession(s, msgs)
		if err := writeNote(absPath, content, s.LastMsg); err != nil {
			slog.Warn("vault: write session note failed", "path", absPath, "err", err)
			continue
		}
		// 🎯T68.6 vault GC manifest: record the (entity, path, hash)
		// tuple so orphan detection is exact set-difference, not lossy
		// slug reverse-mapping. Failures here are logged but do not
		// fail the sync — the manifest is auxiliary state.
		if err := e.backend.RecordVaultOutput(
			filepath.ToSlash(relPath), "session", s.SessionID,
			store.HashVaultContent(content), time.Now().UTC(),
		); err != nil {
			slog.Warn("vault: record session output failed",
				"session", shortID(s.SessionID), "err", err)
		}
		written++
	}

	slog.Info("vault: sessions synced", "written", written, "skipped", skipped)
	return pathMap, nil
}

// syncDecisions writes vault notes for every detected decision. Paths
// depend on layout: v1 root, _mnemo/ wing, or both (🎯T64.3 dual-write).
func (e *Exporter) syncDecisions(ctx context.Context, sessionPaths map[string]string, layout string) error {
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
		content := renderDecision(d, sessionPaths[d.SessionID])
		for _, relPath := range decisionPathsForLayout(layout, d) {
			absPath := filepath.Join(e.path, relPath)
			if !needsUpdate(absPath, d.Timestamp) {
				skipped++
				continue
			}
			if err := writeNote(absPath, content, d.Timestamp); err != nil {
				slog.Warn("vault: write decision note failed", "path", absPath, "err", err)
				continue
			}
			e.recordOutput(relPath, "decision", d.SessionID+":"+d.Timestamp, content)
			written++
		}
	}

	slog.Info("vault: decisions synced", "written", written, "skipped", skipped, "layout", layout)
	return nil
}

// syncMemories writes vault notes for every indexed memory file.
// Paths depend on layout (🎯T64.3 dual-write under "both").
func (e *Exporter) syncMemories(ctx context.Context, layout string) error {
	memories, err := e.backend.SearchMemories("", "", "", 100000)
	if err != nil {
		return fmt.Errorf("vault: search memories: %w", err)
	}

	written, skipped := 0, 0
	for _, m := range memories {
		if ctx.Err() != nil {
			break
		}
		content := renderMemory(m)
		for _, relPath := range memoryPathsForLayout(layout, m) {
			absPath := filepath.Join(e.path, relPath)
			if !needsUpdate(absPath, m.UpdatedAt) {
				skipped++
				continue
			}
			if err := writeNote(absPath, content, m.UpdatedAt); err != nil {
				slog.Warn("vault: write memory note failed", "path", absPath, "err", err)
				continue
			}
			e.recordOutput(relPath, "memory", m.Project+":"+m.Name, content)
			written++
		}
	}

	slog.Info("vault: memories synced", "written", written, "skipped", skipped, "layout", layout)
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
		relPath := planPath(p)
		absPath := filepath.Join(e.path, relPath)
		if !needsUpdate(absPath, p.UpdatedAt) {
			skipped++
			continue
		}
		content := renderPlan(p)
		if err := writeNote(absPath, content, p.UpdatedAt); err != nil {
			slog.Warn("vault: write plan note failed", "path", absPath, "err", err)
			continue
		}
		e.recordOutput(relPath, "plan", p.FilePath, content)
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
		relPath := targetPath(t)
		absPath := filepath.Join(e.path, relPath)
		// Targets carry no reliable last-modified timestamp. Notes are
		// written once and not refreshed until the file is deleted.
		// targetPath encodes the target ID, not the status, so a status
		// change does not rename the file — human annotations survive.
		if _, err := os.Stat(absPath); err == nil {
			skipped++
			continue
		}
		content := renderTarget(t)
		if err := writeNote(absPath, content, ""); err != nil {
			slog.Warn("vault: write target note failed", "path", absPath, "err", err)
			continue
		}
		e.recordOutput(relPath, "target", t.Repo+":"+t.TargetID, content)
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
		relPath := ciRunPath(r)
		absPath := filepath.Join(e.path, relPath)
		if !needsUpdate(absPath, r.CompletedAt) {
			skipped++
			continue
		}
		content := renderCIRun(r)
		if err := writeNote(absPath, content, r.CompletedAt); err != nil {
			slog.Warn("vault: write CI run note failed", "path", absPath, "err", err)
			continue
		}
		e.recordOutput(relPath, "ci_run", r.Repo+":"+strconv.FormatInt(r.RunID, 10), content)
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
		relPath := prPath(r)
		absPath := filepath.Join(e.path, relPath)
		if !needsUpdate(absPath, r.UpdatedAt) {
			skipped++
			continue
		}
		content := renderPR(r)
		if err := writeNote(absPath, content, r.UpdatedAt); err != nil {
			slog.Warn("vault: write PR note failed", "path", absPath, "err", err)
			continue
		}
		e.recordOutput(relPath, r.Type, r.Repo+":"+strconv.Itoa(r.Number), content)
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
		relPath := repoIndexPath(repo.Repo)
		absPath := filepath.Join(e.path, relPath)
		if err := writeNote(absPath, content, ""); err != nil {
			slog.Warn("vault: write repo index failed", "repo", repo.Repo, "err", err)
			continue
		}
		e.recordOutput(relPath, "repo_index", repo.Repo, content)
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
		relPath := skillPath(s)
		absPath := filepath.Join(e.path, relPath)
		if !needsUpdate(absPath, s.UpdatedAt) {
			skipped++
			continue
		}
		content := renderSkill(s)
		if err := writeNote(absPath, content, s.UpdatedAt); err != nil {
			slog.Warn("vault: write skill note failed", "path", absPath, "err", err)
			continue
		}
		e.recordOutput(relPath, "skill", s.Name, content)
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
		relPath := configPath(c)
		absPath := filepath.Join(e.path, relPath)
		if !needsUpdate(absPath, c.UpdatedAt) {
			skipped++
			continue
		}
		content := renderConfig(c)
		if err := writeNote(absPath, content, c.UpdatedAt); err != nil {
			slog.Warn("vault: write config note failed", "path", absPath, "err", err)
			continue
		}
		repoID := c.Repo
		if repoID == "" {
			repoID = "global"
		}
		e.recordOutput(relPath, "claude_config", repoID, content)
		written++
	}

	slog.Info("vault: configs synced", "written", written, "skipped", skipped)
	return nil
}

// syncRootIndex writes the vault root index.md.
func (e *Exporter) syncRootIndex(repos []store.RepoInfo) error {
	stats, _ := e.backend.Stats()
	content := renderRootIndex(repos, stats)
	if err := writeNote(filepath.Join(e.path, "index.md"), content, ""); err != nil {
		return err
	}
	e.recordOutput("index.md", "root_index", "root", content)
	return nil
}

// recordOutput UPSERTs a vault_outputs manifest row for the note that
// was just written (🎯T68.6). The manifest is auxiliary state — a
// failure to record is logged but never fails the sync.
func (e *Exporter) recordOutput(relPath, kind, id, content string) {
	if err := e.backend.RecordVaultOutput(
		filepath.ToSlash(relPath), kind, id,
		store.HashVaultContent(content), time.Now().UTC(),
	); err != nil {
		slog.Warn("vault: record output failed",
			"path", relPath, "kind", kind, "id", id, "err", err)
	}
}

// entityTSComment is the HTML comment prefix written just before the
// generatedFence to record the entity timestamp used during the last write.
// needsUpdate reads this back to detect staleness without relying on file
// mtime (which human editors bump on every save).
const entityTSComment = "<!-- mnemo:entity_ts "

// entityTSCommentSuffix closes the entity-timestamp HTML comment.
const entityTSCommentSuffix = " -->"

// parseEntityTS returns the recorded entity timestamp from raw, scanning
// from the end for a line that exactly matches the entity-timestamp comment
// pattern. Returns ("", false) when no such line exists.
//
// Line-anchored matching mirrors fenceLineIndex: a plain LastIndex would
// also match if the user pasted the literal entityTSComment prefix into
// their annotations (e.g. quoting mnemo's docs). Restricting to whole
// lines keeps user-typed instances of the string safely inside human
// content.
//
// The length guard is required because entityTSComment ends with a space
// and entityTSCommentSuffix starts with one — for the degenerate input
// "<!-- mnemo:entity_ts -->" the prefix and suffix share that space, so
// HasPrefix+HasSuffix both pass but the substring between them is
// negative-length. Without the guard, slicing would panic.
func parseEntityTS(raw string) (string, bool) {
	end := len(raw)
	for end > 0 {
		start := strings.LastIndexByte(raw[:end], '\n') + 1
		line := raw[start:end]
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.HasPrefix(trimmed, entityTSComment) &&
			strings.HasSuffix(trimmed, entityTSCommentSuffix) &&
			len(trimmed) >= len(entityTSComment)+len(entityTSCommentSuffix) {
			ts := trimmed[len(entityTSComment) : len(trimmed)-len(entityTSCommentSuffix)]
			return ts, true
		}
		if start == 0 {
			break
		}
		end = start - 1
	}
	return "", false
}

// needsUpdate returns true when the vault file at absPath does not exist or
// the entity timestamp recorded in its fence comment is older than ts.
// An empty ts always returns true (no reliable timestamp → always regenerate).
//
// The entity timestamp is embedded as an HTML comment just before the
// generatedFence marker on every writeNote call. For files that pre-date
// this mechanism (no comment present) needsUpdate falls back to mtime
// comparison so they are regenerated once and then carry the new marker.
func needsUpdate(absPath, ts string) bool {
	if ts == "" {
		return true
	}
	var entityTime time.Time
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, ts); err == nil {
			entityTime = parsed
			break
		}
	}
	if entityTime.IsZero() {
		return true // unparseable timestamp → always regenerate
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return true // file absent or unreadable
	}

	raw := string(data)
	tsStr, ok := parseEntityTS(raw)
	if !ok {
		// No entity timestamp recorded — fall back to mtime so existing
		// vault files are regenerated once and then carry the new marker.
		info, err := os.Stat(absPath)
		if err != nil {
			return true
		}
		return info.ModTime().Before(entityTime)
	}
	var fileTime time.Time
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, tsStr); err == nil {
			fileTime = parsed
			break
		}
	}
	if fileTime.IsZero() {
		return true
	}
	return fileTime.Before(entityTime)
}

// atomicWriteFile writes data to absPath via a tempfile-fsync-rename
// sequence so that a daemon crash between write and rename never leaves
// a partial file at the destination. The temp file is created in the
// same directory as absPath so the rename is guaranteed same-filesystem.
func atomicWriteFile(absPath string, data []byte) error {
	dir := filepath.Dir(absPath)
	tmp, err := os.CreateTemp(dir, ".mnemo-*.tmp")
	if err != nil {
		return fmt.Errorf("vault: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("vault: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("vault: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("vault: close temp: %w", err)
	}
	if err := os.Rename(tmpName, absPath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("vault: rename temp: %w", err)
	}
	return nil
}

// writeNote writes generated content to absPath, preserving any
// human-added content that follows the generatedFence marker in an
// existing file. The fence is always written; human content (if any)
// is appended after it.
//
// entityTS, when non-empty, is written as an HTML comment just before
// the fence so needsUpdate can detect staleness without relying on
// file mtime (which human editors bump on every save).
//
// If the existing file is non-empty but contains no fence (e.g. a
// pre-existing user file at a colliding path), its entire content is
// treated as human content and preserved below the fence. If that
// preserved content begins with its own YAML frontmatter block
// (---\n…\n---\n), the block is repackaged as a fenced yaml code block
// under a "Preserved frontmatter" heading: Obsidian otherwise renders
// the second --- pair as literal text in the body. The original keys
// remain visible and copy-pasteable.
func writeNote(absPath, generated, entityTS string) error {
	// Harvest human content from the existing file, if any.
	human := ""
	if existing, err := os.ReadFile(absPath); err == nil {
		raw := string(existing)
		if idx := fenceLineIndex(raw); idx >= 0 {
			after := strings.TrimLeft(raw[idx:], "\n")
			if after != "" {
				human = after
			}
		} else if trimmed := strings.TrimSpace(raw); trimmed != "" {
			// Pre-existing file with no fence: treat entire content as
			// human content so we don't silently overwrite user files.
			human = repackagePreexistingContent(trimmed + "\n")
		}
	}

	var out strings.Builder
	out.WriteString(strings.TrimRight(generated, "\n"))
	out.WriteString("\n\n")
	if entityTS != "" {
		fmt.Fprintf(&out, "%s%s -->\n", entityTSComment, entityTS)
	}
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
	return atomicWriteFile(absPath, []byte(out.String()))
}

// repackagePreexistingContent prepares a pre-existing user file's content
// for placement below mnemo's generated fence. If the content begins with
// its own YAML frontmatter (---\n…\n---\n), the block is extracted and
// rewritten as a fenced yaml code block under a heading so that Obsidian
// (which only recognises frontmatter at the very top of a file) does not
// render the stray --- pair as literal text in the body.
//
// Content with no leading frontmatter is returned unchanged.
func repackagePreexistingContent(content string) string {
	body, rest, ok := extractLeadingFrontmatter(content)
	if !ok {
		return content
	}
	var b strings.Builder
	b.WriteString("## Preserved frontmatter\n\n")
	b.WriteString("*This file existed before mnemo took over the path; its original ")
	b.WriteString("YAML frontmatter is preserved below. Copy any keys you want into ")
	b.WriteString("mnemo's frontmatter above the fence, or delete this block.*\n\n")
	b.WriteString("```yaml\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n```\n\n")
	rest = strings.TrimLeft(rest, "\n")
	if rest != "" {
		b.WriteString(rest)
		if !strings.HasSuffix(rest, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// extractLeadingFrontmatter peels a leading YAML frontmatter block from s.
// Returns (body, rest, true) when s begins with "---\n<body>\n---\n" (the
// closing fence on its own line, trailing whitespace tolerated). Returns
// ("", s, false) when no leading frontmatter is present.
//
// Tolerates CRLF line endings on the opening "---" line for consistency
// with fenceLineIndex, which already strips trailing \r before comparing.
func extractLeadingFrontmatter(s string) (body, rest string, ok bool) {
	var after string
	switch {
	case strings.HasPrefix(s, "---\n"):
		after = s[len("---\n"):]
	case strings.HasPrefix(s, "---\r\n"):
		after = s[len("---\r\n"):]
	default:
		return "", s, false
	}
	i := 0
	for i < len(after) {
		nl := strings.IndexByte(after[i:], '\n')
		if nl < 0 {
			// Tolerate a closing "---" with no trailing newline at EOF.
			trimmed := strings.TrimRight(after[i:], " \t\r")
			if trimmed == "---" {
				return after[:i], "", true
			}
			return "", s, false
		}
		line := after[i : i+nl]
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "---" {
			return after[:i], after[i+nl+1:], true
		}
		i += nl + 1
	}
	return "", s, false
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
