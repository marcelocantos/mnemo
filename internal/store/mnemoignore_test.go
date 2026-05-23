// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMnemoIgnoreEmpty(t *testing.T) {
	mi := &MnemoIgnore{}
	if mi.Match("any/path.md", false) {
		t.Errorf("empty matcher must not match anything")
	}
}

func TestMnemoIgnoreLoadMissingFile(t *testing.T) {
	mi, err := LoadMnemoIgnore(filepath.Join(t.TempDir(), "absent.ignore"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if mi == nil {
		t.Fatal("expected non-nil matcher even when file is missing")
	}
	if mi.Match("foo.md", false) {
		t.Errorf("missing-file matcher must not match anything")
	}
}

func TestMnemoIgnoreParseAndMatch(t *testing.T) {
	cases := []struct {
		name     string
		patterns string
		path     string
		isDir    bool
		want     bool
	}{
		// Basenames.
		{"basename match", "draft.md\n", "themes/draft.md", false, true},
		{"basename non-match", "draft.md\n", "themes/keep.md", false, false},

		// Anchored.
		{"anchored at root", "/journal.md\n", "journal.md", false, true},
		{"anchored at root not deeper", "/journal.md\n", "areas/journal.md", false, false},
		{"slash anywhere implies anchored", "areas/private.md\n", "areas/private.md", false, true},
		{"slash anywhere not deeper", "areas/private.md\n", "elsewhere/areas/private.md", false, false},

		// Directory-only.
		{"dir-only matches dir", "secrets/\n", "secrets", true, true},
		{"dir-only ignores files", "secrets/\n", "secrets", false, false},
		{"dir-only matches subdir entry", "secrets/\n", "secrets/keep.md", false, false}, // dir not flagged; walker handles SkipDir

		// Wildcards.
		{"star matches segment", "*.tmp.md\n", "scratch.tmp.md", false, true},
		{"star does not cross slash", "a*c\n", "a/b/c", false, false},
		{"double star matches many", "**/private/*.md\n", "areas/private/note.md", false, true},
		{"double star matches zero", "**/private/*.md\n", "private/note.md", false, true},

		// Negation: a later `!pattern` re-includes a previously excluded
		// path. The matcher is invoked per-path by the walker, so this
		// only matters when both patterns hit the same path; cascading
		// from a dir-only exclusion onto descendants is handled by
		// SkipDir in the walker, not by the matcher.
		{
			"negation re-includes",
			"drafts/keep.md\n!drafts/keep.md\n",
			"drafts/keep.md",
			false,
			false,
		},
		{
			"negation only when seen",
			"drafts/keep.md\n!drafts/keep.md\n",
			"drafts/other.md",
			false,
			false,
		},

		// Comments and blank lines.
		{"comment line ignored", "# this is a comment\nignore.md\n", "ignore.md", false, true},
		{"blank line ignored", "\nignore.md\n\n", "ignore.md", false, true},

		// Trailing whitespace.
		{"trailing whitespace tolerated", "ignore.md   \n", "ignore.md", false, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mi := parseMnemoIgnore(c.patterns)
			got := mi.Match(c.path, c.isDir)
			if got != c.want {
				t.Errorf("Match(%q, %v): got %v want %v", c.path, c.isDir, got, c.want)
			}
		})
	}
}

func TestMnemoIgnoreLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mnemoignore")
	body := "# generated drafts the user doesn't want indexed\n_mnemo/themes/draft-*.md\n!_mnemo/themes/draft-pinned.md\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mi, err := LoadMnemoIgnore(path)
	if err != nil {
		t.Fatalf("LoadMnemoIgnore: %v", err)
	}
	if !mi.Match("_mnemo/themes/draft-experiment.md", false) {
		t.Errorf("draft-experiment should be excluded")
	}
	if mi.Match("_mnemo/themes/draft-pinned.md", false) {
		t.Errorf("draft-pinned re-include should win")
	}
	if mi.Match("_mnemo/themes/published.md", false) {
		t.Errorf("published note must not be excluded")
	}
}
