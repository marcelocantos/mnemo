// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// cancelGrace bounds how long a cancelled mirror call may take to
// return. The subprocesses below sleep far longer than this, so a call
// that returns inside the grace can only have done so by giving up on
// them. Comfortably above subprocessWaitDelay so a loaded CI runner
// does not turn a correct implementation into a red build.
const cancelGrace = 10 * time.Second

// hangingScript is a subprocess that hangs AND forks, so the sleep is a
// grandchild holding the inherited stdout pipe.
//
// The fork is the point. Killing the direct child does not close a pipe
// a grandchild still holds, so Output() keeps blocking. That is how the
// first version of this fix passed on macOS and failed on Linux, taking
// the full 60 seconds there.
//
// HONESTY NOTE: backgrounding was added to force the fork on both
// platforms, and it does not work — macOS still returns promptly even
// with WaitDelay removed, so the defect cannot be reproduced locally
// and this test is only a real oracle on Linux. Do not read a green run
// on a Mac as evidence that cancellation is bounded; the mechanism that
// makes macOS release the pipe is not understood, and CI is the oracle
// that matters here.
const hangingScript = `sleep 60 & wait`

// TestPollGitHubCancelledMidFetch is the 🎯T124 oracle for the gh half:
// cancelling the worker context must terminate a `gh` subprocess in
// flight rather than letting it run to completion.
//
// Before this, the mirror streams used exec.Command with no context, so
// cancellation never reached the child. That is why 🎯T122 had to bound
// Registry.Close's worker wait and abandon workers instead of awaiting
// them, and why the "Close is prompt" test had to be deleted in 🎯T123.
func TestPollGitHubCancelledMidFetch(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	// A gh that never returns on its own.
	gh := fakeGh(t, hangingScript)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := s.pollGitHubForRepo(ctx, gh, "o/r")
	elapsed := time.Since(start)

	if elapsed > cancelGrace {
		t.Fatalf("pollGitHubForRepo took %s after cancellation; the gh "+
			"subprocess is not context-aware", elapsed)
	}
	if err == nil {
		t.Fatal("a cancelled fetch must report an error, not a silent success — " +
			"a silent success would stamp a reconcile cursor for work that never happened")
	}
}

// TestPollCICancelledMidFetch is the same oracle for the ci stream,
// which shells out to `gh run list` (and `gh run view --log` per failed
// run) on its own path.
func TestPollCICancelledMidFetch(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	gh := fakeGh(t, hangingScript)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := s.pollCIForRepo(ctx, gh, "o/r")
	elapsed := time.Since(start)

	if elapsed > cancelGrace {
		t.Fatalf("pollCIForRepo took %s after cancellation; the gh "+
			"subprocess is not context-aware", elapsed)
	}
	if err == nil {
		t.Fatal("a cancelled CI fetch must report an error")
	}
}

// TestIngestGitCommitsCancelled covers the local commits stream, whose
// `git log` had the same problem.
func TestIngestGitCommitsCancelled(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	repo := newGitRepoWithCommit(t)

	// A git that hangs, so cancellation is the only way out. Prepending
	// its directory to PATH shadows the real binary for this test.
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "git"), hangingScript)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := ingestGitCommits(ctx, s.writeDB, repo, "o/r", "")
	elapsed := time.Since(start)

	if elapsed > cancelGrace {
		t.Fatalf("ingestGitCommits took %s after cancellation; git is not context-aware", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v — cancellation must be "+
			"distinguishable from a repo-level fault, or shutdown poisons the backoff", err)
	}
}

// TestReconcileStaleMirrorsStopsOnCancel proves cancellation unwinds the
// whole reconcile pass rather than only the one subprocess: a cancelled
// context must return promptly instead of walking every remaining repo.
func TestReconcileStaleMirrorsStopsOnCancel(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := s.ReconcileStaleMirrors(ctx, time.Now().UTC())
	if elapsed := time.Since(start); elapsed > cancelGrace {
		t.Fatalf("ReconcileStaleMirrors took %s on an already-cancelled context", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// writeExecutable writes a /bin/sh script at path and marks it runnable.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	requirePOSIXShell(t)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// newGitRepoWithCommit creates a throwaway git repo with one commit and
// returns its path, skipping the test when git is unavailable.
func newGitRepoWithCommit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

// newEmptyGitRepo creates an initialised repo with NO commits — the
// state that produced the recurring exit-128 warnings (🎯T124).
func newEmptyGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Keep the test independent of the developer's git config and hooks.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
