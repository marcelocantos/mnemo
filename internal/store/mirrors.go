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

// mirrorReconcilers is the registry of converted mirror streams
// (🎯T68.5): ci and github (gh-backed, keyed by ciRepos) and commits
// (local git, keyed by repo name with the on-disk root resolved once
// per pass). All three are divergence-driven; the github_mirrors
// divergence gap covers all of them.
//
// Called once per reconcile pass (and per MirrorBacklog call), so the
// one knownRepoRoots filesystem walk needed to map commit repos to
// their roots happens at most once per pass, not per repo.
func (s *Store) mirrorReconcilers() []mirrorReconciler {
	// Resolve commit repo name → on-disk root once. A repo without a
	// local checkout (gh-only) simply has no commits stream.
	commitRoots := map[string]string{}
	var commitNames []string
	for _, rr := range s.knownRepoRoots() {
		if rr.repo == "" || commitRoots[rr.repo] != "" {
			continue
		}
		commitRoots[rr.repo] = rr.root
		commitNames = append(commitNames, rr.repo)
	}

	return []mirrorReconciler{
		{
			stream:    "ci",
			interval:  5 * time.Minute,
			needsGh:   true,
			listRepos: s.ciRepos,
			reconcile: s.pollCIForRepo,
		},
		{
			stream:    "github",
			interval:  15 * time.Minute,
			needsGh:   true,
			listRepos: s.ciRepos,
			reconcile: s.pollGitHubForRepo,
		},
		{
			stream:    "commits",
			interval:  30 * time.Minute,
			needsGh:   false,
			listRepos: func() ([]string, error) { return commitNames, nil },
			reconcile: func(_, repo string) error {
				root := commitRoots[repo]
				if root == "" {
					return nil // no local checkout → nothing to index
				}
				var lastDate string
				s.rwmu.RLock()
				_ = s.db.QueryRow(
					`SELECT MAX(commit_date) FROM git_commits WHERE repo = ?`, repo).Scan(&lastDate)
				s.rwmu.RUnlock()
				ingestGitCommits(s.db, root, repo, lastDate)
				return nil
			},
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
