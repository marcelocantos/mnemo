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
	// select exactly (rowid, title, body, meta, ts) in that order, and
	// carry a `WHERE rowid IN (...)` placeholder set appended by the
	// caller.
	//
	// ROWID, not id, and the distinction is load-bearing. FTS5 returns
	// the content table's rowid. For a table declared `id INTEGER
	// PRIMARY KEY` the two are the same value, which is why keying on
	// `id` appeared to work for eleven corpora. topic_segments has a
	// TEXT id (seg_0000c2962ef5) and content_rowid=rowid, so `WHERE id
	// IN (13127)` matched nothing and every segment hit rendered blank.
	selectSQL string
	// sampleExpr is the text probed to draw calibration terms. It must
	// cover the columns the FTS actually indexes, not just one of them.
	//
	// Sampling a single column was a systematic bug: BM25 scores across
	// every indexed column, so a distribution built from one is not
	// representative. audit_entries indexes (summary, raw_text) and was
	// probed on raw_text alone; in the e2e fixture that column held
	// padding, so its distribution was built from words no query uses,
	// every real hit landed above it, and audit took all ten head
	// positions of a twelve-corpus search.
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
			selectSQL: `SELECT rowid, COALESCE(role,''), COALESCE(mnemo_text(text, text_z),''),
				COALESCE(session_id,''), COALESCE(timestamp,'') FROM messages`,
			sampleExpr: "mnemo_text(text, text_z)",
			inDefault:  true,
		},
		{
			kind:   "segment",
			fts:    "topic_segments_fts",
			source: "topic_segments",
			selectSQL: `SELECT rowid, COALESCE(label,''), COALESCE(summary,''),
				COALESCE(repo,'') || ' ' || COALESCE(session_id,''), '' FROM topic_segments`,
			sampleExpr: "COALESCE(label,'') || ' ' || COALESCE(summary,'')",
			inDefault:  true,
		},
		{
			kind:   "decision",
			fts:    "decisions_fts",
			source: "decisions",
			selectSQL: `SELECT rowid, COALESCE(proposal_text,''), COALESCE(confirmation_text,''),
				COALESCE(repo,''), COALESCE(timestamp,'') FROM decisions`,
			sampleExpr: "COALESCE(proposal_text,'') || ' ' || COALESCE(confirmation_text,'')",
			inDefault:  true,
		},
		{
			kind:   "doc",
			fts:    "docs_fts",
			source: "docs",
			selectSQL: `SELECT rowid, COALESCE(title, file_path), COALESCE(mnemo_text(content, content_z),''),
				COALESCE(repo,'') || ' ' || COALESCE(file_path,''), COALESCE(mtime,'') FROM docs`,
			sampleExpr: "COALESCE(title,'') || ' ' || COALESCE(mnemo_text(content, content_z),'')",
			inDefault:  true,
		},
		{
			kind:   "target",
			fts:    "targets_fts",
			source: "targets",
			selectSQL: `SELECT rowid, COALESCE(target_id,'') || ' ' || COALESCE(name,''),
				COALESCE(description,''), COALESCE(repo,'') || ' ' || COALESCE(status,''), ''
				FROM targets`,
			sampleExpr: "COALESCE(name,'') || ' ' || COALESCE(description,'') || ' ' || COALESCE(raw_text,'')",
			inDefault:  true,
		},
		{
			kind:   "commit",
			fts:    "git_commits_fts",
			source: "git_commits",
			selectSQL: `SELECT rowid, COALESCE(subject,''), COALESCE(body,''),
				COALESCE(repo,'') || ' ' || COALESCE(commit_hash,''), COALESCE(commit_date,'')
				FROM git_commits`,
			sampleExpr: "COALESCE(subject,'') || ' ' || COALESCE(body,'')",
			inDefault:  true,
		},
		{
			kind:   "pr",
			fts:    "github_prs_fts",
			source: "github_prs",
			selectSQL: `SELECT rowid, COALESCE(title,''), COALESCE(body,''),
				COALESCE(repo,'') || ' #' || COALESCE(pr_number,''), COALESCE(updated_at,'')
				FROM github_prs`,
			sampleExpr: "COALESCE(title,'') || ' ' || COALESCE(body,'')",
			inDefault:  true,
		},
		{
			kind:   "memory",
			fts:    "memories_fts",
			source: "memories",
			selectSQL: `SELECT rowid, COALESCE(name,''), COALESCE(content,''),
				COALESCE(project,'') || ' ' || COALESCE(memory_type,''), COALESCE(updated_at,'')
				FROM memories`,
			sampleExpr: "COALESCE(name,'') || ' ' || COALESCE(description,'') || ' ' || COALESCE(content,'')",
			inDefault:  true,
		},
		// Below: reachable via `kinds`, out of the default set to bound
		// per-search cost.
		{
			kind:   "plan",
			fts:    "plans_fts",
			source: "plans",
			selectSQL: `SELECT rowid, COALESCE(phase,''), COALESCE(content,''),
				COALESCE(repo,'') || ' ' || COALESCE(file_path,''), COALESCE(updated_at,'')
				FROM plans`,
			sampleExpr: "content",
		},
		{
			kind:   "config",
			fts:    "claude_configs_fts",
			source: "claude_configs",
			selectSQL: `SELECT rowid, COALESCE(file_path,''), COALESCE(content,''),
				COALESCE(repo,''), COALESCE(updated_at,'') FROM claude_configs`,
			sampleExpr: "content",
		},
		{
			kind:   "skill",
			fts:    "skills_fts",
			source: "skills",
			selectSQL: `SELECT rowid, COALESCE(name,''), COALESCE(description,'') || ' ' || COALESCE(content,''),
				COALESCE(file_path,''), COALESCE(updated_at,'') FROM skills`,
			sampleExpr: "COALESCE(name,'') || ' ' || COALESCE(description,'') || ' ' || COALESCE(content,'')",
		},
		{
			kind:   "audit",
			fts:    "audit_entries_fts",
			source: "audit_entries",
			selectSQL: `SELECT rowid, COALESCE(summary,''), COALESCE(raw_text,''),
				COALESCE(repo,'') || ' ' || COALESCE(skill,''), COALESCE(date,'')
				FROM audit_entries`,
			sampleExpr: "COALESCE(summary,'') || ' ' || COALESCE(raw_text,'')",
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

// AllCorpusKinds returns every registered corpus kind, for callers that
// deliberately want the full sweep rather than the default set.
func AllCorpusKinds() []string {
	var out []string
	for _, c := range searchCorpora() {
		out = append(out, c.kind)
	}
	return out
}
