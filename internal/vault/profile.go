// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// Profile is the PKM-tool dialect the vault exporter renders for
// (🎯T64.5). It governs tool-specific syntax — chiefly link form — so
// every entity renderer can stay dialect-agnostic and call through the
// profile instead of hard-coding Obsidian wikilinks.
//
// The zero value renders as generic (safe Markdown that every editor
// understands).
type Profile string

const (
	ProfileObsidian = Profile(store.VaultProfileObsidian)
	ProfileLogseq   = Profile(store.VaultProfileLogseq)
	ProfileFoam     = Profile(store.VaultProfileFoam)
	ProfileGeneric  = Profile(store.VaultProfileGeneric)
)

// profileFrom maps a resolved config profile string to a Profile,
// defaulting unknown/empty to generic. Config validation already
// rejects typos, so the default only fires for the empty case.
func profileFrom(s string) Profile {
	switch s {
	case store.VaultProfileObsidian:
		return ProfileObsidian
	case store.VaultProfileLogseq:
		return ProfileLogseq
	case store.VaultProfileFoam:
		return ProfileFoam
	default:
		return ProfileGeneric
	}
}

// Link renders a link to a vault page.
//
//   - target is the vault-relative page path WITHOUT the ".md"
//     extension, using forward slashes (e.g. "_mnemo/themes/auth-redesign").
//   - alias is the human-facing display text. It may be empty, in which
//     case the link shows the target's own title/basename.
//
// The rendering differs per PKM dialect (see the design's profile
// table):
//
//	| profile  | with alias        | without alias |
//	|----------|-------------------|---------------|
//	| obsidian | [[target|alias]]  | [[target]]    |
//	| logseq   | [[target]]        | [[target]]    |
//	| foam     | [[target]]        | [[target]]    |
//	| generic  | [alias](target.md)| [target](target.md) |
//
// Logseq and Foam have no in-link alias syntax, so a meaningful alias is
// dropped there — the linked page's own title carries the name. Obsidian
// keeps the alias only when it adds information (differs from the target
// basename), so graph view isn't cluttered with redundant [[x|x]].
func (p Profile) Link(target, alias string) string {
	target = strings.TrimSuffix(target, ".md")
	switch p {
	case ProfileObsidian:
		if alias == "" || alias == pageBasename(target) {
			return "[[" + target + "]]"
		}
		return "[[" + target + "|" + alias + "]]"
	case ProfileLogseq, ProfileFoam:
		// Bare wikilink; no in-link alias in these dialects.
		return "[[" + target + "]]"
	default: // ProfileGeneric — portable Markdown link.
		display := alias
		if display == "" {
			display = target
		}
		return "[" + display + "](" + target + ".md)"
	}
}

// pageBasename returns the final path segment of a forward-slash vault
// page path, used to decide whether an Obsidian alias is redundant.
func pageBasename(target string) string {
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}
