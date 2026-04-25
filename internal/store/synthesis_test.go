// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyTaxonomy covers the path → taxonomy inference for every
// documented layout plus common negative cases.
func TestClassifyTaxonomy(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantTag   string
		wantMatch bool
	}{
		{"paper md", "/home/u/work/github.com/o/r/docs/papers/bug-42.md", "paper", true},
		{"paper nested", "/home/u/work/github.com/o/r/docs/papers/2026/jan.md", "paper", true},
		{"design md", "/home/u/work/github.com/o/r/docs/design/v2-arch.md", "design", true},
		{"analysis md", "/home/u/think/docs/analysis/karpathy-wiki.md", "analysis", true},
		{"plans md", "/home/u/think/docs/plans/q3-roadmap.md", "plans", true},
		{"audit-log singleton", "/home/u/work/github.com/o/r/docs/audit-log.md", "audit-log", true},
		{"convergence-report singleton", "/home/u/work/github.com/o/r/docs/convergence-report.md", "convergence-report", true},

		// Negative: wrong extension.
		{"paper pdf rejected", "/x/docs/papers/note.pdf", "", false},
		{"paper txt rejected", "/x/docs/papers/note.txt", "", false},

		// Negative: outside docs/.
		{"top-level papers/ not taxonomy", "/x/papers/foo.md", "", false},
		{"root readme", "/x/README.md", "", false},

		// Negative: under docs/ but not a taxonomy dir.
		{"docs/TODO.md", "/x/docs/TODO.md", "", false},
		{"docs/arch.md bare", "/x/docs/arch.md", "", false},
		{"docs/unrelated/foo.md", "/x/docs/unrelated/foo.md", "", false},

		// Nested docs/: deepest docs/ anchors.
		{"nested docs wins", "/x/vendor/submod/docs/papers/p.md", "paper", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyTaxonomy(tc.path)
			if ok != tc.wantMatch || got != tc.wantTag {
				t.Errorf("classifyTaxonomy(%q) = (%q, %v); want (%q, %v)",
					tc.path, got, ok, tc.wantTag, tc.wantMatch)
			}
		})
	}
}

// TestParseInlineMetadata covers the inline metadata fields parser.
func TestParseInlineMetadata(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    inlineMetadata
	}{
		{
			name: "all fields plain",
			content: `# My Paper

Date: 2026-04-24
Status: draft
Target: 🎯T34
Source: https://example.com/video

Body starts here.`,
			want: inlineMetadata{
				Date:   "2026-04-24",
				Status: "draft",
				Target: "🎯T34",
				Source: "https://example.com/video",
			},
		},
		{
			name: "bold markdown fields",
			content: `# Paper

**Date:** 2026-04-24
**Status:** stable

Body.`,
			want: inlineMetadata{Date: "2026-04-24", Status: "stable"},
		},
		{
			name:    "no fields",
			content: "# Just a title\n\nBody text.",
			want:    inlineMetadata{},
		},
		{
			name: "stops at blank line after fields",
			content: `Date: 2026-04-24

Status: ignored-because-after-blank`,
			want: inlineMetadata{Date: "2026-04-24"},
		},
		{
			name: "unknown fields ignored",
			content: `Date: 2026-04-24
Author: Marcelo
Status: draft`,
			want: inlineMetadata{Date: "2026-04-24", Status: "draft"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseInlineMetadata(tc.content)
			if got != tc.want {
				t.Errorf("parseInlineMetadata = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestIngestSynthesisMixedRoots exercises IngestSynthesis against a
// temp tree containing both a fake git repo (with docs/papers/) and a
// non-repo planning space (with docs/analysis/). Verifies taxonomy
// inference, metadata extraction, and SearchSynthesis filters.
func TestIngestSynthesisMixedRoots(t *testing.T) {
	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)

	// Root 1: simulates ~/work — contains a git repo with synthesis docs.
	workRoot := t.TempDir()
	repoRoot := filepath.Join(workRoot, "github.com", "org", "myrepo")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, filepath.Join(repoRoot, "docs", "papers", "bug-42.md"),
		"# Bug 42\n\nDate: 2026-04-20\nStatus: stable\n\nDeep dive into bug 42.\n")
	writeDoc(t, filepath.Join(repoRoot, "docs", "design", "v2.md"),
		"# V2 Architecture\n\nDate: 2026-04-18\n\nProposal for rewrite.\n")
	writeDoc(t, filepath.Join(repoRoot, "docs", "audit-log.md"),
		"# Audit Log\n\nDate: 2026-04-22\n\nAudit entries.\n")
	// Non-synthesis doc: should be ignored by IngestSynthesis.
	writeDoc(t, filepath.Join(repoRoot, "docs", "README.md"),
		"# Readme\n\nNot a synthesis doc.\n")
	// Junk dir that must be skipped.
	writeDoc(t, filepath.Join(repoRoot, "node_modules", "docs", "papers", "skip-me.md"),
		"# Should Not Appear\n")

	// Root 2: simulates ~/think — non-repo planning space.
	thinkRoot := t.TempDir()
	writeDoc(t, filepath.Join(thinkRoot, "docs", "analysis", "karpathy.md"),
		"# Karpathy Wiki\n\nDate: 2026-04-23\nSource: https://youtu.be/example\n\nSummary.\n")
	writeDoc(t, filepath.Join(thinkRoot, "docs", "plans", "q3.md"),
		"# Q3 Plan\n\nTarget: 🎯T34\n\nPlanning notes.\n")

	s.SetSynthesisRoots([]string{workRoot, thinkRoot})
	if err := s.IngestSynthesis(); err != nil {
		t.Fatal(err)
	}

	// All five taxonomy-matching files should be indexed.
	all, err := s.SearchSynthesis("", "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 synthesis docs, got %d: %+v", len(all), pathsOf(all))
	}

	// Taxonomy filter.
	papers, err := s.SearchSynthesis("", "paper", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(papers) != 1 || papers[0].Taxonomy != "paper" {
		t.Fatalf("expected 1 paper, got %+v", papers)
	}
	if papers[0].DocDate != "2026-04-20" || papers[0].DocStatus != "stable" {
		t.Errorf("expected metadata Date=2026-04-20 Status=stable, got Date=%q Status=%q",
			papers[0].DocDate, papers[0].DocStatus)
	}

	// Repo filter: myrepo matches three docs (paper, design, audit-log).
	inRepo, err := s.SearchSynthesis("", "", "myrepo", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(inRepo) != 3 {
		t.Fatalf("expected 3 docs from myrepo, got %d: %+v", len(inRepo), pathsOf(inRepo))
	}

	// FTS query.
	hits, err := s.SearchSynthesis("Karpathy", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit for 'Karpathy', got %d", len(hits))
	}
	if hits[0].DocSource != "https://youtu.be/example" {
		t.Errorf("expected Source set, got %q", hits[0].DocSource)
	}
	if hits[0].Taxonomy != "analysis" {
		t.Errorf("expected analysis taxonomy, got %q", hits[0].Taxonomy)
	}

	// Target from plans/q3.md.
	plans, err := s.SearchSynthesis("", "plans", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plans doc, got %d", len(plans))
	}
	if plans[0].DocTarget != "🎯T34" {
		t.Errorf("expected Target=🎯T34, got %q", plans[0].DocTarget)
	}
}

// TestIngestSynthesisEmptyRoots verifies that an unconfigured store is
// a no-op (no panic, no error, no rows written).
func TestIngestSynthesisEmptyRoots(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if err := s.IngestSynthesis(); err != nil {
		t.Fatalf("empty-roots IngestSynthesis returned error: %v", err)
	}
	all, err := s.SearchSynthesis("", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("expected no synthesis docs, got %d", len(all))
	}
}

// pathsOf returns the file paths from a slice of DocInfo, for error
// messages that need to show what was indexed.
func pathsOf(docs []DocInfo) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.FilePath
	}
	return out
}
