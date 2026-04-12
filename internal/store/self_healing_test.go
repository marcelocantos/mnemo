// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSelfHealing_Targets_WorkspaceOnly verifies that a repo discovered
// purely through a workspace-root filesystem walk — with no session_meta
// row pointing at it — is still ingested for targets. Regression guard
// for 🎯T17: before the fix, IngestTargets enumerated repos from
// session_meta alone, silently dropping repos on disk that hadn't been
// touched by a Claude Code session.
func TestSelfHealing_Targets_WorkspaceOnly(t *testing.T) {
	workspaceRoot := t.TempDir()
	repoDir := filepath.Join(workspaceRoot, "github.com", "testorg", "widgets")
	mustMkdirAll(t, filepath.Join(repoDir, ".git"))
	mustMkdirAll(t, filepath.Join(repoDir, "docs"))
	mustWriteFile(t, filepath.Join(repoDir, "docs", "targets.md"), `# Targets

### 🎯T42 Widget factory discovered via workspace walker

- **Status**: identified
- **Weight**: 9

The daemon discovers this widget factory repository through the
configured workspace root rather than through a recorded session.
`)

	// Fresh store with an empty projects dir (no sessions).
	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	if err := s.IngestTargets(); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchTargets("widget factory", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected targets to be searchable via FTS after workspace-only ingest")
	}
	if results[0].TargetID != "🎯T42" {
		t.Errorf("expected 🎯T42, got %q", results[0].TargetID)
	}

	// Backfill status must be recorded.
	statuses := s.BackfillStatuses()
	found := false
	for _, st := range statuses {
		if st.Stream == "targets" {
			found = true
			if st.FilesIndexed != 1 || st.FilesOnDisk != 1 {
				t.Errorf("targets backfill: indexed=%d on_disk=%d, want 1/1", st.FilesIndexed, st.FilesOnDisk)
			}
			if st.LastBackfill == "" {
				t.Error("targets backfill last_backfill should not be empty")
			}
			if st.Drift() != 0 {
				t.Errorf("expected zero drift, got %d", st.Drift())
			}
		}
	}
	if !found {
		t.Error("targets stream missing from backfill statuses")
	}
}

// TestSelfHealing_AuditLogs_WorkspaceOnly — regression guard for the
// audit-log stream.
func TestSelfHealing_AuditLogs_WorkspaceOnly(t *testing.T) {
	workspaceRoot := t.TempDir()
	repoDir := filepath.Join(workspaceRoot, "github.com", "testorg", "auditrepo")
	mustMkdirAll(t, filepath.Join(repoDir, ".git"))
	mustMkdirAll(t, filepath.Join(repoDir, "docs"))
	mustWriteFile(t, filepath.Join(repoDir, "docs", "audit-log.md"), `# Audit Log

## 2026-04-12 — /release v0.99.0

- **Commit**: `+"`deadbeef`"+`
- **Outcome**: Shipped the widget factory backfill release.
`)

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	if err := s.IngestAuditLogs(); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchAuditLogs("widget factory", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected audit entry to be searchable after workspace-only ingest")
	}
	if results[0].Version != "v0.99.0" {
		t.Errorf("expected version v0.99.0, got %q", results[0].Version)
	}

	if !hasStream(s.BackfillStatuses(), "audit") {
		t.Error("audit stream missing from backfill statuses")
	}
}

// TestSelfHealing_Plans_WorkspaceOnly — regression guard for the plans
// stream.
func TestSelfHealing_Plans_WorkspaceOnly(t *testing.T) {
	workspaceRoot := t.TempDir()
	repoDir := filepath.Join(workspaceRoot, "github.com", "testorg", "planrepo")
	mustMkdirAll(t, filepath.Join(repoDir, ".git"))
	mustMkdirAll(t, filepath.Join(repoDir, ".planning", "phase-1"))
	mustWriteFile(t, filepath.Join(repoDir, ".planning", "phase-1", "PLAN.md"),
		"# Phase 1\n\nImplement the teleporter using quantum entanglement.\n")

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	if err := s.IngestPlans(); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchPlans("quantum entanglement", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected plan to be searchable after workspace-only ingest")
	}
	if results[0].Phase != "1" {
		t.Errorf("expected phase '1', got %q", results[0].Phase)
	}

	if !hasStream(s.BackfillStatuses(), "plans") {
		t.Error("plans stream missing from backfill statuses")
	}
}

// TestSelfHealing_ClaudeConfigs_WorkspaceOnly — regression guard for
// the claude_configs stream.
func TestSelfHealing_ClaudeConfigs_WorkspaceOnly(t *testing.T) {
	workspaceRoot := t.TempDir()
	repoDir := filepath.Join(workspaceRoot, "github.com", "testorg", "cfgrepo")
	mustMkdirAll(t, filepath.Join(repoDir, ".git"))
	mustWriteFile(t, filepath.Join(repoDir, "CLAUDE.md"),
		"# cfgrepo\n\n## Delivery\n\nMerged to master via squash PR.\n")

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	if err := s.IngestClaudeConfigs(); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchClaudeConfigs("squash", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected claude config to be searchable after workspace-only ingest")
	}

	if !hasStream(s.BackfillStatuses(), "claude_configs") {
		t.Error("claude_configs stream missing from backfill statuses")
	}
}

// TestSelfHealing_Union verifies that workspace-root and session_meta
// discovery sources are merged — a repo reached only via session_meta
// remains reachable even when workspace roots are configured.
func TestSelfHealing_Union(t *testing.T) {
	workspaceRoot := t.TempDir()
	wsRepo := filepath.Join(workspaceRoot, "github.com", "testorg", "wsrepo")
	mustMkdirAll(t, filepath.Join(wsRepo, ".git"))
	mustMkdirAll(t, filepath.Join(wsRepo, "docs"))
	mustWriteFile(t, filepath.Join(wsRepo, "docs", "targets.md"),
		`### 🎯T1 workspace-discovered target

- **Status**: identified
- **Weight**: 5

Reached via the workspace walker.
`)

	// A second repo outside the workspace root — only reachable via
	// session_meta.cwd.
	outsideRoot := t.TempDir()
	outsideRepo := filepath.Join(outsideRoot, "somewhere", "elsewhere")
	mustMkdirAll(t, filepath.Join(outsideRepo, ".git"))
	mustMkdirAll(t, filepath.Join(outsideRepo, "docs"))
	mustWriteFile(t, filepath.Join(outsideRepo, "docs", "targets.md"),
		`### 🎯T2 session-discovered target

- **Status**: identified
- **Weight**: 5

Reached via session_meta only.
`)

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	// Seed session_meta with a cwd pointing at the outside repo.
	if _, err := s.db.Exec(
		"INSERT INTO session_meta (session_id, cwd) VALUES (?, ?)",
		"sess-outside", outsideRepo,
	); err != nil {
		t.Fatal(err)
	}

	if err := s.IngestTargets(); err != nil {
		t.Fatal(err)
	}

	// Both targets must land in the index.
	all, err := s.SearchTargets("", "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	foundWS, foundSess := false, false
	for _, r := range all {
		switch r.TargetID {
		case "🎯T1":
			foundWS = true
		case "🎯T2":
			foundSess = true
		}
	}
	if !foundWS {
		t.Error("workspace-discovered target missing after union ingest")
	}
	if !foundSess {
		t.Error("session-discovered target missing after union ingest")
	}
}

// TestDiscoverRepos_SkipsNoiseDirs verifies that the workspace walker
// skips well-known noise directories like node_modules rather than
// descending into them. Guards against pathological walks on big
// workspaces.
func TestDiscoverRepos_SkipsNoiseDirs(t *testing.T) {
	root := t.TempDir()
	// Real repo.
	realRepo := filepath.Join(root, "github.com", "testorg", "real")
	mustMkdirAll(t, filepath.Join(realRepo, ".git"))

	// A node_modules sibling with a fake .git inside — should not be
	// reported as a repo because the walker skips node_modules.
	fake := filepath.Join(root, "node_modules", "bogus")
	mustMkdirAll(t, filepath.Join(fake, ".git"))

	got := discoverRepos([]string{root})

	// Expect exactly one repo (the real one).
	if len(got) != 1 {
		t.Fatalf("expected 1 repo, got %d: %v", len(got), got)
	}
	if got[0] != realRepo {
		t.Errorf("expected %q, got %q", realRepo, got[0])
	}
}

// TestDiscoverRepos_MissingRoot is silent when a workspace root doesn't
// exist — mnemo must not crash if the user misconfigures a root.
func TestDiscoverRepos_MissingRoot(t *testing.T) {
	got := discoverRepos([]string{"/nonexistent/path/that/will/never/exist"})
	if got != nil {
		t.Errorf("expected nil for missing root, got %v", got)
	}
}

// TestConfig_DefaultsWhenEmpty covers the ResolvedWorkspaceRoots
// fallback path.
func TestConfig_DefaultsWhenEmpty(t *testing.T) {
	cfg := Config{}
	roots := cfg.ResolvedWorkspaceRoots()
	if len(roots) == 0 {
		t.Error("expected at least one default workspace root")
	}
}

// TestStats_ExposesBackfillStreams verifies that Stats() surfaces the
// per-stream backfill status recorded by IngestTargets et al. The
// Streams field is populated by an inlined SELECT against
// ingest_status — without this test, a typo in that query (or a
// schema change that drops the table) would silently produce empty
// output and no existing assertion would catch it.
func TestStats_ExposesBackfillStreams(t *testing.T) {
	workspaceRoot := t.TempDir()

	// Two repos with distinct artefacts so we can assert two streams.
	targetsRepo := filepath.Join(workspaceRoot, "github.com", "testorg", "trepo")
	mustMkdirAll(t, filepath.Join(targetsRepo, ".git"))
	mustMkdirAll(t, filepath.Join(targetsRepo, "docs"))
	mustWriteFile(t, filepath.Join(targetsRepo, "docs", "targets.md"),
		`### 🎯T1 stats exposure target

- **Status**: identified
- **Weight**: 5

Ensures Stats() surfaces this ingest.
`)

	auditRepo := filepath.Join(workspaceRoot, "github.com", "testorg", "arepo")
	mustMkdirAll(t, filepath.Join(auditRepo, ".git"))
	mustMkdirAll(t, filepath.Join(auditRepo, "docs"))
	mustWriteFile(t, filepath.Join(auditRepo, "docs", "audit-log.md"),
		`## 2026-04-12 — /release v1.0.0

- **Commit**: `+"`cafef00d`"+`
- **Outcome**: Shipped stats exposure test fixture.
`)

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	if err := s.IngestTargets(); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestAuditLogs(); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if !hasStream(stats.Streams, "targets") {
		t.Error("Stats().Streams missing 'targets' entry")
	}
	if !hasStream(stats.Streams, "audit") {
		t.Error("Stats().Streams missing 'audit' entry")
	}
	// Pin the counts for at least one stream.
	for _, st := range stats.Streams {
		if st.Stream == "targets" {
			if st.FilesIndexed != 1 || st.FilesOnDisk != 1 {
				t.Errorf("Stats targets: indexed=%d on_disk=%d, want 1/1", st.FilesIndexed, st.FilesOnDisk)
			}
			if st.LastBackfill == "" {
				t.Error("Stats targets: LastBackfill should be populated")
			}
		}
	}
}

// TestStatus_ExposesBackfillStreams — same guard for Status(), which
// has its own separate inlined SELECT against ingest_status. The two
// queries could drift independently, so each surface needs its own
// regression test.
func TestStatus_ExposesBackfillStreams(t *testing.T) {
	workspaceRoot := t.TempDir()
	repoDir := filepath.Join(workspaceRoot, "github.com", "testorg", "statusrepo")
	mustMkdirAll(t, filepath.Join(repoDir, ".git"))
	mustMkdirAll(t, filepath.Join(repoDir, "docs"))
	mustWriteFile(t, filepath.Join(repoDir, "docs", "targets.md"),
		`### 🎯T1 status exposure target

- **Status**: identified
- **Weight**: 3

Ensures Status() surfaces this ingest.
`)

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	if err := s.IngestTargets(); err != nil {
		t.Fatal(err)
	}

	// Status() has default values for all its size params; 0 maps to
	// the documented default inside Status itself.
	result, err := s.Status(0, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasStream(result.Streams, "targets") {
		t.Errorf("Status().Streams missing 'targets' entry; got %+v", result.Streams)
	}
	for _, st := range result.Streams {
		if st.Stream == "targets" {
			if st.FilesIndexed != 1 || st.FilesOnDisk != 1 {
				t.Errorf("Status targets: indexed=%d on_disk=%d, want 1/1", st.FilesIndexed, st.FilesOnDisk)
			}
		}
	}
}

// TestDrift_SurfacedWhenFileIsUnparseable is a regression guard for
// the drift-surfacing observability feature: when an artefact exists
// on disk but fails to contribute any rows to the index, BackfillStatus
// must show files_on_disk > files_indexed so the agent can see the
// discrepancy at a glance. A silently-ignored parse failure (or a
// parser that drops 90% of its input) should not look like full
// coverage.
func TestDrift_SurfacedWhenFileIsUnparseable(t *testing.T) {
	workspaceRoot := t.TempDir()

	// Good repo — contributes one indexed target.
	goodRepo := filepath.Join(workspaceRoot, "github.com", "testorg", "goodrepo")
	mustMkdirAll(t, filepath.Join(goodRepo, ".git"))
	mustMkdirAll(t, filepath.Join(goodRepo, "docs"))
	mustWriteFile(t, filepath.Join(goodRepo, "docs", "targets.md"),
		`### 🎯T1 parses correctly

- **Status**: identified
- **Weight**: 3

Produces a parseable target row.
`)

	// Bad repo — file exists (so files_on_disk increments) but has no
	// recognisable target headings, so parseTargetsFile returns empty
	// and files_indexed does NOT increment. This produces drift of 1.
	badRepo := filepath.Join(workspaceRoot, "github.com", "testorg", "badrepo")
	mustMkdirAll(t, filepath.Join(badRepo, ".git"))
	mustMkdirAll(t, filepath.Join(badRepo, "docs"))
	mustWriteFile(t, filepath.Join(badRepo, "docs", "targets.md"),
		`# Targets

This file is well-formed markdown but contains zero target headings.
The parser should produce no rows, and the drift count should rise
accordingly.
`)

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workspaceRoot})

	if err := s.IngestTargets(); err != nil {
		t.Fatal(err)
	}

	statuses := s.BackfillStatuses()
	var targets *BackfillStatus
	for i := range statuses {
		if statuses[i].Stream == "targets" {
			targets = &statuses[i]
			break
		}
	}
	if targets == nil {
		t.Fatal("targets stream missing from BackfillStatuses")
	}
	if targets.FilesOnDisk != 2 {
		t.Errorf("FilesOnDisk = %d, want 2 (both repos have a targets.md on disk)", targets.FilesOnDisk)
	}
	if targets.FilesIndexed != 1 {
		t.Errorf("FilesIndexed = %d, want 1 (only goodrepo produces rows)", targets.FilesIndexed)
	}
	if targets.Drift() != 1 {
		t.Errorf("Drift = %d, want 1", targets.Drift())
	}
}

// TestLoadConfig_MissingFile verifies that a missing ~/.mnemo/config.json
// returns a zero Config without error. This is the critical no-config-
// present path — startup must not fail just because the user hasn't
// created the file.
func TestLoadConfig_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	cfg, err := loadConfigFrom(filepath.Join(tmp, "nonexistent.json"))
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if len(cfg.WorkspaceRoots) != 0 {
		t.Errorf("expected empty WorkspaceRoots, got %v", cfg.WorkspaceRoots)
	}
	if len(cfg.ExtraProjectDirs) != 0 {
		t.Errorf("expected empty ExtraProjectDirs, got %v", cfg.ExtraProjectDirs)
	}
}

// TestLoadConfig_ValidJSON verifies the happy path: a well-formed
// config file is parsed and its fields are preserved.
func TestLoadConfig_ValidJSON(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	mustWriteFile(t, path, `{
		"workspace_roots": ["/path/one", "/path/two"],
		"extra_project_dirs": ["/extra/dir"]
	}`)
	cfg, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(cfg.WorkspaceRoots) != 2 || cfg.WorkspaceRoots[0] != "/path/one" || cfg.WorkspaceRoots[1] != "/path/two" {
		t.Errorf("WorkspaceRoots = %v, want [/path/one /path/two]", cfg.WorkspaceRoots)
	}
	if len(cfg.ExtraProjectDirs) != 1 || cfg.ExtraProjectDirs[0] != "/extra/dir" {
		t.Errorf("ExtraProjectDirs = %v, want [/extra/dir]", cfg.ExtraProjectDirs)
	}
	// ResolvedWorkspaceRoots should return the explicit list (no
	// fallback to the default).
	got := cfg.ResolvedWorkspaceRoots()
	if len(got) != 2 || got[0] != "/path/one" {
		t.Errorf("ResolvedWorkspaceRoots = %v, want explicit list", got)
	}
}

// TestLoadConfig_MalformedJSON verifies that a broken config file
// surfaces a parse error rather than silently returning a zero
// Config. This distinction matters: a missing file is "user hasn't
// configured yet" (no error); a broken file is "user made a mistake"
// (error so main.go can log and fall back).
func TestLoadConfig_MalformedJSON(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	mustWriteFile(t, path, `{"workspace_roots": [this is not valid JSON}`)
	_, err := loadConfigFrom(path)
	if err == nil {
		t.Error("expected parse error for malformed JSON, got nil")
	}
}

// TestCIRepos_UnionOfWorkspaceAndSessionMeta verifies that ciRepos
// returns the union of (a) GitHub repos discovered via the workspace
// walker and (b) repos known through session_meta.repo. This matters
// for CI polling — before 🎯T17, ciRepos only looked at session_meta,
// so a brand-new project that hadn't been touched by a Claude Code
// session couldn't have its CI runs polled.
func TestCIRepos_UnionOfWorkspaceAndSessionMeta(t *testing.T) {
	workspaceRoot := t.TempDir()

	// Workspace-side: a github.com repo the walker will discover.
	// extractRepo() uses a regex that matches /work/github.com/org/repo,
	// so the path has to contain /work/ for the walker's repo-name
	// derivation to produce an org/repo pair. The tempdir root may or
	// may not contain /work/; mirror that shape under the root so the
	// assertion is deterministic regardless.
	workRoot := filepath.Join(workspaceRoot, "work")
	wsRepoDir := filepath.Join(workRoot, "github.com", "walkerorg", "walkerrepo")
	mustMkdirAll(t, filepath.Join(wsRepoDir, ".git"))

	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	s.SetWorkspaceRoots([]string{workRoot})

	// Session_meta-side: a separate org/repo that is NOT on disk
	// anywhere under the workspace root. ciRepos must still surface
	// it from the session_meta.repo column fallback.
	if _, err := s.db.Exec(
		"INSERT INTO session_meta (session_id, repo) VALUES (?, ?)",
		"sess-ci-fallback", "sessionorg/sessionrepo",
	); err != nil {
		t.Fatal(err)
	}

	repos, err := s.ciRepos()
	if err != nil {
		t.Fatalf("ciRepos failed: %v", err)
	}

	foundWalker, foundSession := false, false
	for _, r := range repos {
		if r == "walkerorg/walkerrepo" {
			foundWalker = true
		}
		if r == "sessionorg/sessionrepo" {
			foundSession = true
		}
	}
	if !foundWalker {
		t.Errorf("ciRepos missing workspace-walked repo 'walkerorg/walkerrepo'; got %v", repos)
	}
	if !foundSession {
		t.Errorf("ciRepos missing session_meta fallback repo 'sessionorg/sessionrepo'; got %v", repos)
	}

	// Non-GitHub paths (no slash) must be filtered out. Insert one
	// and re-run to pin the behaviour.
	if _, err := s.db.Exec(
		"INSERT INTO session_meta (session_id, repo) VALUES (?, ?)",
		"sess-bare", "bare-name-no-slash",
	); err != nil {
		t.Fatal(err)
	}
	repos, err = s.ciRepos()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if r == "bare-name-no-slash" {
			t.Errorf("ciRepos returned non-GitHub bare name %q", r)
		}
	}
}

// TestSetWorkspaceRoots_NilClears verifies that passing nil (or an
// empty slice) to SetWorkspaceRoots clears the field, disabling the
// workspace walk entirely. This is the explicit opt-out for tests
// and callers that want session_meta-only behaviour — it must not
// silently fall back to DefaultWorkspaceRoots, which would walk
// ~/work and pick up every repo on the machine.
func TestSetWorkspaceRoots_NilClears(t *testing.T) {
	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)

	// Set something, then clear with nil.
	s.SetWorkspaceRoots([]string{"/tmp/one", "/tmp/two"})
	s.SetWorkspaceRoots(nil)

	// With no workspace roots and no session_meta, knownRepoRoots
	// must return an empty slice — not walk any default directory.
	roots := s.knownRepoRoots()
	if len(roots) != 0 {
		t.Errorf("expected zero repos after SetWorkspaceRoots(nil), got %v", roots)
	}

	// Empty slice should behave identically to nil.
	s.SetWorkspaceRoots([]string{"/tmp/one"})
	s.SetWorkspaceRoots([]string{})
	roots = s.knownRepoRoots()
	if len(roots) != 0 {
		t.Errorf("expected zero repos after SetWorkspaceRoots([]), got %v", roots)
	}
}

// TestDiscoverRepos_GitWorktreeFile verifies that a directory whose
// `.git` is a FILE (the shape git worktrees use — the file contains
// a `gitdir:` pointer to the real .git dir elsewhere) is still
// detected as a repo root. The walker uses os.Stat, which doesn't
// distinguish file from dir, so this should just work — but it's
// worth pinning because git worktrees are common and a future
// "re-check with os.Stat + is_dir()" refactor would silently break
// them.
func TestDiscoverRepos_GitWorktreeFile(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "worktree-style")
	mustMkdirAll(t, repoDir)
	// .git is a FILE here, not a directory.
	mustWriteFile(t, filepath.Join(repoDir, ".git"),
		"gitdir: /somewhere/else/.git/worktrees/mybranch\n")

	got := discoverRepos([]string{root})
	if len(got) != 1 {
		t.Fatalf("expected 1 repo, got %d: %v", len(got), got)
	}
	if got[0] != repoDir {
		t.Errorf("expected %q, got %q", repoDir, got[0])
	}
}

// --- test helpers ---

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasStream(statuses []BackfillStatus, stream string) bool {
	for _, s := range statuses {
		if s.Stream == stream {
			return true
		}
	}
	return false
}
