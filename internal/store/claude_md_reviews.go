// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ReviewTriggerConfig tunes when a fresh CLAUDE.md summary review
// fires. The cheap signal is "entries since last review" (transcript
// events with timestamp > the last review's reviewed_at).
//
// Two thresholds compose:
//
//   - HighEntryThreshold — fires immediately when crossed. Catches
//     repos under heavy active work where the project's shape can
//     change fast even within a day.
//   - LowEntryThreshold + LowMinAge — fires when both conditions
//     hold. Catches the slow drift case where a small but non-zero
//     amount of activity has accumulated over a long period.
//
// Defaults are conservative — better to under-trigger reviews (no
// LLM cost) than to over-trigger them (LLM cost on every tick).
type ReviewTriggerConfig struct {
	HighEntryThreshold int
	LowEntryThreshold  int
	LowMinAge          time.Duration
}

// DefaultReviewTriggerConfig is the recommended baseline.
var DefaultReviewTriggerConfig = ReviewTriggerConfig{
	HighEntryThreshold: 500,
	LowEntryThreshold:  50,
	LowMinAge:          24 * time.Hour,
}

// ShouldReview decides whether a repo's CLAUDE.md summary needs a
// fresh LLM review. lastReviewAt is the zero value when the repo has
// no prior review on record; in that case any non-zero
// entriesSinceReview is sufficient as long as the low-threshold
// criteria are also met (no special "first review" path — the
// thresholds apply uniformly).
//
// Returns (shouldReview, reason) — reason is empty when shouldReview
// is false, otherwise a human-readable string suitable for logging
// the trigger ("entries=523 ≥ high=500"). The pure-function shape
// makes the trigger deterministic and table-testable.
func ShouldReview(
	cfg ReviewTriggerConfig,
	lastReviewAt time.Time,
	entriesSinceReview int,
	now time.Time,
) (bool, string) {
	if entriesSinceReview >= cfg.HighEntryThreshold {
		return true, fmt.Sprintf("entries=%d ≥ high=%d",
			entriesSinceReview, cfg.HighEntryThreshold)
	}
	if entriesSinceReview >= cfg.LowEntryThreshold {
		// First-review case: lastReviewAt is zero, so any age check
		// against it would always pass. Treat the absence of a prior
		// review as "infinitely old" — only the low-entry threshold
		// gates the first review.
		if lastReviewAt.IsZero() {
			return true, fmt.Sprintf("entries=%d ≥ low=%d (no prior review)",
				entriesSinceReview, cfg.LowEntryThreshold)
		}
		age := now.Sub(lastReviewAt)
		if age >= cfg.LowMinAge {
			return true, fmt.Sprintf("entries=%d ≥ low=%d AND age=%s ≥ %s",
				entriesSinceReview, cfg.LowEntryThreshold,
				age.Truncate(time.Minute), cfg.LowMinAge)
		}
	}
	return false, ""
}

// ClaudeMDReview is one row of the claude_md_reviews table.
type ClaudeMDReview struct {
	ID               int64  `json:"id"`
	Repo             string `json:"repo"`
	ReviewedAt       string `json:"reviewed_at"`
	CommitID         string `json:"commit_id,omitempty"`
	Summary          string `json:"summary"`
	Verdict          string `json:"verdict"`
	ProposedSummary  string `json:"proposed_summary,omitempty"`
	ProposedClaudeMD string `json:"proposed_claude_md,omitempty"`
}

// LatestReview returns the most recent CLAUDE.md review for repo, or
// nil if none exists. Used both by the trigger worker (to compute
// entries-since-review) and by mnemo_repos to surface the verdict.
func (s *Store) LatestReview(repo string) (*ClaudeMDReview, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	row := s.db.QueryRow(`
		SELECT id, repo, reviewed_at, commit_id, summary,
		       verdict, proposed_summary, proposed_claude_md
		FROM claude_md_reviews
		WHERE repo = ?
		ORDER BY reviewed_at DESC
		LIMIT 1
	`, repo)
	var r ClaudeMDReview
	var proposed, proposedMD sql.NullString
	if err := row.Scan(&r.ID, &r.Repo, &r.ReviewedAt, &r.CommitID, &r.Summary,
		&r.Verdict, &proposed, &proposedMD); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if proposed.Valid {
		r.ProposedSummary = proposed.String
	}
	if proposedMD.Valid {
		r.ProposedClaudeMD = proposedMD.String
	}
	return &r, nil
}

// EntriesSinceForRepo counts rows in entries with timestamp > since,
// scoped to sessions whose session_meta.repo OR session_meta.cwd
// matches repo. Used as the cheap-signal driver for ShouldReview.
//
// Empty since (zero time) counts ALL entries — the first-review case.
func (s *Store) EntriesSinceForRepo(repo string, since time.Time) (int, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	pattern := "%" + repo + "%"
	args := []any{pattern, pattern}
	timeFilter := ""
	if !since.IsZero() {
		timeFilter = "AND COALESCE(json_extract(e.raw, '$.timestamp'), '') > ?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}

	q := fmt.Sprintf(`
		SELECT COUNT(*)
		FROM entries e
		WHERE e.session_id IN (
			SELECT sm.session_id FROM session_meta sm
			WHERE sm.repo LIKE ? OR sm.cwd LIKE ?
		)
		%s
	`, timeFilter)

	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// RecordReview persists an LLM-generated CLAUDE.md review.
// reviewedAt should be the time the LLM run completed. The (repo,
// reviewed_at) UNIQUE constraint prevents a clock-collision retry
// from creating a duplicate row.
func (s *Store) RecordReview(r ClaudeMDReview) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO claude_md_reviews
			(repo, reviewed_at, commit_id, summary, verdict,
			 proposed_summary, proposed_claude_md)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo, reviewed_at) DO NOTHING
	`,
		r.Repo, r.ReviewedAt, r.CommitID, r.Summary, r.Verdict,
		nullableString(r.ProposedSummary), nullableString(r.ProposedClaudeMD))
	return err
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RecordClaudeConfig upserts a CLAUDE.md row keyed by file_path.
// Exposed so test fixtures and external bootstrapping (e.g. a
// scripted setup) can prime the index without going through the
// workspace scanner.
func (s *Store) RecordClaudeConfig(repo, filePath, content string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO claude_configs (repo, file_path, content, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			repo = excluded.repo,
			content = excluded.content,
			updated_at = excluded.updated_at
	`, repo, filePath, content, time.Now().UTC().Format(time.RFC3339))
	return err
}
