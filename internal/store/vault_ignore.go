// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"strings"
)

// VaultIgnore is a parsed .mnemoignore file. Patterns follow gitignore
// syntax (the subset that is meaningful for a single root-level ignore
// file — nested .mnemoignore files are not honoured by design).
//
// Supported syntax:
//
//   - Blank lines and lines starting with '#' are ignored.
//   - Leading '/' anchors a pattern to the vault root. Without it,
//     the pattern matches anywhere in the tree.
//   - Trailing '/' restricts the pattern to directories.
//   - Trailing "/**" or a bare "**" segment matches any number of
//     intermediate path components.
//   - Glob characters '*', '?', and character classes work as in
//     filepath.Match within a single component.
//   - A leading '!' inverts the match — a later negation pattern
//     un-ignores a path that an earlier pattern excluded.
//
// Matching is "last rule wins" so a `!` re-include can override an
// earlier broad exclude (e.g. `_mnemo/themes/*` + `!_mnemo/themes/keep.md`).
type VaultIgnore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	pattern  string // pattern body, stripped of !, leading /, trailing /
	negate   bool
	dirOnly  bool
	anchored bool   // true → pattern matches relative path from root only
	parts    []string
}

// LoadVaultIgnore reads ignoreFile and parses it. Returns an empty
// VaultIgnore (matches nothing) when the file does not exist; any other
// IO error is also treated as "no patterns" so a malformed permissions
// state cannot silently widen the read surface — there is nothing to
// widen here, the absent file means "no extra exclusions".
func LoadVaultIgnore(ignoreFile string) *VaultIgnore {
	data, err := os.ReadFile(ignoreFile)
	if err != nil {
		return &VaultIgnore{}
	}
	return ParseVaultIgnore(string(data))
}

// ParseVaultIgnore parses gitignore-syntax text. Exported for tests
// and for any caller (e.g. a future config validator) that wants to
// parse without touching the filesystem.
func ParseVaultIgnore(text string) *VaultIgnore {
	vi := &VaultIgnore{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		// Trim leading whitespace too — gitignore actually treats
		// leading whitespace as literal, but in practice users write
		// indented patterns expecting them to work; the relaxed read
		// is friendlier and matches the documentation's spirit.
		line = strings.TrimLeft(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		if strings.HasPrefix(line, "/") {
			r.anchored = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		// An interior slash also anchors the pattern in gitignore
		// semantics — `foo/bar` matches only at root, not under any
		// other dir. Detect after stripping the leading slash above.
		if strings.Contains(line, "/") {
			r.anchored = true
		}
		if line == "" {
			continue
		}
		r.pattern = line
		r.parts = strings.Split(line, "/")
		vi.rules = append(vi.rules, r)
	}
	return vi
}

// IsEmpty reports whether the ignore file contributed no rules.
// Callers can short-circuit matching work on the fast path.
func (vi *VaultIgnore) IsEmpty() bool { return vi == nil || len(vi.rules) == 0 }

// Match reports whether the given vault-relative path is excluded.
// relPath uses the OS path separator. isDir is true for directory
// entries (so directory-only patterns can skip files).
//
// Last-rule-wins semantics: a later negation overrides an earlier
// exclude. The default (no rule matched) is "not ignored".
func (vi *VaultIgnore) Match(relPath string, isDir bool) bool {
	if vi.IsEmpty() {
		return false
	}
	// Normalise to forward slash for pattern comparison: gitignore
	// patterns are slash-delimited regardless of platform.
	rel := filepath.ToSlash(relPath)
	rel = strings.TrimPrefix(rel, "./")
	ignored := false
	for _, r := range vi.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if matchRule(r, rel) {
			ignored = !r.negate
		}
	}
	return ignored
}

// matchRule reports whether r matches rel. rel uses forward slashes
// and is relative to the vault root with no leading slash.
func matchRule(r ignoreRule, rel string) bool {
	if r.anchored {
		return matchSegments(r.parts, strings.Split(rel, "/"))
	}
	// Unanchored: try matching the pattern at any depth. Either the
	// full pattern matches the tail, or any single component matches
	// (the common "ignore by basename" case like `node_modules` or
	// `*.tmp`).
	segs := strings.Split(rel, "/")
	for i := 0; i <= len(segs)-len(r.parts); i++ {
		if matchSegments(r.parts, segs[i:i+len(r.parts)]) {
			return true
		}
	}
	// Also try a deep match where the pattern starts at an
	// arbitrary depth and consumes through the end (handles
	// `dir/**` and similar — the `**` parts already span depths,
	// but the prefix component needs the loop above).
	if len(r.parts) > 0 && containsDoubleStar(r.parts) {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(r.parts, segs[i:]) {
				return true
			}
		}
	}
	return false
}

// matchSegments returns whether pattern segments match path segments,
// honouring `**` as "zero or more components".
func matchSegments(pat, path []string) bool {
	switch {
	case len(pat) == 0:
		return len(path) == 0
	case pat[0] == "**":
		// `**` matches zero or more components.
		for i := 0; i <= len(path); i++ {
			if matchSegments(pat[1:], path[i:]) {
				return true
			}
		}
		return false
	case len(path) == 0:
		return false
	default:
		ok, err := filepath.Match(pat[0], path[0])
		if err != nil || !ok {
			return false
		}
		return matchSegments(pat[1:], path[1:])
	}
}

func containsDoubleStar(parts []string) bool {
	for _, p := range parts {
		if p == "**" {
			return true
		}
	}
	return false
}
