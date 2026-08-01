// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Label sources recorded in themes.label_source.
const (
	labelSourceVaultUser = "vault_user"
	labelSourceBigram    = "bigram"
	labelSourceLLM       = "llm"
)

// defaultUserFilenameExclude are the daily-note / stub filename patterns
// that disqualify a vault_user note from anchoring a cluster label
// (docs/design/vault-clustering.md § Cluster labelling, gate 3),
// regardless of length. Users extend this set via
// vault_clustering.label.user_filename_exclude.
var defaultUserFilenameExclude = []*regexp.Regexp{
	regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(\.md)?$`),
	regexp.MustCompile(`^\d{4}_\d{2}_\d{2}(\.md)?$`),
	regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}(\.md)?$`),
	regexp.MustCompile(`^(daily|journal|journals|inbox|scratch|todo|untitled)(\.md)?$`),
}

// excludedParentDirs are path segments under which any note is a
// daily/journal/stub, excluded regardless of filename.
var excludedParentDirs = map[string]struct{}{
	"daily": {}, "journals": {}, "inbox": {}, "scratch": {},
}

// userAnchorResult is the outcome of the user-anchored labelling path.
// When Label is non-empty a qualifying vault_user note named the theme;
// otherwise RejectNote explains which gate the best candidate failed (or
// is empty when the cluster had no vault_user member at all).
type userAnchorResult struct {
	Label      string
	RejectNote string
}

// userAnchorLabel implements gate 1 of the label chain: a vault_user note
// names the cluster when it passes all four quality gates
// (docs/design/vault-clustering.md § Cluster labelling):
//
//  1. centroid-closeness — the closest vault_user member to the centroid;
//  2. minimum body — ≥ minTokens words of body (frontmatter already
//     stripped by VaultUserCorpusDocs);
//  3. filename exclusion — daily-note / stub filenames and dirs are out;
//  4. title-content coherence — the title shares a non-stopword token
//     with the cluster centroid text.
//
// simTo maps member index → centroid cosine (computed by the caller).
// extraExclude are user-configured extra filename regexps.
func userAnchorLabel(docs []ClusterCorpusDoc, members []int, simTo map[int]float64,
	centroidText string, minTokens int, extraExclude []*regexp.Regexp) userAnchorResult {

	// Candidate = the centroid-closest vault_user member (gate 1).
	best := -1
	bestSim := -1.0
	for _, mi := range members {
		if docs[mi].Kind != "vault_user" {
			continue
		}
		if s := simTo[mi]; s > bestSim {
			bestSim = s
			best = mi
		}
	}
	if best < 0 {
		return userAnchorResult{} // no vault_user member — silent fall-through
	}

	path := docs[best].EntityID // vault_user EntityID is the file path
	title := noteTitle(path)

	// Gate 3: filename / parent-dir exclusion.
	if filenameExcluded(path, extraExclude) {
		return userAnchorResult{RejectNote: "user-anchor rejected: daily-note/stub filename pattern"}
	}
	// Gate 2: minimum body content.
	if len(strings.Fields(docs[best].Text)) < minTokens {
		return userAnchorResult{RejectNote: "user-anchor rejected: below the " +
			itoaMin(minTokens) + "-token body minimum"}
	}
	// Gate 4: title-content coherence.
	if !titleSharesToken(title, centroidText) {
		return userAnchorResult{RejectNote: "user-anchor rejected: title shares no token with the cluster centroid"}
	}
	return userAnchorResult{Label: title}
}

// noteTitle derives a human title from a vault note path: the base name
// without extension, underscores/dashes to spaces, title-cased.
func noteTitle(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	base = strings.NewReplacer("-", " ", "_", " ").Replace(base)
	return titleCase(strings.TrimSpace(base))
}

// filenameExcluded reports whether a vault note path matches a daily-note
// / stub pattern (default or user-extended) or lives under an excluded
// parent directory.
func filenameExcluded(path string, extra []*regexp.Regexp) bool {
	base := strings.ToLower(filepath.Base(path))
	for _, re := range defaultUserFilenameExclude {
		if re.MatchString(base) {
			return true
		}
	}
	for _, re := range extra {
		if re != nil && re.MatchString(base) {
			return true
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if _, ok := excludedParentDirs[strings.ToLower(seg)]; ok {
			return true
		}
	}
	return false
}

// titleSharesToken reports whether the title and centroid text share at
// least one non-stopword token (gate 4). Non-stemmed: an exact token
// match is required, which is stricter than the design's stemmed
// overlap but never produces a false coherence.
func titleSharesToken(title, centroidText string) bool {
	cent := map[string]struct{}{}
	for _, t := range tokenize(centroidText) {
		cent[t] = struct{}{}
	}
	for _, t := range tokenize(title) {
		if _, ok := cent[t]; ok {
			return true
		}
	}
	return false
}

// compileUserFilenameExclude turns config regex strings into compiled
// patterns, dropping (with the returned notes) any that do not compile so
// one bad regex cannot break labelling.
func compileUserFilenameExclude(patterns []string) ([]*regexp.Regexp, []string) {
	var out []*regexp.Regexp
	var notes []string
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			notes = append(notes, "label.user_filename_exclude: bad regexp "+p+" ignored")
			continue
		}
		out = append(out, re)
	}
	return out, notes
}

func itoaMin(n int) string {
	// small helper to avoid importing strconv here; token minimums are small.
	if n <= 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
