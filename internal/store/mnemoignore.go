// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"strings"
)

// MnemoIgnore is a gitignore-syntax exclusion matcher for the vault.
//
// Supports the subset of gitignore syntax that covers practical PKM
// use: comments (`#`), blank lines, negation (`!`), root-anchored
// patterns (leading `/`), directory-only patterns (trailing `/`),
// `**` (any path segments, including zero), `*` (any chars except
// `/`), `?` (single char except `/`), and `[…]` character classes.
//
// The matcher is intentionally scoped: only the file at the vault
// root is consulted. Nested .mnemoignore files inside subdirectories
// are not honoured (documented in docs/design/vault-library-wing.md
// under "Scope of .mnemoignore").
type MnemoIgnore struct {
	patterns []mnemoIgnorePattern
}

type mnemoIgnorePattern struct {
	raw      string
	negate   bool // pattern starts with '!'
	dirOnly  bool // pattern ends with '/'
	anchored bool // pattern starts with '/' OR contains a non-trailing '/'
	pattern  string
}

// LoadMnemoIgnore reads a .mnemoignore file at path and returns the
// parsed matcher. A missing file returns an empty matcher (Match
// always returns false) — this is the "no .mnemoignore present"
// state, not an error.
func LoadMnemoIgnore(path string) (*MnemoIgnore, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &MnemoIgnore{}, nil
		}
		return nil, err
	}
	return parseMnemoIgnore(string(data)), nil
}

func parseMnemoIgnore(text string) *MnemoIgnore {
	mi := &MnemoIgnore{}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Handle escaped leading `#` or `!`.
		raw := line
		negate := false
		if strings.HasPrefix(line, "!") {
			negate = true
			line = line[1:]
		}
		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		// A leading `/` anchors the pattern at the vault root.
		// A pattern containing a non-trailing `/` is also anchored
		// (gitignore: "If the pattern contains a slash other than at
		// the end, ... it is relative to the directory level of the
		// particular .gitignore file itself").
		anchored := false
		if strings.HasPrefix(line, "/") {
			anchored = true
			line = strings.TrimPrefix(line, "/")
		} else if strings.Contains(line, "/") {
			anchored = true
		}
		if line == "" {
			continue
		}
		mi.patterns = append(mi.patterns, mnemoIgnorePattern{
			raw:      raw,
			negate:   negate,
			dirOnly:  dirOnly,
			anchored: anchored,
			pattern:  line,
		})
	}
	return mi
}

// Match reports whether relPath (vault-relative, forward-slash separated)
// is excluded by the matcher. isDir is true when relPath refers to a
// directory — gitignore directory-only patterns (trailing `/`) only
// match directories. Negated patterns (leading `!`) re-include a
// previously matched path; last match wins, matching git's semantics.
//
// relPath uses forward slashes regardless of host OS so patterns
// written by users (Obsidian/Logseq run cross-platform) behave
// identically. Callers should normalise before invoking.
func (mi *MnemoIgnore) Match(relPath string, isDir bool) bool {
	if mi == nil || len(mi.patterns) == 0 {
		return false
	}
	relPath = strings.TrimPrefix(relPath, "/")
	excluded := false
	for _, p := range mi.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if mnemoIgnoreMatchOne(p, relPath) {
			excluded = !p.negate
		}
	}
	return excluded
}

// mnemoIgnoreMatchOne reports whether relPath matches a single pattern.
// Anchored patterns match starting at relPath's root; unanchored
// patterns may match at any path component boundary (including the
// basename alone).
func mnemoIgnoreMatchOne(p mnemoIgnorePattern, relPath string) bool {
	if p.anchored {
		return matchGlob(p.pattern, relPath)
	}
	// Unanchored: try matching against the bare basename, against the
	// full path, and against every suffix starting at a path boundary.
	if matchGlob(p.pattern, filepath.Base(relPath)) {
		return true
	}
	if matchGlob(p.pattern, relPath) {
		return true
	}
	for i := 0; i < len(relPath); i++ {
		if relPath[i] == '/' {
			if matchGlob(p.pattern, relPath[i+1:]) {
				return true
			}
		}
	}
	return false
}

// matchGlob is a gitignore-flavoured glob matcher. Differences vs.
// path.Match (the stdlib):
//   - `**` matches any sequence of characters including `/`.
//   - `*` matches any sequence of non-`/` characters.
//   - `?` matches a single non-`/` character.
//   - Trailing `**` (alone or after `/`) matches any tail, including empty.
//
// The implementation is a token-based backtracking matcher; patterns
// and input strings are short (≤ a few hundred chars), so the worst
// case is acceptable.
func matchGlob(pattern, name string) bool {
	return matchGlobAt(pattern, name, 0, 0)
}

func matchGlobAt(pat, name string, pi, ni int) bool {
	for pi < len(pat) {
		switch pat[pi] {
		case '*':
			// Detect `**`.
			if pi+1 < len(pat) && pat[pi+1] == '*' {
				// Consume the `**`. If followed by `/`, also consume it.
				j := pi + 2
				if j < len(pat) && pat[j] == '/' {
					j++
				}
				// Match zero characters …
				if matchGlobAt(pat, name, j, ni) {
					return true
				}
				// … or any number of characters (including `/`).
				for ni < len(name) {
					ni++
					if matchGlobAt(pat, name, j, ni) {
						return true
					}
				}
				return false
			}
			// Single `*`: match any run of non-`/` chars (including empty).
			j := pi + 1
			for {
				if matchGlobAt(pat, name, j, ni) {
					return true
				}
				if ni >= len(name) || name[ni] == '/' {
					return false
				}
				ni++
			}
		case '?':
			if ni >= len(name) || name[ni] == '/' {
				return false
			}
			pi++
			ni++
		case '[':
			if ni >= len(name) {
				return false
			}
			closeIdx := strings.IndexByte(pat[pi+1:], ']')
			if closeIdx < 0 {
				// Malformed class — treat `[` as a literal.
				if pi >= len(pat) || pat[pi] != name[ni] {
					return false
				}
				pi++
				ni++
				continue
			}
			class := pat[pi+1 : pi+1+closeIdx]
			if !matchCharClass(class, name[ni]) {
				return false
			}
			pi += closeIdx + 2
			ni++
		default:
			if ni >= len(name) || pat[pi] != name[ni] {
				return false
			}
			pi++
			ni++
		}
	}
	return ni == len(name)
}

func matchCharClass(class string, c byte) bool {
	negate := false
	if len(class) > 0 && (class[0] == '!' || class[0] == '^') {
		negate = true
		class = class[1:]
	}
	matched := false
	for i := 0; i < len(class); i++ {
		// Range form a-z.
		if i+2 < len(class) && class[i+1] == '-' {
			if c >= class[i] && c <= class[i+2] {
				matched = true
			}
			i += 2
			continue
		}
		if class[i] == c {
			matched = true
		}
	}
	if negate {
		return !matched
	}
	return matched
}
