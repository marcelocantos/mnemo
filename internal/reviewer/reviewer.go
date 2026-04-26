// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package reviewer implements 🎯T41 — the periodic worker that
// asks an LLM to assess each repo's CLAUDE.md summary against recent
// activity, so a stale summary in mnemo_repos is flagged for human
// rewrite instead of silently misrepresenting the project.
//
// Cheap-signal gating: the worker only invokes the LLM when an entry
// count threshold has been crossed since the last review (see
// store.ShouldReview). This keeps the LLM cost bounded — for a
// dormant repo, the trigger never fires.
//
// The worker writes its verdict + (optionally) a proposed summary or
// proposed CLAUDE.md rewrite into the claude_md_reviews table.
// Nothing is auto-applied to the user's filesystem; the proposal is
// surfaced via mnemo_repos for the human to act on.
package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// LLMCaller is the subset of compact.LLMCaller this package needs.
// Defined locally so reviewer doesn't depend on compact (and so tests
// can stub a deterministic LLM).
type LLMCaller interface {
	Call(ctx context.Context, systemPrompt, userPrompt string) (LLMResult, error)
}

// LLMResult is the subset of compact.LLMResult this package consumes.
type LLMResult struct {
	Text         string
	Model        string
	PromptTokens int
	OutputTokens int
	CostUSD      float64
}

// Reviewer is the per-repo review orchestrator. One Reviewer is
// shared across all repos; each Tick iterates over the repo set.
type Reviewer struct {
	store  *store.Store
	llm    LLMCaller
	config store.ReviewTriggerConfig
}

// New builds a Reviewer with the default trigger config. Use
// NewWithConfig to override.
func New(s *store.Store, llm LLMCaller) *Reviewer {
	return &Reviewer{store: s, llm: llm, config: store.DefaultReviewTriggerConfig}
}

// NewWithConfig builds a Reviewer with a custom trigger config —
// useful for tests that want low thresholds.
func NewWithConfig(s *store.Store, llm LLMCaller, cfg store.ReviewTriggerConfig) *Reviewer {
	return &Reviewer{store: s, llm: llm, config: cfg}
}

// Tick does one pass over the repo set. For each repo whose
// trigger fires, an LLM call is made (sequentially within a tick;
// parallelism is the next-tick worker's concern). Errors are logged
// but never propagated — a bad LLM run on one repo must not block
// the rest of the scan.
func (r *Reviewer) Tick(ctx context.Context) {
	repos, err := r.store.ListRepos("")
	if err != nil {
		slog.Warn("review: list repos failed", "err", err)
		return
	}
	now := time.Now().UTC()
	for _, repoInfo := range repos {
		if err := ctx.Err(); err != nil {
			return
		}
		// Repos with no indexed CLAUDE.md have nothing to review;
		// the summary surface is empty, so a verdict would be
		// meaningless. Skip silently — adding a CLAUDE.md will
		// promote the repo into the review pool on the next tick.
		if repoInfo.Summary == "" {
			continue
		}
		r.tickOne(ctx, repoInfo, now)
	}
}

func (r *Reviewer) tickOne(ctx context.Context, repoInfo store.RepoInfo, now time.Time) {
	last, err := r.store.LatestReview(repoInfo.Repo)
	if err != nil {
		slog.Warn("review: latest review lookup failed",
			"repo", repoInfo.Repo, "err", err)
		return
	}
	var lastAt time.Time
	if last != nil {
		lastAt, _ = time.Parse(time.RFC3339, last.ReviewedAt)
	}
	count, err := r.store.EntriesSinceForRepo(repoInfo.Repo, lastAt)
	if err != nil {
		slog.Warn("review: entry count failed",
			"repo", repoInfo.Repo, "err", err)
		return
	}
	trigger, reason := store.ShouldReview(r.config, lastAt, count, now)
	if !trigger {
		return
	}
	slog.Info("review: trigger fired",
		"repo", repoInfo.Repo, "reason", reason)

	verdict, err := r.runLLM(ctx, repoInfo)
	if err != nil {
		slog.Warn("review: LLM call failed",
			"repo", repoInfo.Repo, "err", err)
		return
	}
	verdict.Repo = repoInfo.Repo
	verdict.ReviewedAt = now.Format(time.RFC3339)
	verdict.Summary = repoInfo.Summary

	if err := r.store.RecordReview(verdict); err != nil {
		slog.Warn("review: record failed",
			"repo", repoInfo.Repo, "err", err)
		return
	}
	slog.Info("review: recorded",
		"repo", repoInfo.Repo, "verdict", verdict.Verdict)
}

// runLLM constructs the system+user prompts, invokes the LLM, and
// parses the JSON verdict. Returns a partially-populated review;
// caller fills in Repo/ReviewedAt/Summary.
func (r *Reviewer) runLLM(ctx context.Context, repoInfo store.RepoInfo) (store.ClaudeMDReview, error) {
	userPrompt, err := r.buildUserPrompt(repoInfo)
	if err != nil {
		return store.ClaudeMDReview{}, fmt.Errorf("build prompt: %w", err)
	}
	res, err := r.llm.Call(ctx, systemPrompt, userPrompt)
	if err != nil {
		return store.ClaudeMDReview{}, err
	}
	return parseVerdict(res.Text)
}

// systemPrompt is the LLM's role description. Kept tight; the
// userPrompt carries all repo-specific content.
const systemPrompt = `You are reviewing whether a project's CLAUDE.md
summary still accurately describes the project, given recent activity.
Output a single JSON object with fields:
  - verdict: one of "current" (summary still accurate), "stale"
    (summary needs a rewrite but CLAUDE.md itself is otherwise fine),
    or "rewritten" (CLAUDE.md itself has fallen out of step with the
    project and needs a rewrite, not just the summary line).
  - proposed_summary: a one-sentence replacement (≤ 120 chars).
    Required when verdict is "stale" or "rewritten". Omit when
    verdict is "current".
  - proposed_claude_md: a complete CLAUDE.md rewrite. Required only
    when verdict is "rewritten". Omit otherwise.
Output ONLY the JSON object — no prose, no Markdown fence. The
object must parse with encoding/json.Unmarshal.`

// buildUserPrompt assembles the per-repo data the LLM needs: current
// CLAUDE.md content, the existing summary, and recent commit/session
// signal. Caps each block so a 10k-commit repo doesn't overflow the
// LLM context window.
func (r *Reviewer) buildUserPrompt(repoInfo store.RepoInfo) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Repo: %s\n", repoInfo.Repo)
	fmt.Fprintf(&b, "Existing summary (extracted from CLAUDE.md first paragraph):\n%s\n\n",
		repoInfo.Summary)

	// Best-effort fetch of full CLAUDE.md — if it fails we still
	// run the review with just the summary; the LLM can then only
	// produce a "current" or "stale" verdict, never "rewritten".
	configs, err := r.store.SearchClaudeConfigs("", repoInfo.Repo, 1)
	if err == nil && len(configs) > 0 {
		fmt.Fprintf(&b, "Current CLAUDE.md content:\n```\n%s\n```\n\n",
			truncate(configs[0].Content, 8000))
	}

	// Last ~30 days of commits, capped at 20 by SearchCommits's
	// limit. Empty query returns the most recent in date order.
	commits, err := r.store.SearchCommits("", repoInfo.Repo, "", 30, 20)
	if err == nil && len(commits) > 0 {
		b.WriteString("Recent commits (newest first):\n")
		for _, c := range commits {
			fmt.Fprintf(&b, "  %s  %s\n",
				safe(c.CommitDate, 10), truncate(c.Subject, 100))
		}
		b.WriteByte('\n')
	}

	return b.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func safe(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// parseVerdict accepts the LLM's text response and pulls out the
// JSON object. Tolerates a leading/trailing Markdown fence (```json
// ... ```) since LLMs sometimes emit one despite the system prompt.
func parseVerdict(text string) (store.ClaudeMDReview, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	var raw struct {
		Verdict          string `json:"verdict"`
		ProposedSummary  string `json:"proposed_summary"`
		ProposedClaudeMD string `json:"proposed_claude_md"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return store.ClaudeMDReview{}, fmt.Errorf("parse LLM JSON: %w (text=%q)", err, text)
	}
	switch raw.Verdict {
	case "current", "stale", "rewritten":
		// ok
	default:
		return store.ClaudeMDReview{}, fmt.Errorf("invalid verdict %q", raw.Verdict)
	}
	return store.ClaudeMDReview{
		Verdict:          raw.Verdict,
		ProposedSummary:  raw.ProposedSummary,
		ProposedClaudeMD: raw.ProposedClaudeMD,
	}, nil
}
