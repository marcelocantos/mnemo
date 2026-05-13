// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// discoverRepos walks the configured workspace roots and returns every
// directory containing a .git entry. Descent stops at .git (no need to
// walk inside a repo) and skips well-known noise directories so the
// walk stays cheap even on large workspaces.
//
// isExcluded, if non-nil, is consulted for each directory and short-
// circuits descent into registered exclusion subtrees (e.g. the
// configured vault_path when it happens to sit inside a workspace
// root). Pass nil from tests that have no exclusion registry.
//
// The result is sorted and deduplicated. Unreadable roots are skipped
// silently — a missing or permission-denied workspace root must not
// abort the walk.
func discoverRepos(roots []string, isExcluded func(string) bool) []string {
	seen := map[string]bool{}
	var out []string

	for _, root := range roots {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			if isExcluded != nil && isExcluded(path) {
				return filepath.SkipDir
			}
			// A directory containing .git (file or dir) is a repo root;
			// no need to descend further.
			if hasGitEntry(path) {
				if !seen[path] {
					seen[path] = true
					out = append(out, path)
				}
				return filepath.SkipDir
			}
			switch d.Name() {
			case "node_modules", ".venv", "venv", "target",
				"build", ".build", "dist", ".next", ".cache",
				"__pycache__", ".tox", ".mypy_cache", ".pytest_cache":
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			slog.Warn("workspace walk failed", "root", root, "err", err)
		}
	}

	sort.Strings(out)
	return out
}

// hasGitEntry reports whether dir contains a .git child (file or dir).
// A .git file (used by git worktrees) counts as a repo root.
func hasGitEntry(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
