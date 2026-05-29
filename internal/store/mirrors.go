// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"log/slog"
	"os/exec"
	"time"
)

// mirrorReconciler describes one external mirror stream as a
// divergence-driven reconciler (🎯T68.5): a repo's stream is
// reconciled only when its mirror_status cursor is missing or older
// than Interval, replacing the former boot-once backfill / fixed poll.
// Adding a stream is one entry here, not a new scheduler — the
// convergence-engine "register a stream" shape (cf. 🎯T68.4).
type mirrorReconciler struct {
	stream    string
	interval  time.Duration
	needsGh   bool
	listRepos func() ([]string, error)
	reconcile func(ghPath, repo string) error
}

// mirrorReconcilers is the registry of converted mirror streams. CI is
// converted first (🎯T68.5); github (s.ciRepos / s.pollGitHubForRepo)
// and commits (knownRepoRoots / ingestGitCommits) register here in
// follow-up increments, at which point they too become divergence-
// driven and the github_mirrors divergence gap covers them.
func (s *Store) mirrorReconcilers() []mirrorReconciler {
	return []mirrorReconciler{
		{
			stream:    "ci",
			interval:  5 * time.Minute,
			needsGh:   true,
			listRepos: s.ciRepos,
			reconcile: s.pollCIForRepo,
		},
	}
}

// ReconcileStaleMirrors reconciles every repo×stream whose mirror_status
// cursor is missing or older than the stream's interval, recording each
// reconcile (🎯T68.5). Returns the number of (repo, stream) pairs
// reconciled this pass. Idempotent and cheap when nothing is stale, so
// it is safe to call on a frequent tick: a repo reconciled recently is
// skipped, while a newly-seen repo is picked up on the next tick rather
// than waiting out a fixed poll cycle. gh-backed streams are skipped
// silently when gh is not installed.
func (s *Store) ReconcileStaleMirrors(now time.Time) (int, error) {
	reconciled := 0
	for _, mr := range s.mirrorReconcilers() {
		ghPath := ""
		if mr.needsGh {
			p, err := exec.LookPath("gh")
			if err != nil {
				continue
			}
			ghPath = p
		}
		repos, err := mr.listRepos()
		if err != nil {
			slog.Warn("mirror: list repos failed", "stream", mr.stream, "err", err)
			continue
		}
		cutoff := now.Add(-mr.interval)
		for _, repo := range repos {
			if !s.mirrorStale(repo, mr.stream, cutoff) {
				continue
			}
			if err := mr.reconcile(ghPath, repo); err != nil {
				slog.Warn("mirror: reconcile failed", "stream", mr.stream, "repo", repo, "err", err)
				continue
			}
			if err := s.recordMirrorReconcile(repo, mr.stream, now); err != nil {
				slog.Warn("mirror: record reconcile failed", "stream", mr.stream, "repo", repo, "err", err)
			}
			reconciled++
		}
	}
	return reconciled, nil
}

// mirrorStale reports whether (repo, stream) has no reconcile cursor or
// one older than cutoff.
func (s *Store) mirrorStale(repo, stream string, cutoff time.Time) bool {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	var last string
	if err := s.db.QueryRow(
		`SELECT last_reconciled_at FROM mirror_status WHERE repo = ? AND stream = ?`,
		repo, stream).Scan(&last); err != nil {
		return true // missing → never reconciled → stale
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, last); err == nil {
			return t.Before(cutoff)
		}
	}
	return true // unparseable → treat as stale
}

// recordMirrorReconcile upserts the reconcile cursor for (repo, stream).
func (s *Store) recordMirrorReconcile(repo, stream string, at time.Time) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO mirror_status (repo, stream, last_reconciled_at)
		VALUES (?, ?, ?)
		ON CONFLICT(repo, stream) DO UPDATE SET
			last_reconciled_at = excluded.last_reconciled_at
	`, repo, stream, at.UTC().Format(time.RFC3339Nano))
	return err
}

// MirrorBacklog returns the gap for the github_mirrors divergence row
// (🎯T68.4/🎯T68.5): the count of repo×stream pairs across the
// converted mirror streams whose reconcile cursor is missing or stale,
// plus the most recent reconcile timestamp across all of them.
func (s *Store) MirrorBacklog(now time.Time) (gap int, lastReconciled string) {
	for _, mr := range s.mirrorReconcilers() {
		if mr.needsGh {
			if _, err := exec.LookPath("gh"); err != nil {
				continue
			}
		}
		repos, err := mr.listRepos()
		if err != nil {
			continue
		}
		cutoff := now.Add(-mr.interval)
		for _, repo := range repos {
			if s.mirrorStale(repo, mr.stream, cutoff) {
				gap++
			}
		}
	}
	s.rwmu.RLock()
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(last_reconciled_at), '') FROM mirror_status`).Scan(&lastReconciled)
	s.rwmu.RUnlock()
	return gap, lastReconciled
}
