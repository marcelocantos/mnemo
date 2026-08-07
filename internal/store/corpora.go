// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Corpus registry for unified search (🎯T144).
//
// mnemo indexes 22 FTS corpora and, before this, mnemo_search queried
// exactly one of them (messages_fts). Every other corpus was reachable
// only through a tool of its own — which is why the tool surface reached
// 70 entries: "search X" meant "a tool for X". This registry is the
// other half of 🎯T143's reduction: one search spans the corpora, so the
// per-corpus tools are subsumed rather than merely deleted.
//
// Adding a corpus is one entry here. It is deliberately NOT every FTS
// table: each additional corpus is another FTS query per search, on the
// tool that carries 55% of all agent calls.

// corpusSpec describes one searchable corpus: how to match it, how to
// resolve a hit back to displayable fields, and how to sample it for
// calibration.
type corpusSpec struct {
	// kind is the value reported on each hit and accepted by the
	// `kinds` filter. Stable identifier — it appears in tool output.
	kind string
	// fts is the FTS5 table matched against.
	fts string
	// source is the content table the FTS index shadows.
	source string
	// selectSQL resolves a set of rowids to display fields. It must
	// select exactly (id, title, body, meta, ts) in that order, and
	// carry a single `WHERE id IN (...)` placeholder set appended by
	// the caller.
	selectSQL string
	// sampleExpr is the text column probed to draw calibration terms.
	sampleExpr string
	// inDefault marks membership of the default search set. Corpora
	// outside it are reachable via the `kinds` filter.
	inDefault bool
}

// searchCorpora is the registry. Order is stable so output ordering is
// deterministic when quantiles tie.
func searchCorpora() []corpusSpec {
	return []corpusSpec{
		{
			kind:   "message",
			fts:    "messages_fts",
			source: "messages",
			selectSQL: `SELECT id, COALESCE(role,''), COALESCE(text,''),
				COALESCE(session_id,''), COALESCE(timestamp,'') FROM messages`,
			sampleExpr: "text",
			inDefault:  true,
		},
		{
			kind:   "segment",
			fts:    "topic_segments_fts",
			source: "topic_segments",
			selectSQL: `SELECT id, COALESCE(label,''), COALESCE(summary,''),
				COALESCE(repo,'') || ' ' || COALESCE(session_id,''), '' FROM topic_segments`,
			sampleExpr: "summary",
			inDefault:  true,
		},
		{
			kind:   "decision",
			fts:    "decisions_fts",
			source: "decisions",
			selectSQL: `SELECT id, COALESCE(proposal_text,''), COALESCE(confirmation_text,''),
				COALESCE(repo,''), COALESCE(timestamp,'') FROM decisions`,
			sampleExpr: "proposal_text",
			inDefault:  true,
		},
		{
			kind:   "doc",
			fts:    "docs_fts",
			source: "docs",
			selectSQL: `SELECT id, COALESCE(title, file_path), COALESCE(content,''),
				COALESCE(repo,'') || ' ' || COALESCE(file_path,''), COALESCE(mtime,'') FROM docs`,
			sampleExpr: "content",
			inDefault:  true,
		},
		{
			kind:   "target",
			fts:    "targets_fts",
			source: "targets",
			selectSQL: `SELECT id, COALESCE(target_id,'') || ' ' || COALESCE(name,''),
				COALESCE(description,''), COALESCE(repo,'') || ' ' || COALESCE(status,''), ''
				FROM targets`,
			sampleExpr: "description",
			inDefault:  true,
		},
		{
			kind:   "commit",
			fts:    "git_commits_fts",
			source: "git_commits",
			selectSQL: `SELECT id, COALESCE(subject,''), COALESCE(body,''),
				COALESCE(repo,'') || ' ' || COALESCE(commit_hash,''), COALESCE(commit_date,'')
				FROM git_commits`,
			sampleExpr: "subject",
			inDefault:  true,
		},
		{
			kind:   "pr",
			fts:    "github_prs_fts",
			source: "github_prs",
			selectSQL: `SELECT id, COALESCE(title,''), COALESCE(body,''),
				COALESCE(repo,'') || ' #' || COALESCE(pr_number,''), COALESCE(updated_at,'')
				FROM github_prs`,
			sampleExpr: "title",
			inDefault:  true,
		},
		{
			kind:   "memory",
			fts:    "memories_fts",
			source: "memories",
			selectSQL: `SELECT id, COALESCE(name,''), COALESCE(content,''),
				COALESCE(project,'') || ' ' || COALESCE(memory_type,''), COALESCE(updated_at,'')
				FROM memories`,
			sampleExpr: "content",
			inDefault:  true,
		},
		// Below: reachable via `kinds`, out of the default set to bound
		// per-search cost.
		{
			kind:   "plan",
			fts:    "plans_fts",
			source: "plans",
			selectSQL: `SELECT id, COALESCE(phase,''), COALESCE(content,''),
				COALESCE(repo,'') || ' ' || COALESCE(file_path,''), COALESCE(updated_at,'')
				FROM plans`,
			sampleExpr: "content",
		},
		{
			kind:   "config",
			fts:    "claude_configs_fts",
			source: "claude_configs",
			selectSQL: `SELECT id, COALESCE(file_path,''), COALESCE(content,''),
				COALESCE(repo,''), COALESCE(updated_at,'') FROM claude_configs`,
			sampleExpr: "content",
		},
		{
			kind:   "skill",
			fts:    "skills_fts",
			source: "skills",
			selectSQL: `SELECT id, COALESCE(name,''), COALESCE(description,'') || ' ' || COALESCE(content,''),
				COALESCE(file_path,''), COALESCE(updated_at,'') FROM skills`,
			sampleExpr: "content",
		},
		{
			kind:   "audit",
			fts:    "audit_entries_fts",
			source: "audit_entries",
			selectSQL: `SELECT id, COALESCE(summary,''), COALESCE(raw_text,''),
				COALESCE(repo,'') || ' ' || COALESCE(skill,''), COALESCE(date,'')
				FROM audit_entries`,
			sampleExpr: "raw_text",
		},
	}
}

// corpusByKind returns the spec for a kind.
func corpusByKind(kind string) (corpusSpec, bool) {
	for _, c := range searchCorpora() {
		if c.kind == kind {
			return c, true
		}
	}
	return corpusSpec{}, false
}
