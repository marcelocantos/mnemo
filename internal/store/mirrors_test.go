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
