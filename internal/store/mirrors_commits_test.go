// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestIngestGitCommitsEmptyRepoIsQuiet is the 🎯T124 oracle for the
// recurring-warning half.
//
// The directories that produced "git log failed ... exit status 128"
// three times per reconcile pass, indefinitely — fundguard, quixy,
// vendep — turned out NOT to be non-repos, which is what the target
// originally assumed. They are real checkouts (`git rev-parse
// --git-dir` succeeds, so the existing guard passes) that have never
// been committed to. `git log` exits 128 there with "your current
// branch 'master' does not have any commits yet".
//
// That is a settled fact about a never-pushed scaffold, not a fault.
func TestIngestGitCommitsEmptyRepoIsQuiet(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	repo := newEmptyGitRepo(t)

	n, err := ingestGitCommits(context.Background(), s.writeDB, repo, "o/empty", "")
	if err != nil {
		t.Fatalf("a checkout with no commits is not an error: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d commits from an empty repo, want 0", n)
	}
}

// TestIngestGitCommitsNonRepoIsClassified proves a directory that is
// not a checkout at all is reported as such, so the caller can memoise
// it instead of re-shelling every pass (criterion 4).
func TestIngestGitCommitsNonRepoIsClassified(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	_, err := ingestGitCommits(context.Background(), s.writeDB, t.TempDir(), "o/plain", "")
	if !errors.Is(err, errNotGitCheckout) {
		t.Fatalf("want errNotGitCheckout for a plain directory, got %v", err)
	}
}

// TestIngestGitCommitsIndexesRealRepo guards the happy path, so the
// quiet-on-failure handling above cannot pass by doing nothing at all.
func TestIngestGitCommitsIndexesRealRepo(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	repo := newGitRepoWithCommit(t)

	n, err := ingestGitCommits(context.Background(), s.writeDB, repo, "o/real", "")
	if err != nil {
		t.Fatalf("ingestGitCommits: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d commits, want 1", n)
	}

	var subject string
	if err := s.readDB.QueryRow(
		`SELECT subject FROM git_commits WHERE repo = 'o/real'`).Scan(&subject); err != nil {
		t.Fatalf("commit not indexed: %v", err)
	}
	if subject != "initial" {
		t.Errorf("subject = %q, want %q", subject, "initial")
	}
}

// TestCommitsStreamStopsReShellingNonRepo is criterion 4 end to end: a
// path established as not a checkout is skipped WITHOUT spawning git
// again on subsequent passes.
//
// Counting subprocess spawns is the only honest way to test this — the
// observable defect was not a wrong result but repeated work, and a
// test on the return value alone would pass just as well against the
// old code.
func TestCommitsStreamStopsReShellingNonRepo(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// A git stub that appends a line per invocation, and fails like the
	// real thing does outside a checkout.
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	writeExecutable(t, filepath.Join(dir, "git"),
		"echo x >> "+counter+"\n"+
			`echo "fatal: not a git repository" >&2; exit 128`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	spawns := func() int {
		b, err := os.ReadFile(counter)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(b), "x")
	}

	// First pass: classifies and memoises.
	if _, err := ingestGitCommits(context.Background(), s.writeDB, root, "o/x", ""); !errors.Is(err, errNotGitCheckout) {
		t.Fatalf("want errNotGitCheckout, got %v", err)
	}
	s.markNotCheckout(root)
	after := spawns()
	if after == 0 {
		t.Fatal("the stub git was never invoked; the test is not exercising the path")
	}

	// Subsequent passes must consult the memo, not the filesystem.
	for i := 0; i < 3; i++ {
		if !s.isNotCheckout(root) {
			t.Fatal("root was not memoised as a non-checkout")
		}
	}
	if got := spawns(); got != after {
		t.Errorf("git spawned %d more times after the path was established as a "+
			"non-checkout (total %s); it must be skipped without re-shelling",
			got-after, strconv.Itoa(got))
	}
}
