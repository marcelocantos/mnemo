// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeGh writes an executable stub named "gh" whose body is the given
// shell script, and returns its path. The script receives gh's real
// argv, so it can behave differently for "pr" and "issue".
func fakeGh(t *testing.T, script string) string {
	t.Helper()
	requirePOSIXShell(t)
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPollGitHubPropagatesFailure is the fix for why the noise never
// decayed (🎯T116): pollGitHubForRepo logged both fetch errors and
// returned nil, so ReconcileStaleMirrors recorded a success, 🎯T91's
// backoff never engaged, and an impossible repo was retried every
// interval forever.
func TestPollGitHubPropagatesFailure(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	gh := fakeGh(t, `echo "GraphQL: Could not resolve to a Repository with the name 'o/r'." >&2; exit 1`)

	err := s.pollGitHubForRepo(gh, "o/r")
	if err == nil {
		t.Fatal("a repo that cannot be resolved must return an error so the backoff engages")
	}

	// And that error must actually drive the backoff end to end.
	now := time.Now().UTC()
	if !s.mirrorDue("o/r", "github", 15*time.Minute, now) {
		t.Fatal("expected an untracked repo to be due")
	}
	if rerr := s.recordMirrorFailure("o/r", "github", now); rerr != nil {
		t.Fatal(rerr)
	}
	if s.mirrorDue("o/r", "github", 15*time.Minute, now.Add(time.Minute)) {
		t.Error("expected the repo to back off after a propagated failure")
	}
}

// TestPollGitHubTreatsDisabledFeatureAsSkip: issues being switched off
// is a settled fact about the repo (torvalds/linux), not a fault. It
// must not warn, must not count as a failure, and must not stop the
// other sub-stream for the same repo from succeeding.
func TestPollGitHubTreatsDisabledFeatureAsSkip(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	gh := fakeGh(t, `
if [ "$1" = "issue" ]; then
  echo "the 'o/r' repository has disabled issues" >&2
  exit 1
fi
echo '[]'`)

	if err := s.pollGitHubForRepo(gh, "o/r"); err != nil {
		t.Errorf("disabled issues must be a quiet skip, not a failure: %v", err)
	}
}

// TestPollGitHubSucceedsWhenBothStreamsReturn guards the happy path:
// empty-but-valid JSON from both sub-streams is a clean reconcile.
func TestPollGitHubSucceedsWhenBothStreamsReturn(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	gh := fakeGh(t, `echo '[]'`)

	if err := s.pollGitHubForRepo(gh, "o/r"); err != nil {
		t.Errorf("expected a clean reconcile, got %v", err)
	}
}
