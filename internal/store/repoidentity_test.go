// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mkRepo creates a checkout at dir with the given origin URL. An empty
// url means "git init but never pushed" — a .git dir with no remote.
func mkRepo(t *testing.T, dir, url string) string {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\trepositoryformatversion = 0\n"
	if url != "" {
		cfg += "[remote \"origin\"]\n\turl = " + url + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mkWorktree creates a worktree-shaped checkout at dir: a .git FILE
// pointing into parent's .git/worktrees/<name>, exactly as git writes.
func mkWorktree(t *testing.T, dir, parentRepo string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(parentRepo, ".git", "worktrees", filepath.Base(dir))
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"),
		[]byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveGitHubRepo(t *testing.T) {
	org := filepath.Join(t.TempDir(), "work", "github.com", "marcelocantos")

	// An ordinary clone: the remote is authoritative.
	plain := mkRepo(t, filepath.Join(org, "mnemo"), "git@github.com:marcelocantos/mnemo.git")
	// A worktree of it — the layout that produced most of the bad names.
	worktree := mkWorktree(t, filepath.Join(org, "mnemo-rel"), plain)
	// A never-pushed scaffold: git init, no remote, not on GitHub.
	scaffold := mkRepo(t, filepath.Join(org, "hellojava"), "")
	// A checkout whose remote is not GitHub at all.
	windows := mkRepo(t, filepath.Join(org, "hms"), `H:\development\git\HMS\hub.git`)
	// A local clone named after its source, with a local-path origin.
	clone := mkRepo(t, filepath.Join(org, "mnemo.experiment"), filepath.Join(org, "mnemo"))
	// A misfiled clone: directory says one thing, remote says another.
	misfiled := mkRepo(t, filepath.Join(org, "dawn"), "git@github.com:google/dawn.git")
	// A similarly-named repo that must NOT be swallowed by the prefix
	// rule ("mnemo" is a prefix of "mnemonic" with no separator).
	mnemonic := mkRepo(t, filepath.Join(org, "mnemonic"), "")

	tests := []struct {
		name string
		root string
		want string
	}{
		{"plain clone resolves from its remote", plain, "marcelocantos/mnemo"},
		{"worktree resolves to its parent repo", worktree, "marcelocantos/mnemo"},
		{"https remote form", mkRepo(t, filepath.Join(org, "writ"), "https://github.com/marcelocantos/writ.git"), "marcelocantos/writ"},
		{"remote without .git suffix", mkRepo(t, filepath.Join(org, "arr.ai"), "git@github.com:marcelocantos/arr.ai"), "marcelocantos/arr.ai"},
		{"never-pushed scaffold is not a GitHub repo", scaffold, ""},
		{"non-GitHub remote is not a GitHub repo", windows, ""},
		{"prefix-named local clone resolves to its source", clone, "marcelocantos/mnemo"},
		{"misfiled clone follows its remote, not its path", misfiled, "google/dawn"},
		{"prefix rule needs a separator", mnemonic, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveGitHubRepo(tc.root); got != tc.want {
				t.Errorf("resolveGitHubRepo(%s) = %q, want %q",
					filepath.Base(tc.root), got, tc.want)
			}
		})
	}
}

// TestResolveGitHubRepoNoSubprocess is the cost guard: resolution runs
// for every root on every reconcile pass, so it must be file reads, not
// a `git` exec per repo (🎯T116). Proven by resolving with PATH emptied
// — any shell-out would fail and change the answer.
func TestResolveGitHubRepoNoSubprocess(t *testing.T) {
	org := filepath.Join(t.TempDir(), "work", "github.com", "marcelocantos")
	root := mkRepo(t, filepath.Join(org, "mnemo"), "git@github.com:marcelocantos/mnemo.git")

	t.Setenv("PATH", "")
	if got := resolveGitHubRepo(root); got != "marcelocantos/mnemo" {
		t.Errorf("resolution needs a subprocess: got %q with PATH empty", got)
	}
}

func TestPermanentGitHubSkip(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"gh issue list: exit status 1: the 'torvalds/linux' repository has disabled issues", true},
		{"gh pr list: exit status 1: Actions are disabled for this repository", true},
		{"gh issue list: exit status 1: GraphQL: Could not resolve to a Repository with the name 'x/y'.", false},
		{"gh pr list: exit status 1: HTTP 401: Bad credentials", false},
		{"gh pr list: exit status 1: could not connect to api.github.com", false},
	}
	for _, tc := range tests {
		got := permanentGitHubSkip(errors.New(tc.msg)) != ""
		if got != tc.want {
			t.Errorf("permanentGitHubSkip(%.60q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestWithStderrSurfacesReason: the classifier above can only work if
// the reason reaches the error text. exec's ExitError stringifies to
// "exit status 1" and hides stderr, where gh puts the explanation.
func TestWithStderrSurfacesReason(t *testing.T) {
	err := exec.Command("sh", "-c", "echo 'has disabled issues' >&2; exit 1").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Skip("no ExitError from shell")
	}
	// .Run() does not capture stderr; emulate what .Output() populates.
	ee.Stderr = []byte("the 'torvalds/linux' repository has disabled issues\n")
	got := withStderr(ee).Error()
	if permanentGitHubSkip(errors.New(got)) == "" {
		t.Errorf("stderr reason not surfaced for classification: %q", got)
	}
}
