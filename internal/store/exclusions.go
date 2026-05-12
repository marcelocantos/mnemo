// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
)

// exclusionRegistry holds canonicalised path prefixes that ingest
// walkers must skip. The registry exists to prevent mnemo's own
// generated output — currently the configured vault_path — from
// being re-ingested as input. Without exclusion, a vault sitting
// inside a synthesis root or repo docs/ tree would feed its own
// generated content back through the docs walker on every Sync
// cycle, growing the docs index without bound.
//
// Paths are canonicalised (absolute + symlinks resolved when the
// path exists) at registration time and again at check time, so a
// vault reached via a symlink is excluded whether the walker arrives
// via the symlink or via the resolved target. Non-existent paths are
// stored in their absolute-but-unresolved form; canonicalisation
// retries at check time once the directory has been created.
type exclusionRegistry struct {
	mu      sync.RWMutex
	entries []exclusionEntry
}

// exclusionEntry is one registered exclusion. reason is surfaced in
// logs so future-you can answer "why is this path being skipped" by
// reading mnemo's startup log instead of bisecting code.
type exclusionEntry struct {
	original  string // as registered (pre-canonicalisation)
	canonical string
	reason    string
}

// register adds path to the registry. Duplicate registrations of the
// same canonical path are silently ignored, so callers can call this
// idempotently from per-user init code that may run more than once.
func (r *exclusionRegistry) register(path, reason string) {
	if path == "" {
		return
	}
	canon := canonicalisePath(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.canonical == canon {
			return
		}
	}
	r.entries = append(r.entries, exclusionEntry{
		original:  path,
		canonical: canon,
		reason:    reason,
	})
	slog.Info("ingest exclusion registered",
		"path", canon, "reason", reason)
}

// isExcluded reports whether path falls under any registered exclusion.
// "Under" means path is the excluded path itself or a descendant of it.
// Both the query and each registered entry are re-canonicalised so a
// vault path that was registered before its directory existed still
// matches once mnemo's own writes materialise it.
func (r *exclusionRegistry) isExcluded(path string) bool {
	if path == "" {
		return false
	}
	canon := canonicalisePath(path)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.entries {
		ec := e.canonical
		// Re-canonicalise lazily: a path registered before its
		// directory existed has its symlinks unresolved. Retry once
		// the directory may have been created.
		if ec == e.original {
			if recanon := canonicalisePath(e.original); recanon != ec {
				ec = recanon
			}
		}
		if isUnderOrEqual(canon, ec) {
			return true
		}
	}
	return false
}

// canonicalisePath returns an absolute, symlink-resolved version of
// path. If the path does not exist (EvalSymlinks fails), the
// absolute-but-unresolved form is returned. Callers must not assume
// the returned path exists.
func canonicalisePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		// Abs only fails when os.Getwd fails, which is exceptional
		// (e.g. the cwd was deleted out from under the process).
		// Fall back to a Clean'd path so we still return something
		// usable rather than panicking.
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// RegisterExcludedPath registers a directory subtree that ingest
// walkers must skip. Used at startup to keep mnemo's own outputs
// (e.g. the configured vault_path) out of the docs index, which
// would otherwise re-ingest generated content and grow without
// bound on every Sync cycle. The reason string is surfaced in logs
// to make "why is this path being skipped" debuggable. Safe to
// call concurrently; idempotent.
func (s *Store) RegisterExcludedPath(path, reason string) {
	s.exclusions.register(path, reason)
}

// IsExcluded reports whether path falls under any registered
// exclusion. Walkers consult this per entry: excluded directories
// should be skipped via filepath.SkipDir, excluded files should be
// ignored. Safe to call concurrently.
func (s *Store) IsExcluded(path string) bool {
	return s.exclusions.isExcluded(path)
}

// isUnderOrEqual reports whether child is parent itself or a
// descendant of parent. Both arguments must already be canonical
// (absolute, same separator convention, symlinks resolved when
// possible). filepath.Rel handles cross-platform separator concerns;
// the "..\..." prefix check captures the "child is outside parent"
// case on both Unix and Windows.
func isUnderOrEqual(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// Rel returns a path starting with ".." when child is outside
	// parent. On Windows the separator after ".." may be "\" rather
	// than "/", so we check the prefix with the OS separator.
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}
