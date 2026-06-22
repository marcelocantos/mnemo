// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

// TestMirrorStaleness covers the 🎯T68.5 reconcile-cursor logic: a
// repo×stream with no cursor is stale, a recently-recorded one is
// fresh, and an old one is stale again.
func TestMirrorStaleness(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now().UTC()
	cutoff := now.Add(-5 * time.Minute)

	// No cursor yet → stale (never reconciled).
	if !s.mirrorStale("owner/repo", "ci", cutoff) {
		t.Fatalf("expected missing cursor to be stale")
	}

	// Record a reconcile now → fresh relative to a 5-minute cutoff.
	if err := s.recordMirrorReconcile("owner/repo", "ci", now); err != nil {
		t.Fatalf("recordMirrorReconcile: %v", err)
	}
	if s.mirrorStale("owner/repo", "ci", cutoff) {
		t.Errorf("expected freshly-reconciled cursor to be fresh")
	}

	// Backdate the reconcile to 10 minutes ago → stale again.
	if err := s.recordMirrorReconcile("owner/repo", "ci", now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("recordMirrorReconcile (backdate): %v", err)
	}
	if !s.mirrorStale("owner/repo", "ci", cutoff) {
		t.Errorf("expected 10-min-old cursor to be stale vs a 5-min cutoff")
	}

	// A different stream for the same repo is independent (still stale).
	if !s.mirrorStale("owner/repo", "github", cutoff) {
		t.Errorf("expected untracked stream to be stale")
	}
}

// TestMirrorFailureBackoff covers the 🎯T91 per-(repo,stream) failure
// backoff: a failing repo is not re-attempted until an exponential
// backoff of the stream interval has elapsed, a success clears the
// backoff, and the staleness cursor (used by MirrorBacklog) is untouched
// by failures.
func TestMirrorFailureBackoff(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now().UTC()
	const interval = 5 * time.Minute

	// Untracked → due.
	if !s.mirrorDue("o/r", "ci", interval, now) {
		t.Fatalf("expected untracked repo to be due")
	}

	// One failure → fail_count 1 → backoff = interval (5m).
	if err := s.recordMirrorFailure("o/r", "ci", now); err != nil {
		t.Fatalf("recordMirrorFailure: %v", err)
	}
	if s.mirrorDue("o/r", "ci", interval, now) {
		t.Errorf("expected backoff immediately after a failure")
	}
	if s.mirrorDue("o/r", "ci", interval, now.Add(4*time.Minute)) {
		t.Errorf("expected still backing off at +4m (backoff 5m)")
	}
	if !s.mirrorDue("o/r", "ci", interval, now.Add(6*time.Minute)) {
		t.Errorf("expected due again once the 5m backoff elapsed")
	}

	// Second consecutive failure → fail_count 2 → backoff doubles to 10m.
	if err := s.recordMirrorFailure("o/r", "ci", now.Add(6*time.Minute)); err != nil {
		t.Fatalf("recordMirrorFailure 2: %v", err)
	}
	if s.mirrorDue("o/r", "ci", interval, now.Add(12*time.Minute)) {
		t.Errorf("expected 10m backoff after 2 failures (only 6m elapsed)")
	}
	if !s.mirrorDue("o/r", "ci", interval, now.Add(17*time.Minute)) {
		t.Errorf("expected due after the 10m backoff elapsed")
	}

	// A failure must not advance the success cursor, so MirrorBacklog still
	// sees the repo as never-reconciled (stale).
	if !s.mirrorStale("o/r", "ci", now.Add(17*time.Minute).Add(-interval)) {
		t.Errorf("a failure must not mark the repo as reconciled")
	}

	// A success clears the backoff and sets the cursor.
	tSucc := now.Add(20 * time.Minute)
	if err := s.recordMirrorReconcile("o/r", "ci", tSucc); err != nil {
		t.Fatalf("recordMirrorReconcile: %v", err)
	}
	if s.mirrorDue("o/r", "ci", interval, tSucc.Add(time.Minute)) {
		t.Errorf("expected not due right after a success")
	}
	if !s.mirrorDue("o/r", "ci", interval, tSucc.Add(6*time.Minute)) {
		t.Errorf("expected due once the interval elapses post-success (fail_count reset)")
	}
}

// TestMirrorReconcilersRegistered verifies all three mirror streams
// (ci, github, commits) are registered as divergence-driven
// reconcilers (🎯T68.5 complete).
func TestMirrorReconcilersRegistered(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	got := map[string]bool{}
	for _, mr := range s.mirrorReconcilers() {
		got[mr.stream] = true
	}
	for _, want := range []string{"ci", "github", "commits"} {
		if !got[want] {
			t.Errorf("mirror stream %q not registered; have %v", want, got)
		}
	}
}

// TestReconcileStaleMirrorsEmpty verifies the reconciler is a safe
// no-op when there are no repos to reconcile (fresh store, no sessions)
// — it must not panic or error regardless of gh availability.
func TestReconcileStaleMirrorsEmpty(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	n, err := s.ReconcileStaleMirrors(time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileStaleMirrors: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 reconciles on an empty store, got %d", n)
	}
	if gap, _ := s.MirrorBacklog(time.Now().UTC()); gap != 0 {
		t.Errorf("expected 0 mirror backlog on an empty store, got %d", gap)
	}
}
