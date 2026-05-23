// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
)

func TestVaultIgnore(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		path    string
		isDir   bool
		ignored bool
	}{
		{name: "empty file", text: "", path: "a.md", ignored: false},
		{name: "comment only", text: "# nothing\n", path: "a.md", ignored: false},
		{name: "basename glob", text: "*.tmp\n", path: "a.tmp", ignored: true},
		{name: "basename glob nested", text: "*.tmp\n", path: "deep/inside/a.tmp", ignored: true},
		{name: "basename no match", text: "*.tmp\n", path: "a.md", ignored: false},
		{name: "anchored root", text: "/foo.md\n", path: "foo.md", ignored: true},
		{name: "anchored does not match nested", text: "/foo.md\n", path: "sub/foo.md", ignored: false},
		{name: "interior slash anchors", text: "_mnemo/themes/draft-*.md\n", path: "_mnemo/themes/draft-x.md", ignored: true},
		{name: "interior slash not matched elsewhere", text: "_mnemo/themes/draft-*.md\n", path: "other/_mnemo/themes/draft-x.md", ignored: false},
		{name: "dir only matches dir", text: "node_modules/\n", path: "node_modules", isDir: true, ignored: true},
		{name: "dir only skips file", text: "node_modules/\n", path: "node_modules", isDir: false, ignored: false},
		{name: "double star at end", text: "build/**\n", path: "build/a/b/c.md", ignored: true},
		{name: "double star middle", text: "areas/**/secret.md\n", path: "areas/x/y/secret.md", ignored: true},
		{name: "double star zero", text: "areas/**/secret.md\n", path: "areas/secret.md", ignored: true},
		{name: "negation re-includes", text: "_mnemo/themes/*\n!_mnemo/themes/keep.md\n", path: "_mnemo/themes/keep.md", ignored: false},
		{name: "negation order matters", text: "!_mnemo/themes/keep.md\n_mnemo/themes/*\n", path: "_mnemo/themes/keep.md", ignored: true},
		{name: "blank and comments ignored", text: "\n  \n#hi\n*.bak\n", path: "x.bak", ignored: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vi := ParseVaultIgnore(tc.text)
			got := vi.Match(filepath.FromSlash(tc.path), tc.isDir)
			if got != tc.ignored {
				t.Fatalf("Match(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.ignored)
			}
		})
	}
}

func TestLoadVaultIgnoreAbsent(t *testing.T) {
	vi := LoadVaultIgnore(filepath.Join(t.TempDir(), "does-not-exist"))
	if !vi.IsEmpty() {
		t.Fatalf("expected empty matcher for missing file")
	}
	if vi.Match("anything.md", false) {
		t.Fatalf("empty matcher should not match")
	}
}
