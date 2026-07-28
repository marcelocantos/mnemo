// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// withStderr surfaces a subprocess's stderr in its error text. exec's
// ExitError stringifies to bare "exit status 1", which hides the only
// part that says WHY — and the reason is what decides whether a failure
// is a permanent, expected state (issues switched off) or a fault worth
// backing off from (🎯T116).
func withStderr(err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || len(ee.Stderr) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
}

// Repo identity for the GitHub-facing mirror streams (🎯T116).
//
// The convention that a checkout lives at <root>/work/github.com/<org>/
// <repo> is a sound assumption, but the path is a *label*, not evidence:
// the authoritative answer to "which GitHub repo is this" is the
// checkout's configured origin remote. Trusting the path alone made
// mnemo fetch PRs and issues for anything shaped like a repo — empty
// `git init` scaffolds that were never pushed, backup copies, and
// vendored clones filed under the wrong org — producing thousands of
// warnings for repos that do not exist under the name being asked for.
//
// Two legitimate layouts make path and identity diverge, and both fall
// out of reading the remote:
//
//   - Git worktrees. A worktree's .git is a FILE pointing into the
//     parent repo, and it shares the parent's config — so reading the
//     remote maps rustuml-activity to marcelocantos/rustuml with no
//     special-casing beyond following the gitdir pointer.
//   - Local clones named after the repo they came from, e.g.
//     yourworld.defunct or frozen.old. These often have a local-path
//     origin (or none), so they fall back to the prefix rule below.
//
// Resolution reads .git/config directly rather than shelling out to
// git: a reconcile pass walks every known root, and a subprocess per
// repo per pass would be hundreds of execs on a large workspace.

// githubRemotePattern extracts org/repo from the origin URL forms git
// actually writes: git@github.com:org/repo(.git), https://github.com/
// org/repo(.git), and ssh://git@github.com/org/repo(.git).
var githubRemotePattern = regexp.MustCompile(`github\.com[:/]+([^/]+)/([^/\s]+?)(?:\.git)?/?$`)

// gitConfigOriginURL returns the origin remote URL configured for the
// checkout at root, or "" when there is none.
//
// Handles worktrees: when .git is a file it names the worktree's gitdir,
// whose config lives in the parent repo. Reading the parent's config is
// exactly what git itself does, so a worktree reports its parent's
// remote.
func gitConfigOriginURL(root string) string {
	gitPath := filepath.Join(root, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return ""
	}

	configPath := filepath.Join(gitPath, "config")
	if !info.IsDir() {
		// Worktree (or submodule): ".git" is a file containing
		// "gitdir: /path/to/parent/.git/worktrees/<name>".
		raw, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		dir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "gitdir:"))
		if dir == "" {
			return ""
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		// Strip the per-worktree suffix to reach the shared config.
		if i := strings.Index(filepath.ToSlash(dir), "/worktrees/"); i >= 0 {
			dir = filepath.FromSlash(filepath.ToSlash(dir)[:i])
		}
		configPath = filepath.Join(dir, "config")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	return originURLFromConfig(string(data))
}

// originURLFromConfig pulls the url of [remote "origin"] out of a git
// config file. Minimal INI walk — enough for the one key we need, and
// tolerant of the indentation and comment styles git writes.
func originURLFromConfig(cfg string) string {
	inOrigin := false
	for _, line := range strings.Split(cfg, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		if strings.HasPrefix(t, "[") {
			// Section header; git writes [remote "origin"].
			compact := strings.ReplaceAll(t, " ", "")
			inOrigin = strings.EqualFold(compact, `[remote"origin"]`)
			continue
		}
		if !inOrigin {
			continue
		}
		key, val, ok := strings.Cut(t, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		return strings.TrimSpace(val)
	}
	return ""
}

// githubRepoFromRemote maps an origin URL to "org/repo", or "" when the
// remote does not point at GitHub (a local path, a corporate host, a
// Windows share).
func githubRepoFromRemote(url string) string {
	m := githubRemotePattern.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

// resolveGitHubRepo reports the GitHub "org/repo" a checkout actually
// belongs to, or "" when it is not a GitHub checkout and so must never
// be fetched.
//
// Order matters: the origin remote is authoritative and covers ordinary
// clones and worktrees. Only when it is absent or non-GitHub do we fall
// back to the prefix convention — a local clone named after the repo it
// came from (foo.experiment, foo.old, foo-scratch) sitting beside that
// repo. The fallback resolves through the sibling's own remote, so it
// can never invent a repo that isn't itself a real checkout.
func resolveGitHubRepo(root string) string {
	if repo := githubRepoFromRemote(gitConfigOriginURL(root)); repo != "" {
		return repo
	}
	return resolveBySiblingPrefix(root)
}

// resolveBySiblingPrefix implements the local-clone convention: the
// source repo's directory name is a prefix of the clone's, e.g.
// yourworld.defunct beside yourworld. The longest matching sibling wins,
// and it must itself resolve to a GitHub repo via its remote.
func resolveBySiblingPrefix(root string) string {
	parent := filepath.Dir(root)
	name := filepath.Base(root)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sib := e.Name()
		if sib == name || !strings.HasPrefix(name, sib) {
			continue
		}
		// Require a separator after the prefix so "mnemo" does not
		// swallow an unrelated "mnemonic".
		switch name[len(sib)] {
		case '.', '-', '_':
		default:
			continue
		}
		if len(sib) > len(best) {
			best = sib
		}
	}
	if best == "" {
		return ""
	}
	// Resolve via the sibling's remote only — never recurse into the
	// prefix rule again, which could chain clone to clone.
	return githubRepoFromRemote(gitConfigOriginURL(filepath.Join(parent, best)))
}
