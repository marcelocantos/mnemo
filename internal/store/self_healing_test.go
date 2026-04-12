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
