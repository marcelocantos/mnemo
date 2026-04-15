// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupDocRepo creates a fake git repo with a .git dir and seeds session_meta
// so IngestDocs can discover the repo root via knownRepoRootsLocked.
func setupDocRepo(t *testing.T, s *Store, repoRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		"INSERT OR IGNORE INTO session_meta (session_id, cwd) VALUES (?, ?)",
		"sess-docs-seed", repoRoot,
	); err != nil {
		t.Fatal(err)
	}
}

func writeDoc(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDocIngestBasic verifies markdown files in docs/ are indexed with
// FTS search working.
func TestDocIngestBasic(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "myorg", "myrepo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	docsDir := filepath.Join(repoRoot, "docs")
	writeDoc(t, filepath.Join(docsDir, "architecture.md"),
		"# Architecture\n\nThis repo uses a layered architecture with a data access layer.\n")
	writeDoc(t, filepath.Join(docsDir, "api.md"),
		"# API Reference\n\nThe REST API exposes endpoints for user management.\n")

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT COUNT(*) AS cnt FROM docs")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, _ := rows[0]["cnt"].(int64); cnt != 2 {
		t.Fatalf("expected 2 docs, got %v", rows[0]["cnt"])
	}

	// FTS search.
	results, err := s.SearchDocs("layered architecture", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected FTS hit for 'layered architecture'")
	}
	if !strings.Contains(results[0].Content, "layered architecture") {
		t.Errorf("unexpected content: %q", results[0].Content)
	}
}

// TestDocIngestDedup verifies that when .md and .pdf share the same stem,
// only the .md is indexed.
func TestDocIngestDedup(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	docsDir := filepath.Join(repoRoot, "docs")
	// Write .md and matching .pdf stub (won't be extracted since pdftotext
	// would fail on a fake file, but the selection happens before extraction).
	writeDoc(t, filepath.Join(docsDir, "spec.md"),
		"# Spec\n\nMarkdown version wins over PDF.\n")
	// Create a non-PDF byte sequence (won't parse but tests dedup selection).
	if err := os.WriteFile(filepath.Join(docsDir, "spec.pdf"), []byte("%PDF-fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	// Only the .md should be indexed (pdf would fail extraction and be skipped,
	// but the selection already preferred .md so pdf is never attempted).
	rows, err := s.Query("SELECT kind, file_path FROM docs ORDER BY kind")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 doc (dedup), got %d: %v", len(rows), rows)
	}
	if kind, _ := rows[0]["kind"].(string); kind != "md" {
		t.Errorf("expected kind=md, got %q", kind)
	}
}

// TestDocIngestTxtVsPdf verifies that .txt beats .pdf when no .md exists.
func TestDocIngestTxtVsPdf(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	docsDir := filepath.Join(repoRoot, "docs")
	writeDoc(t, filepath.Join(docsDir, "notes.txt"),
		"Plain text notes about the project.\n")
	if err := os.WriteFile(filepath.Join(docsDir, "notes.pdf"), []byte("%PDF-fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT kind FROM docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(rows))
	}
	if kind, _ := rows[0]["kind"].(string); kind != "txt" {
		t.Errorf("expected kind=txt, got %q", kind)
	}
}

// TestDocIngestGitignore verifies that directories matching .gitignore patterns
// are not indexed.
func TestDocIngestGitignore(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	// Write .gitignore that excludes "generated/" dir.
	if err := os.WriteFile(filepath.Join(repoRoot, ".gitignore"), []byte("generated/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	docsDir := filepath.Join(repoRoot, "docs")
	writeDoc(t, filepath.Join(docsDir, "good.md"), "# Good Doc\n\nThis should be indexed.\n")
	// docs/generated/ should be skipped.
	writeDoc(t, filepath.Join(docsDir, "generated", "auto.md"),
		"# Auto Generated\n\nThis should NOT be indexed.\n")

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT file_path FROM docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 doc (gitignore excluded generated/), got %d: %v", len(rows), rows)
	}
	fp, _ := rows[0]["file_path"].(string)
	if !strings.HasSuffix(fp, "good.md") {
		t.Errorf("expected good.md, got %q", fp)
	}
}

// TestDocIngestIncremental verifies that re-ingesting unchanged files skips them.
func TestDocIngestIncremental(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	docsDir := filepath.Join(repoRoot, "docs")
	writeDoc(t, filepath.Join(docsDir, "design.md"), "# Design\n\nInitial design.\n")

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT indexed_at FROM docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(rows))
	}
	firstIndexedAt, _ := rows[0]["indexed_at"].(string)

	// Re-ingest without changing the file — should skip (same hash).
	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows2, err := s.Query("SELECT indexed_at FROM docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != 1 {
		t.Fatalf("expected 1 doc after re-ingest, got %d", len(rows2))
	}
	secondIndexedAt, _ := rows2[0]["indexed_at"].(string)

	// indexed_at should be unchanged since content hash matched.
	if firstIndexedAt != secondIndexedAt {
		t.Errorf("indexed_at changed on unchanged file: %q → %q", firstIndexedAt, secondIndexedAt)
	}

	// Now change the file and re-ingest — should update.
	writeDoc(t, filepath.Join(docsDir, "design.md"), "# Design\n\nUpdated design with new sections.\n")
	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchDocs("Updated design", "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected updated content to be searchable after re-ingest")
	}
}

// TestDocIngestRootReadme verifies that well-known root-level files
// (README.md, CHANGELOG.md) are indexed, but random root-level .md files
// are not.
func TestDocIngestRootReadme(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	writeDoc(t, filepath.Join(repoRoot, "README.md"), "# My Repo\n\nThis is the readme.\n")
	writeDoc(t, filepath.Join(repoRoot, "CHANGELOG.md"), "# Changelog\n\n## v0.1.0\nInitial release.\n")
	// A random file at repo root should NOT be indexed.
	writeDoc(t, filepath.Join(repoRoot, "random.md"), "# Random\n\nShouldNotAppear.\n")

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT file_path FROM docs ORDER BY file_path")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 root docs (README+CHANGELOG), got %d: %v", len(rows), rows)
	}
	for _, r := range rows {
		fp, _ := r["file_path"].(string)
		base := filepath.Base(fp)
		if base != "README.md" && base != "CHANGELOG.md" {
			t.Errorf("unexpected file indexed: %q", fp)
		}
	}
}

// TestDocIngestExcludedDirs verifies that well-known noise dirs are skipped.
func TestDocIngestExcludedDirs(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	docsDir := filepath.Join(repoRoot, "docs")
	writeDoc(t, filepath.Join(docsDir, "real.md"), "# Real Doc\n\nShould be indexed.\n")
	// node_modules inside docs/ should be excluded.
	writeDoc(t, filepath.Join(docsDir, "node_modules", "pkg", "README.md"),
		"# Node Package\n\nShouldNotAppear.\n")

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT COUNT(*) AS cnt FROM docs")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, _ := rows[0]["cnt"].(int64); cnt != 1 {
		t.Fatalf("expected 1 doc (excluded node_modules), got %v", rows[0]["cnt"])
	}
}

// TestDocIngestDesignDir verifies that docs/design/ and notes/ directories
// are also swept.
func TestDocIngestDesignDir(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	writeDoc(t, filepath.Join(repoRoot, "design", "overview.md"),
		"# Overview\n\nArchitectural overview of the system.\n")
	writeDoc(t, filepath.Join(repoRoot, "notes", "meeting-2026.md"),
		"# Meeting Notes\n\nDecisions made in the architecture review.\n")

	if err := s.IngestDocs(); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Query("SELECT COUNT(*) AS cnt FROM docs")
	if err != nil {
		t.Fatal(err)
	}
	if cnt, _ := rows[0]["cnt"].(int64); cnt != 2 {
		t.Fatalf("expected 2 docs from design/ and notes/, got %v", rows[0]["cnt"])
	}
}

// TestSelectDoc verifies the stem priority selection logic directly.
func TestSelectDoc(t *testing.T) {
	cases := []struct {
		name      string
		input     []stemCandidate
		wantExt   string
		wantEmpty bool
	}{
		{
			name:    "md wins over pdf",
			input:   []stemCandidate{{ext: ".pdf", path: "a.pdf"}, {ext: ".md", path: "a.md"}},
			wantExt: ".md",
		},
		{
			name:    "txt wins over pdf",
			input:   []stemCandidate{{ext: ".pdf", path: "b.pdf"}, {ext: ".txt", path: "b.txt"}},
			wantExt: ".txt",
		},
		{
			name:    "md wins over txt and pdf",
			input:   []stemCandidate{{ext: ".pdf", path: "c.pdf"}, {ext: ".txt", path: "c.txt"}, {ext: ".md", path: "c.md"}},
			wantExt: ".md",
		},
		{
			name:      "empty input returns empty",
			input:     []stemCandidate{},
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectDoc(tc.input)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("expected empty, got %q", got)
				}
				return
			}
			if ext := filepath.Ext(got); ext != tc.wantExt {
				t.Errorf("expected ext %q, got %q (path=%q)", tc.wantExt, ext, got)
			}
		})
	}
}

// TestMatchesGitignore verifies the simplified gitignore matching.
func TestMatchesGitignore(t *testing.T) {
	patterns := []string{"generated/", "*.min.js", "dist/**", "vendor"}

	cases := []struct {
		name    string
		dirName string
		rel     string
		want    bool
	}{
		{"generated dir", "generated", "docs/generated", true},
		{"nested generated", "generated", "generated", true},
		{"dist dir", "dist", "dist", true},
		{"vendor", "vendor", "vendor", true},
		{"normal dir", "design", "design", false},
		{"min.js file", "bundle.min.js", "bundle.min.js", true},
		{"normal md", "spec.md", "docs/spec.md", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchesGitignore(patterns, tc.dirName, tc.rel)
			if got != tc.want {
				t.Errorf("matchesGitignore(patterns, %q, %q) = %v, want %v",
					tc.dirName, tc.rel, got, tc.want)
			}
		})
	}
}
