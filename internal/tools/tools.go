// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package tools defines MCP tool schemas and handlers for the mnemo server.
package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/bridge"
	"github.com/marcelocantos/mnemo/internal/store"
)

// Handler handles tool calls using a store backend.
type Handler struct {
	mem store.Backend
}

// NewHandler creates a tool handler backed by the given store.
func NewHandler(mem store.Backend) *Handler {
	return &Handler{mem: mem}
}

// Definitions returns the MCP tool definitions.
// These are served to the proxy via the ListTools RPC method.
func Definitions() []mcp.Tool {
	return []mcp.Tool{
		mcp.NewTool("mnemo_search",
			mcp.WithDescription(`Search across Claude Code session transcripts. Uses FTS5 full-text search with fuzzy matching.

Plain word queries use OR matching — "QR code pairing protocol" finds messages containing ANY of those words, ranked by how many match (BM25). This means partial matches surface instead of returning nothing. Messages matching more/rarer terms rank higher.

For exact matching, use explicit FTS5 operators:
- Require all terms: "QR AND transfer"
- Exact phrase: "\"QR transfer\""
- Exclude terms: "QR NOT test"
- Proximity: NEAR(QR transfer, 5)

By default searches only interactive sessions (excludes subagents, worktrees, ephemeral). Noise messages (interrupts, compaction summaries, tool-loaded markers) are excluded from the index.`),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query — plain words use OR (fuzzy). Use AND/NOT/NEAR/quotes for precise control.")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
			mcp.WithString("session_type", mcp.Description(`Filter by session type (default "interactive"). Values: "interactive", "subagent", "worktree", "ephemeral", "all"`)),
			mcp.WithString("repo", mcp.Description(`Filter by repo. Flexible matching against session working directory and extracted repo name. Accepts: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), host/org/repo ("github.com/marcelocantos/mnemo"), or a path fragment ("~/work/myproject").`)),
			mcp.WithNumber("context_before", mcp.Description("Number of messages before each hit to include (default 3)")),
			mcp.WithNumber("context_after", mcp.Description("Number of messages after each hit to include (default 3)")),
			mcp.WithString("context_filter", mcp.Description(`Filter for context messages. "substantive" (default): only non-noise user/assistant messages. "all": include everything (tool calls, system messages, noise).`)),
		),
		mcp.NewTool("mnemo_sessions",
			mcp.WithDescription("List transcript sessions, sorted by most recent activity. By default shows only interactive sessions with at least 6 substantive messages."),
			mcp.WithString("session_type", mcp.Description(`Filter by session type (default "interactive"). Values: "interactive", "subagent", "worktree", "ephemeral", "all"`)),
			mcp.WithNumber("min_messages", mcp.Description("Minimum substantive (non-noise) messages to include (default 6)")),
			mcp.WithNumber("limit", mcp.Description("Max sessions to return (default 30)")),
			mcp.WithString("project", mcp.Description("Filter by project name substring")),
			mcp.WithString("repo", mcp.Description("Filter by repo (org/name substring, e.g. \"marcelocantos/mnemo\")")),
			mcp.WithString("work_type", mcp.Description(`Filter by work type: "development", "feature", "bugfix", "refactor", "chore", "docs", "test", "ci", "release", "review", "branch-work"`)),
		),
		mcp.NewTool("mnemo_read_session",
			mcp.WithDescription("Read messages from a specific session transcript. Returns messages ordered chronologically."),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID (the JSONL filename stem, or a prefix)")),
			mcp.WithString("role", mcp.Description(`Filter by role: "user" or "assistant". Omit for all roles.`)),
			mcp.WithNumber("offset", mcp.Description("Skip first N messages (default 0)")),
			mcp.WithNumber("limit", mcp.Description("Max messages to return (default 50)")),
		),
		mcp.NewTool("mnemo_query",
			mcp.WithDescription(`Run a read-only query against the transcript database.

Accepts plain SQL (SELECT/WITH) or sqldeep nested syntax for hierarchical JSON output.

sqldeep example — repos with their recent sessions:
  FROM session_meta sm
  JOIN session_summary ss ON ss.session_id = sm.session_id
  WHERE ss.last_msg >= datetime('now', '-7 days')
    AND ss.session_type = 'interactive'
  SELECT {
    sm.repo,
    sessions: FROM session_summary s
      WHERE s.session_id = sm.session_id
      SELECT { s.session_id, s.last_msg, s.substantive_msgs, },
  }
  GROUP BY sm.repo

Tables:
  entries (id, session_id, project, type, timestamp, raw)
    — every JSONL line stored as JSONB in 'raw'. Virtual columns:
      model, stop_reason, input_tokens, output_tokens,
      cache_read_tokens, cache_creation_tokens, agent_id, version,
      slug, is_sidechain, data_type, data_command, data_hook_event,
      top_tool_use_id, parent_tool_use_id
    — entry types: user, assistant, progress, system, file-history-snapshot
    — use json_extract(raw, '$.path') for fields without virtual columns
  messages (id, entry_id, session_id, project, role, text, timestamp, type, is_noise)
    — content blocks from user/assistant entries. entry_id links to entries.
    — tool_use fields: tool_name, tool_use_id, tool_input (JSONB), content_type
    — virtual columns from tool_input: tool_file_path, tool_command, etc.
  messages_fts — FTS5 virtual table (excludes noise). Use: WHERE messages_fts MATCH 'terms'
  snapshot_files (id, entry_id, session_id, file_path, backup_time)
    — auto-extracted from file-history-snapshot entries via trigger
  snapshot_files_fts — FTS5 on file_path. Use: WHERE snapshot_files_fts MATCH 'pattern'
  sessions — view: session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg
  session_meta (session_id, repo, cwd, git_branch, work_type, topic)
  session_summary (session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg)
  memories (id, project, file_path, name, description, memory_type, content, updated_at)
    — auto-memory files from ~/.claude/projects/*/memory/*.md
    — memory_type: user, feedback, project, reference
  memories_fts — FTS5 on name, description, content, project
  skills (id, file_path, name, description, content, updated_at)
    — skill files from ~/.claude/skills/*.md
  skills_fts — FTS5 on name, description, content
  claude_configs (id, repo, file_path, content, updated_at)
    — CLAUDE.md project instruction files from all repo roots
  claude_configs_fts — FTS5 on content, repo
  audit_entries (id, repo, file_path, date, skill, version, summary, raw_text)
    — parsed entries from docs/audit-log.md in each repo
    — skill: release, audit, docs, etc. version: vN.N.N if present
  audit_entries_fts — FTS5 on summary, raw_text, repo
  ci_runs (id, repo, run_id, workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url)
    — GitHub Actions runs polled from repos seen in session history
    — status: completed, in_progress, queued; conclusion: success, failure, cancelled, skipped
  ci_runs_fts — FTS5 on repo, workflow, branch, log_summary, conclusion

Join pattern — message with its entry metadata:
  SELECT m.text, e.model, e.input_tokens FROM messages m JOIN entries e ON e.id = m.entry_id

Token usage query:
  SELECT date(timestamp) AS day, SUM(input_tokens) AS input, SUM(output_tokens) AS output
  FROM entries WHERE type = 'assistant' GROUP BY day ORDER BY day DESC

File history — which sessions touched a file:
  SELECT sf.session_id, sf.backup_time, sm.repo
  FROM snapshot_files sf JOIN session_meta sm ON sm.session_id = sf.session_id
  WHERE sf.file_path LIKE '%store.go'

Session types (derived from project path): interactive, subagent, worktree, ephemeral.
is_noise = 1 for interrupts, compaction summaries, tool-loaded markers, slash command markup.
Results capped at 100 rows.

Tip: If you find yourself running the same complex query pattern repeatedly, save it as a template with mnemo_define for reuse.`),
			mcp.WithString("query", mcp.Required(), mcp.Description("SQL SELECT/WITH query, or sqldeep nested syntax (FROM ... SELECT { ... })")),
		),
		mcp.NewTool("mnemo_repos",
			mcp.WithDescription(`List repositories that have been worked on in Claude Code sessions. Returns repo name, filesystem path, session count, and last activity. Use this to discover repo locations, find related projects, or get an overview of recent work.`),
			mcp.WithString("filter", mcp.Description(`Optional filter. Supports: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), path fragment ("/work/github"), or glob ("marcelocantos/sql*"). Omit to list all repos.`)),
		),
		mcp.NewTool("mnemo_stats",
			mcp.WithDescription("Show transcript index statistics — sessions and messages broken down by session type, with noise vs substantive counts."),
		),
		mcp.NewTool("mnemo_recent_activity",
			mcp.WithDescription("Recent session activity grouped by repo. Returns per-repo JSON with session count, message count, last activity time, work types, and key topics. Useful for understanding where active work is happening across projects."),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 7)")),
			mcp.WithString("repo", mcp.Description(`Filter by repo. Accepts: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), or path fragment.`)),
		),
		mcp.NewTool("mnemo_status",
			mcp.WithDescription(`Rich status report of recent work across repos. Returns repos → sessions → conversation excerpts with drill-down offsets.

User messages are shown in full. Assistant messages are truncated (default 200 chars). Each message includes its database ID — use mnemo_read_session with offset to retrieve the full text.

Use this when you need context about recent work: the user references prior discussions, you need to understand project history before making decisions, or you want to know what's been happening across repos. Don't dump the output to the user — use it to inform your own understanding.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 7)")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithNumber("max_sessions", mcp.Description("Max sessions per repo (default 3)")),
			mcp.WithNumber("max_excerpts", mcp.Description("Max message excerpts per session (default 20, most recent kept)")),
			mcp.WithNumber("truncate_len", mcp.Description("Truncate assistant messages to this length (default 200)")),
		),
		mcp.NewTool("mnemo_memories",
			mcp.WithDescription(`Search across Claude Code auto-memory files from all projects. Memories are structured notes with frontmatter (name, description, type) that agents save across sessions.

Memory types: "user" (role/preferences), "feedback" (corrections/confirmations), "project" (ongoing work context), "reference" (pointers to external systems).

Use this to find decisions, preferences, and context captured in any project — even when working in a different repo. Also queryable via mnemo_query against the memories table.`),
			mcp.WithString("query", mcp.Description("Search query (uses same fuzzy OR matching as mnemo_search). Omit to list all.")),
			mcp.WithString("type", mcp.Description(`Filter by memory type: "user", "feedback", "project", "reference"`)),
			mcp.WithString("project", mcp.Description("Filter by project name substring")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_usage",
			mcp.WithDescription(`Token usage analytics across sessions. Aggregates input, output, cache read, and cache creation tokens with cost estimates.

Returns per-period breakdown and totals. Cost estimates use published Anthropic pricing (Opus, Sonnet, Haiku families). Unknown models use Sonnet pricing as fallback.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithString("repo", mcp.Description(`Filter by repo. Accepts: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), or path fragment.`)),
			mcp.WithString("model", mcp.Description(`Filter by model prefix (e.g. "claude-opus-4", "claude-sonnet-4")`)),
			mcp.WithString("group_by", mcp.Description(`Group results by: "day" (default), "model", or "repo"`)),
		),
		mcp.NewTool("mnemo_skills",
			mcp.WithDescription(`Search across Claude Code skill files (~/.claude/skills/). Skills define reusable workflows — release processes, audit procedures, documentation generation, etc. Use this to discover relevant skills or understand what workflows are available.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list all.")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_configs",
			mcp.WithDescription(`Search across CLAUDE.md project instruction files from all repos. These files contain build instructions, conventions, delivery definitions, and project-specific agent guidance. Use this to understand how other projects are configured or to find cross-project patterns.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list all.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_audit",
			mcp.WithDescription(`Search across audit logs (docs/audit-log.md) from all repos. Audit logs record maintenance activities: releases, audits, documentation runs. Use this to check when a project was last released, find maintenance patterns across repos, or review past audit findings.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list all.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name")),
			mcp.WithString("skill", mcp.Description("Filter by skill name (e.g. 'release', 'audit')")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_targets",
			mcp.WithDescription(`Search across convergence targets (docs/targets.md) from all repos. Targets track desired states — features to build, bugs to fix, quality gaps to close. Use this to find targets across projects, check what's active/achieved, or discover cross-project priorities.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list all.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name")),
			mcp.WithString("status", mcp.Description("Filter by status: identified, converging, achieved")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_plans",
			mcp.WithDescription(`Search across implementation plans (.planning/ directories) from all repos. Plans contain architectural decisions, task breakdowns, and implementation reasoning from GSD workflows. Use this to find past design decisions or understand how features were planned.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list all.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_docs",
			mcp.WithDescription(`Search across project documentation files (markdown, plain-text, PDF) indexed from all tracked repos. Covers README, CHANGELOG, design notes, and any files under docs/, design/, notes/, papers/ directories. Deduplicates .md/.pdf pairs with same stem — always prefers .md. Use this to find project documentation, design decisions, and release notes across repos.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list recent.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithString("kind", mcp.Description("Filter by file kind: md, txt, pdf")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_who_ran",
			mcp.WithDescription(`Find sessions that ran a specific shell command. Searches Bash tool_use entries by command pattern, returning session ID, repo, matched command, and timestamp. Useful for tracing when and where a command was last executed across all sessions.`),
			mcp.WithString("pattern", mcp.Required(), mcp.Description("Command substring to match (LIKE match, case-insensitive)")),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_permissions",
			mcp.WithDescription(`Analyze tool usage patterns across sessions to suggest allowedTools rules for settings.json.

Returns the most frequently used tools with counts and concrete suggestions for permission rules. Also analyzes Bash command patterns to suggest fine-grained Bash permissions (e.g., "Bash(go *)", "Bash(git *)").

Use this to understand which tools agents use most and to tighten permissions without blocking common workflows.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithNumber("limit", mcp.Description("Max results per category (default 20)")),
		),
		mcp.NewTool("mnemo_prs",
			mcp.WithDescription(`Search GitHub PRs and issues across all indexed repos. Uses FTS5 for keyword search on titles and bodies. Data is polled from GitHub repos that appear in session history and backfilled at startup.

Supports filtering by state, author, and recency. Results include both PRs and issues unless filtered by type.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching on title/body). Omit to list recent.")),
			mcp.WithString("repo", mcp.Description("Filter by repo (e.g. 'mnemo', 'marcelocantos/mnemo')")),
			mcp.WithString("state", mcp.Description("Filter by state: open, closed, merged (PRs only), all (default)")),
			mcp.WithString("author", mcp.Description("Filter by author username")),
			mcp.WithString("type", mcp.Description("Filter by type: pr, issue, all (default)")),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_ci",
			mcp.WithDescription(`Search CI/CD run history across repos. Indexes GitHub Actions runs from all repos that appear in session history.

Supports FTS search across workflow names, branches, and failure logs. Use this to:
- Find recent CI failures across projects
- Check if a specific repo's CI is green
- Search failure logs for error patterns
- Correlate CI runs with development sessions

Runs are polled incrementally from GitHub Actions. Failed run logs are indexed for full-text search.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching against workflow, branch, logs). Omit to list recent runs.")),
			mcp.WithString("repo", mcp.Description("Filter by repo (e.g. 'mnemo', 'marcelocantos/mnemo')")),
			mcp.WithString("conclusion", mcp.Description("Filter by conclusion: success, failure, cancelled, skipped")),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_commits",
			mcp.WithDescription(`Search git commits across all indexed repos. Uses FTS5 for keyword search on commit messages. Commits are indexed automatically from repos that appear in session history. Supports cross-repo queries with date range filtering.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching on subject/body). Omit to list recent.")),
			mcp.WithString("repo", mcp.Description("Filter by repo (e.g. 'mnemo', 'marcelocantos/mnemo')")),
			mcp.WithString("author", mcp.Description("Filter by author name or email substring")),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_decisions",
			mcp.WithDescription(`Search past decisions across all sessions. Decisions are automatically detected from proposal + confirmation patterns in conversations (e.g., assistant proposes an approach, user confirms with "yes", "go ahead", "lgtm"). Use this to recall what was decided and why.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list recent.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_restore",
			mcp.WithDescription(`Return the compacted context for a session chain — all compaction summaries across the full /clear-bounded chain, oldest first.

Use this at the start of a session to restore context from a previous run. Given any session ID in a chain (including the current one), it returns the structured summaries (targets, decisions, files touched, open threads) produced by the background compactor across all segments of that chain.

Returns nothing if no compactions have been produced yet (the background compactor runs every 5 minutes on active sessions).`),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Any session ID in the chain (or a prefix).")),
		),
		mcp.NewTool("mnemo_chain",
			mcp.WithDescription(`Retrieve the full /clear-bounded session chain for any session ID.

When a user types /clear in Claude Code, the current JSONL transcript ends and a new one begins within ~300ms. mnemo detects these rollovers by looking for a <command-name>/clear</command-name> marker in the first user message of a successor session combined with a ≤5s gap between the predecessor's last event and the successor's first event.

Given any session ID in a chain, this tool returns the complete ordered chain from oldest to newest, with per-session summaries (topic, timestamps, repo) and the gap/confidence for each link.

If the session has no chain links, a single-element result is returned.`),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Any session ID in the chain (or a prefix)")),
		),
		mcp.NewTool("mnemo_whatsup",
			mcp.WithDescription(`Report which active Claude Code sessions are doing expensive work right now.

Shows per-session CPU%, RSS memory, CPU time, cwd, and resolved transcript path alongside system-wide memory pressure. Cross-references live session PIDs with session metadata (repo, topic, work type) and reads PWD from each process's environment. Results are sorted by CPU% descending so the busiest session appears first.

Use postmortem=true when no live sessions are detected (e.g. after a machine crash) to recover which directories had recent Claude activity based on transcript file mtimes within the last 24 hours.

Use this to answer "what is Claude doing right now?" — especially useful when the machine is hot or fans are spinning.`),
			mcp.WithBoolean("postmortem", mcp.Description("When true and no live sessions exist, report directories with recent Claude activity from transcript mtimes (last 24h).")),
		),
		mcp.NewTool("mnemo_self",
			mcp.WithDescription(`Discover the calling session's ID. Two-phase protocol:

Phase 1: Call with no arguments. Returns a unique nonce. This nonce appears in your transcript as the tool response.
Phase 2: Call again with the nonce. mnemo searches for the session containing it and returns your session ID.

Example: call mnemo_self → get nonce "mnemo:abc123". Call mnemo_self with nonce "mnemo:abc123" → get your session ID. Then use mnemo_read_session to read your own transcript.`),
			mcp.WithString("nonce", mcp.Description("The nonce returned by a previous mnemo_self call. Omit on first call to generate a new nonce.")),
		),
		mcp.NewTool("mnemo_define",
			mcp.WithDescription(`Define a reusable parameterised query template. Templates persist across sessions in SQLite. Use {{param_name}} placeholders in the query. If a template with the same name exists, it is updated.`),
			mcp.WithString("name", mcp.Required(), mcp.Description("Template name (unique identifier)")),
			mcp.WithString("description", mcp.Description("What this template does")),
			mcp.WithString("query", mcp.Required(), mcp.Description("SQL query with {{param}} placeholders")),
			mcp.WithArray("params", mcp.Description("List of parameter names referenced in the query (e.g. [\"days\", \"repo\"])")),
		),
		mcp.NewTool("mnemo_evaluate",
			mcp.WithDescription(`Execute a named query template with parameters. Returns results in the same format as mnemo_query.`),
			mcp.WithString("name", mcp.Required(), mcp.Description("Template name")),
			mcp.WithObject("params", mcp.Description(`Parameter values as key-value pairs (e.g. {"days": "7", "repo": "mnemo"})`)),
		),
		mcp.NewTool("mnemo_list_templates",
			mcp.WithDescription(`List all saved query templates with their names, descriptions, and parameter definitions.`),
		),
		mcp.NewTool("mnemo_discover_patterns",
			mcp.WithDescription(`Analyze transcript history to discover workaround patterns that suggest missing mnemo features.

Detects:
- direct_jsonl_read: Bash commands that read JSONL transcript files directly (bypassing mnemo)
- transcript_grep: grep/rg over transcript directories instead of using mnemo_search
- repeated_query: mnemo_query shapes repeated across 3+ sessions (candidates for templates)
- repeated_search: mnemo_search patterns repeated across 3+ sessions (may warrant dedicated tools)

Returns candidate features with evidence counts, example sessions, and suggested actions.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 90)")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithNumber("min_occurrences", mcp.Description("Minimum pattern occurrences to report (default 3)")),
		),
		mcp.NewTool("mnemo_images",
			mcp.WithDescription(`Search images captured from Claude Code transcripts. Three search modes: (1) text (default) — FTS5 over AI descriptions and OCR text; (2) semantic — embed the query text and find images by meaning using CLIP k-NN (requires embed backend); (3) similar — find visually similar images given an image ID. Use 'text' to find images by paraphrase, 'semantic' for conceptual matches like "architecture diagram", and 'similar' to browse related screenshots.`),
			mcp.WithString("query", mcp.Description("Search query. Used in 'text' and 'semantic' modes. Omit to list recent (text mode).")),
			mcp.WithString("mode", mcp.Description(`Search mode: "text" (FTS5 over descriptions + OCR, default), "semantic" (embed query, k-NN on CLIP vectors), or "similar" (visual similarity to another image, requires similar_to).`)),
			mcp.WithNumber("similar_to", mcp.Description("Image ID to find visually similar images (used with mode='similar').")),
			mcp.WithString("repo", mcp.Description("Filter by repo (session's repo)")),
			mcp.WithString("session", mcp.Description("Filter by session ID prefix")),
			mcp.WithNumber("days", mcp.Description("Recency window (default 90)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
			mcp.WithString("search_fields", mcp.Description(`For text mode: which indexes to search: "both" (default), "description" (AI descriptions only), or "ocr" (extracted text only).`)),
		),
	}
}

// Call executes a tool by name with the given arguments.
// Returns (text, isError, err) where isError means a tool-level error
// (returned to the user) vs err which is a transport/system error.
//
// The connection context is accepted on every call so that tools which
// care about per-connection identity (currently: none; future:
// mnemo_self/mnemo_restore/compactor hooks) can use it. Most tools
// ignore it.
func (h *Handler) Call(_ mcpbridge.ConnContext, name string, args map[string]any) (string, bool, error) {
	switch name {
	case "mnemo_search":
		return h.search(args)
	case "mnemo_sessions":
		return h.sessions(args)
	case "mnemo_read_session":
		return h.readSession(args)
	case "mnemo_query":
		return h.query(args)
	case "mnemo_repos":
		return h.repos(args)
	case "mnemo_recent_activity":
		return h.recentActivity(args)
	case "mnemo_status":
		return h.status(args)
	case "mnemo_stats":
		return h.stats()
	case "mnemo_memories":
		return h.memories(args)
	case "mnemo_skills":
		return h.skills(args)
	case "mnemo_usage":
		return h.usage(args)
	case "mnemo_configs":
		return h.configs(args)
	case "mnemo_audit":
		return h.auditLogs(args)
	case "mnemo_targets":
		return h.targets(args)
	case "mnemo_plans":
		return h.plans(args)
	case "mnemo_docs":
		return h.docs(args)
	case "mnemo_who_ran":
		return h.whoRan(args)
	case "mnemo_permissions":
		return h.permissions(args)
	case "mnemo_prs":
		return h.prs(args)
	case "mnemo_ci":
		return h.ci(args)
	case "mnemo_commits":
		return h.commits(args)
	case "mnemo_decisions":
		return h.decisions(args)
	case "mnemo_restore":
		return h.restore(args)
	case "mnemo_chain":
		return h.chain(args)
	case "mnemo_self":
		return h.self(args)
	case "mnemo_whatsup":
		return h.whatsup(args)
	case "mnemo_define":
		return h.defineTemplate(args)
	case "mnemo_evaluate":
		return h.evaluateTemplate(args)
	case "mnemo_list_templates":
		return h.listTemplates()
	case "mnemo_discover_patterns":
		return h.discoverPatterns(args)
	case "mnemo_images":
		return h.images(args)
	default:
		return "", false, fmt.Errorf("unknown tool: %s", name)
	}
}

func (h *Handler) search(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	sessionType, _ := args["session_type"].(string)
	repoFilter, _ := args["repo"].(string)
	contextBefore := 3
	if cb, ok := args["context_before"].(float64); ok && cb >= 0 {
		contextBefore = int(cb)
	}
	contextAfter := 3
	if ca, ok := args["context_after"].(float64); ok && ca >= 0 {
		contextAfter = int(ca)
	}
	substantiveOnly := true
	if cf, ok := args["context_filter"].(string); ok && cf == "all" {
		substantiveOnly = false
	}
	if query == "" {
		return "query is required", true, nil
	}

	results, err := h.mem.Search(query, limit, sessionType, repoFilter, contextBefore, contextAfter, substantiveOnly)
	if err != nil {
		return fmt.Sprintf("search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No results found. Try different terms — the content may use different vocabulary than expected.", false, nil
	}

	var b strings.Builder
	for _, r := range results {
		sid := r.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		for _, cm := range r.Before {
			fmt.Fprintf(&b, "  [%s] %s\n", cm.Role, cm.Text)
		}
		fmt.Fprintf(&b, ">> [%s] %s | %s | %s | msg:%d\n>> %s\n",
			r.Role, r.Project, sid, r.Timestamp, r.MessageID, r.Text)
		for _, cm := range r.After {
			fmt.Fprintf(&b, "  [%s] %s\n", cm.Role, cm.Text)
		}
		b.WriteByte('\n')
	}
	return b.String(), false, nil
}

func (h *Handler) sessions(args map[string]any) (string, bool, error) {
	sessionType, _ := args["session_type"].(string)
	minMessages := 6
	if m, ok := args["min_messages"].(float64); ok && m >= 0 {
		minMessages = int(m)
	}
	limit := 30
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	projectFilter, _ := args["project"].(string)
	repoFilter, _ := args["repo"].(string)
	workTypeFilter, _ := args["work_type"].(string)

	sessions, err := h.mem.ListSessions(sessionType, minMessages, limit, projectFilter, repoFilter, workTypeFilter)
	if err != nil {
		return fmt.Sprintf("list sessions failed: %v", err), true, nil
	}
	if len(sessions) == 0 {
		return "No sessions found.", false, nil
	}

	live := h.mem.LiveSessions()

	var b strings.Builder
	for _, si := range sessions {
		sid := si.SessionID
		if len(sid) > 10 {
			sid = sid[:10]
		}
		repo := si.Repo
		if repo == "" {
			repo = "-"
		}
		workType := si.WorkType
		if workType == "" {
			workType = "-"
		}
		lastMsg := si.LastMsg
		if len(lastMsg) > 19 {
			lastMsg = lastMsg[:19]
		}
		topic := si.Topic
		if len(topic) > 80 {
			topic = topic[:77] + "..."
		}
		liveness := ""
		if pid, ok := live[si.SessionID]; ok {
			liveness = fmt.Sprintf("  [LIVE pid=%d]", pid)
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s  %d/%d msgs  %s%s\n",
			sid, repo, workType, lastMsg, si.SubstantiveMsgs, si.TotalMsgs, topic, liveness)
	}
	return b.String(), false, nil
}

func (h *Handler) readSession(args map[string]any) (string, bool, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "session_id is required", true, nil
	}
	role, _ := args["role"].(string)
	offset := 0
	if o, ok := args["offset"].(float64); ok && o >= 0 {
		offset = int(o)
	}
	limit := 50
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	messages, err := h.mem.ReadSession(sessionID, role, offset, limit)
	if err != nil {
		return fmt.Sprintf("read session failed: %v", err), true, nil
	}
	if len(messages) == 0 {
		return "No messages found for session " + sessionID, false, nil
	}

	var b strings.Builder
	for _, m := range messages {
		marker := ""
		if m.IsNoise {
			marker = " [noise]"
		}
		fmt.Fprintf(&b, "[%s]%s %s\n%s\n\n", m.Role, marker, m.Timestamp, m.Text)
	}
	return b.String(), false, nil
}

func (h *Handler) query(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "query is required", true, nil
	}

	rows, err := h.mem.Query(query)
	if err != nil {
		return fmt.Sprintf("query failed: %v", err), true, nil
	}
	if len(rows) == 0 {
		return "No rows returned.", false, nil
	}

	var b strings.Builder
	for _, row := range rows {
		for k, v := range row {
			fmt.Fprintf(&b, "%s: %v  ", k, v)
		}
		b.WriteByte('\n')
	}
	return b.String(), false, nil
}

func (h *Handler) repos(args map[string]any) (string, bool, error) {
	filter, _ := args["filter"].(string)

	repos, err := h.mem.ListRepos(filter)
	if err != nil {
		return fmt.Sprintf("list repos failed: %v", err), true, nil
	}
	if len(repos) == 0 {
		return "No repos found.", false, nil
	}

	var b strings.Builder
	for _, r := range repos {
		lastActivity := r.LastActivity
		if len(lastActivity) > 19 {
			lastActivity = lastActivity[:19]
		}
		fmt.Fprintf(&b, "%-45s  %4d sessions  %s  %s\n",
			r.Repo, r.Sessions, lastActivity, r.Path)
	}
	return b.String(), false, nil
}

func (h *Handler) stats() (string, bool, error) {
	stats, err := h.mem.Stats()
	if err != nil {
		return fmt.Sprintf("stats failed: %v", err), true, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Total: %d sessions, %d messages\n\n", stats.TotalSessions, stats.TotalMessages)
	fmt.Fprintf(&b, "%-12s %8s %10s %12s %8s\n", "Type", "Sessions", "Total Msgs", "Substantive", "Noise")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 55))
	for _, ts := range stats.ByType {
		fmt.Fprintf(&b, "%-12s %8d %10d %12d %8d\n",
			ts.SessionType, ts.Sessions, ts.TotalMsgs, ts.SubstantiveMsgs, ts.NoiseMsgs)
	}

	if len(stats.Streams) > 0 {
		fmt.Fprintf(&b, "\n%-16s %8s %8s %6s  %s\n", "Stream", "Indexed", "On Disk", "Drift", "Last Backfill")
		fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 70))
		for _, st := range stats.Streams {
			drift := st.FilesOnDisk - st.FilesIndexed
			fmt.Fprintf(&b, "%-16s %8d %8d %6d  %s\n",
				st.Stream, st.FilesIndexed, st.FilesOnDisk, drift, st.LastBackfill)
		}
	}

	return b.String(), false, nil
}

func (h *Handler) status(args map[string]any) (string, bool, error) {
	days := 7
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)
	maxSessions := 3
	if m, ok := args["max_sessions"].(float64); ok && m > 0 {
		maxSessions = int(m)
	}
	maxExcerpts := 20
	if m, ok := args["max_excerpts"].(float64); ok && m > 0 {
		maxExcerpts = int(m)
	}
	truncateLen := 200
	if t, ok := args["truncate_len"].(float64); ok && t > 0 {
		truncateLen = int(t)
	}

	result, err := h.mem.Status(days, repoFilter, maxSessions, maxExcerpts, truncateLen)
	if err != nil {
		return fmt.Sprintf("status failed: %v", err), true, nil
	}
	if len(result.Repos) == 0 && len(result.Streams) == 0 {
		return "No recent activity found.", false, nil
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) recentActivity(args map[string]any) (string, bool, error) {
	days := 7
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)

	results, err := h.mem.RecentActivity(days, repoFilter)
	if err != nil {
		return fmt.Sprintf("recent activity failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No recent activity found.", false, nil
	}

	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) memories(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	memType, _ := args["type"].(string)
	project, _ := args["project"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchMemories(query, memType, project, limit)
	if err != nil {
		return fmt.Sprintf("memory search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No memories found.", false, nil
	}

	var b strings.Builder
	for _, m := range results {
		proj := m.Project
		if len(proj) > 30 {
			// Trim project path prefix for readability.
			parts := strings.Split(proj, "-")
			if len(parts) > 1 {
				proj = parts[len(parts)-1]
			}
		}
		fmt.Fprintf(&b, "## %s [%s] (%s)\n%s\n\n%s\n\n",
			m.Name, m.MemoryType, proj, m.Description, m.Content)
	}
	return b.String(), false, nil
}

func (h *Handler) skills(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchSkills(query, limit)
	if err != nil {
		return fmt.Sprintf("skill search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No skills found.", false, nil
	}

	var b strings.Builder
	for _, sk := range results {
		fmt.Fprintf(&b, "## %s\n%s\n\n%s\n\n", sk.Name, sk.Description, sk.Content)
	}
	return b.String(), false, nil
}

func (h *Handler) configs(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repoFilter, _ := args["repo"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchClaudeConfigs(query, repoFilter, limit)
	if err != nil {
		return fmt.Sprintf("config search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No CLAUDE.md configs found.", false, nil
	}

	var b strings.Builder
	for _, c := range results {
		fmt.Fprintf(&b, "## %s\n**Path:** %s\n\n%s\n\n---\n\n", c.Repo, c.FilePath, c.Content)
	}
	return b.String(), false, nil
}

func (h *Handler) usage(args map[string]any) (string, bool, error) {
	days := 30
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)
	model, _ := args["model"].(string)
	groupBy, _ := args["group_by"].(string)

	result, err := h.mem.Usage(days, repoFilter, model, groupBy)
	if err != nil {
		return fmt.Sprintf("usage query failed: %v", err), true, nil
	}
	if len(result.Rows) == 0 {
		return "No usage data found.", false, nil
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) auditLogs(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repo, _ := args["repo"].(string)
	skill, _ := args["skill"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchAuditLogs(query, repo, skill, limit)
	if err != nil {
		return fmt.Sprintf("audit log search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No audit log entries found.", false, nil
	}

	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) targets(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repo, _ := args["repo"].(string)
	status, _ := args["status"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchTargets(query, repo, status, limit)
	if err != nil {
		return fmt.Sprintf("targets search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No targets found.", false, nil
	}

	var b strings.Builder
	for _, t := range results {
		statusStr := t.Status
		if statusStr == "" {
			statusStr = "unknown"
		}
		weightStr := ""
		if t.Weight != 0 {
			weightStr = fmt.Sprintf(" weight=%.1f", t.Weight)
		}
		fmt.Fprintf(&b, "## %s %s [%s%s] (%s)\n", t.TargetID, t.Name, statusStr, weightStr, t.Repo)
		if t.Description != "" {
			fmt.Fprintf(&b, "%s\n", t.Description)
		}
		b.WriteByte('\n')
	}
	return b.String(), false, nil
}

func (h *Handler) plans(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repoFilter, _ := args["repo"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchPlans(query, repoFilter, limit)
	if err != nil {
		return fmt.Sprintf("plan search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No plans found.", false, nil
	}

	var b strings.Builder
	for _, p := range results {
		phase := p.Phase
		if phase == "" {
			phase = "(root)"
		}
		fmt.Fprintf(&b, "## %s [phase: %s] (%s)\n\n%s\n\n", p.FilePath, phase, p.Repo, p.Content)
	}
	return b.String(), false, nil
}

func (h *Handler) docs(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repoFilter, _ := args["repo"].(string)
	kind, _ := args["kind"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchDocs(query, repoFilter, kind, limit)
	if err != nil {
		return fmt.Sprintf("doc search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No docs found.", false, nil
	}

	var b strings.Builder
	for _, d := range results {
		title := d.Title
		if title == "" {
			title = filepath.Base(d.FilePath)
		}
		fmt.Fprintf(&b, "## %s [%s] (%s)\n", title, d.Kind, d.Repo)
		fmt.Fprintf(&b, "**Path**: %s\n\n", d.FilePath)
		// Truncate very long content for display.
		content := d.Content
		if len(content) > 2000 {
			content = content[:2000] + "\n…(truncated)"
		}
		fmt.Fprintf(&b, "%s\n\n", content)
	}
	return b.String(), false, nil
}

func (h *Handler) permissions(args map[string]any) (string, bool, error) {
	days := 30
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	result, err := h.mem.Permissions(days, repoFilter, limit)
	if err != nil {
		return fmt.Sprintf("permissions analysis failed: %v", err), true, nil
	}
	if len(result.TopTools) == 0 {
		return "No tool usage data found.", false, nil
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) prs(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repo, _ := args["repo"].(string)
	state, _ := args["state"].(string)
	author, _ := args["author"].(string)
	activityType, _ := args["type"].(string)
	days := 30
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := h.mem.SearchGitHubActivity(query, repo, state, author, activityType, days, limit)
	if err != nil {
		return fmt.Sprintf("GitHub activity search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No PRs or issues found.", false, nil
	}
	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) ci(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repo, _ := args["repo"].(string)
	conclusion, _ := args["conclusion"].(string)
	days := 30
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := h.mem.SearchCI(query, repo, conclusion, days, limit)
	if err != nil {
		return fmt.Sprintf("CI search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No CI runs found.", false, nil
	}
	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) commits(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repo, _ := args["repo"].(string)
	author, _ := args["author"].(string)
	days := 30
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := h.mem.SearchCommits(query, repo, author, days, limit)
	if err != nil {
		return fmt.Sprintf("commits search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No commits found.", false, nil
	}
	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) decisions(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	repo, _ := args["repo"].(string)
	days := 30
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := h.mem.SearchDecisions(query, repo, days, limit)
	if err != nil {
		return fmt.Sprintf("decisions search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No decisions found.", false, nil
	}
	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) self(args map[string]any) (string, bool, error) {
	nonce, _ := args["nonce"].(string)

	if nonce == "" {
		nonce = store.NoncePrefix + uuid.NewString()
		return nonce, false, nil
	}

	if !strings.HasPrefix(nonce, store.NoncePrefix) {
		return "invalid nonce — must be a value returned by a previous mnemo_self call", true, nil
	}

	sessionID, err := h.mem.ResolveNonce(nonce)
	if err != nil {
		return fmt.Sprintf("Nonce not found. The transcript may not be ingested yet — wait a moment and retry. Error: %v", err), true, nil
	}

	return fmt.Sprintf("session_id: %s", sessionID), false, nil
}

func (h *Handler) whoRan(args map[string]any) (string, bool, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "pattern is required", true, nil
	}
	days := 30
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := h.mem.WhoRan(pattern, days, repoFilter, limit)
	if err != nil {
		return fmt.Sprintf("who_ran query failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No matching commands found.", false, nil
	}
	out, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) restore(args map[string]any) (string, bool, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "session_id is required", true, nil
	}

	compactions, err := h.mem.ChainCompactions(sessionID)
	if err != nil {
		return fmt.Sprintf("restore failed: %v", err), true, nil
	}
	if len(compactions) == 0 {
		return "No compactions available yet for this session chain. The background compactor runs every 5 minutes on active sessions.", false, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Compacted context for session chain (%d span(s)):\n\n", len(compactions))

	// Token-budget footer data: measure the running summariser cost
	// against the session's own cost for this chain leaf. Surfaces the
	// 🎯T10 AC6 invariant live.
	var compIn, compOut, sessIn, sessOut int64
	if in, out, err := h.mem.CompactionTokens(sessionID); err == nil {
		compIn, compOut = in, out
	}
	if in, out, err := h.mem.SessionTokens(sessionID); err == nil {
		sessIn, sessOut = in, out
	}

	for i, c := range compactions {
		sid := c.SessionID
		if len(sid) > 10 {
			sid = sid[:10]
		}
		fmt.Fprintf(&b, "── Span %d  [%s]  entries %d..%d  %s ──\n",
			i+1, sid, c.EntryIDFrom, c.EntryIDTo,
			c.GeneratedAt.Format("2006-01-02 15:04"))
		if c.Summary != "" {
			fmt.Fprintf(&b, "Summary: %s\n", c.Summary)
		}
		if c.PayloadJSON != "" && c.PayloadJSON != "{}" {
			var payload struct {
				Targets     []string `json:"targets"`
				Files       []string `json:"files"`
				OpenThreads []string `json:"open_threads"`
				Decisions   []struct {
					What string `json:"what"`
					Why  string `json:"why"`
				} `json:"decisions"`
			}
			if err := json.Unmarshal([]byte(c.PayloadJSON), &payload); err == nil {
				if len(payload.Targets) > 0 {
					fmt.Fprintf(&b, "Targets: %s\n", strings.Join(payload.Targets, ", "))
				}
				if len(payload.Files) > 0 {
					fmt.Fprintf(&b, "Files: %s\n", strings.Join(payload.Files, ", "))
				}
				for _, d := range payload.Decisions {
					fmt.Fprintf(&b, "Decision: %s — %s\n", d.What, d.Why)
				}
				if len(payload.OpenThreads) > 0 {
					fmt.Fprintf(&b, "Open threads: %s\n", strings.Join(payload.OpenThreads, "; "))
				}
			}
		}
		b.WriteByte('\n')
	}

	compTotal := compIn + compOut
	sessTotal := sessIn + sessOut
	if compTotal > 0 || sessTotal > 0 {
		fmt.Fprintf(&b, "── Budget ──\n")
		fmt.Fprintf(&b, "Compaction tokens: %d (prompt %d + output %d)\n", compTotal, compIn, compOut)
		if sessTotal > 0 {
			ratio := 100.0 * float64(compTotal) / float64(sessTotal)
			fmt.Fprintf(&b, "Session tokens: %d  |  Compaction/session: %.2f%%  (target < 10%%)\n", sessTotal, ratio)
		} else {
			fmt.Fprintf(&b, "Session tokens: unknown yet  |  ratio unmeasurable\n")
		}
	}

	return b.String(), false, nil
}

func (h *Handler) chain(args map[string]any) (string, bool, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "session_id is required", true, nil
	}

	links, err := h.mem.Chain(sessionID)
	if err != nil {
		return fmt.Sprintf("chain lookup failed: %v", err), true, nil
	}
	if len(links) == 0 {
		return fmt.Sprintf("No session found for ID %s", sessionID), true, nil
	}

	var b strings.Builder
	if len(links) == 1 {
		fmt.Fprintf(&b, "Single session (no chain links detected):\n")
	} else {
		fmt.Fprintf(&b, "Chain of %d sessions (oldest → newest):\n", len(links))
	}
	for i, link := range links {
		sid := link.SessionID
		if len(sid) > 10 {
			sid = sid[:10]
		}
		repo := link.Repo
		if repo == "" {
			repo = link.Project
		}
		topic := link.Topic
		if len(topic) > 80 {
			topic = topic[:77] + "..."
		}
		first := link.FirstMsg
		if len(first) > 19 {
			first = first[:19]
		}
		last := link.LastMsg
		if len(last) > 19 {
			last = last[:19]
		}
		marker := "  "
		if link.SessionID == sessionID {
			marker = ">>"
		}
		fmt.Fprintf(&b, "%s [%d] %s  %s  %s→%s  %s\n",
			marker, i+1, sid, repo, first, last, topic)
		if i < len(links)-1 && link.Confidence != "" {
			fmt.Fprintf(&b, "       ↓ gap=%dms confidence=%s\n", link.GapMs, link.Confidence)
		}
	}
	return b.String(), false, nil
}

func (h *Handler) whatsup(args map[string]any) (string, bool, error) {
	postmortem, _ := args["postmortem"].(bool)
	result, err := h.mem.Whatsup(postmortem)
	if err != nil {
		return fmt.Sprintf("whatsup failed: %v", err), true, nil
	}

	var b strings.Builder

	if len(result.Sessions) == 0 {
		fmt.Fprintf(&b, "No live Claude Code sessions detected.\n")
	} else {
		fmt.Fprintf(&b, "%-12s %-6s %7s %10s %-12s %-20s %s\n",
			"Session", "PID", "CPU%", "RSS", "WorkType", "Repo", "Topic")
		fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 90))
		for _, s := range result.Sessions {
			sid := s.SessionID
			if len(sid) > 12 {
				sid = sid[:12]
			}
			rss := fmt.Sprintf("%dMB", s.RSSBytes/1024/1024)
			repo := s.Repo
			if len(repo) > 20 {
				repo = repo[:17] + "..."
			}
			topic := s.Topic
			if len(topic) > 40 {
				topic = topic[:37] + "..."
			}
			workType := s.WorkType
			if workType == "" {
				workType = "-"
			}
			fmt.Fprintf(&b, "%-12s %-6d %6.1f%% %10s %-12s %-20s %s\n",
				sid, s.PID, s.CPUPct, rss, workType, repo, topic)
			if s.Cwd != "" {
				fmt.Fprintf(&b, "  cwd: %s\n", s.Cwd)
			}
			switch len(s.Transcripts) {
			case 0:
				// no transcript found — omit
			case 1:
				fmt.Fprintf(&b, "  transcript: %s\n", s.Transcripts[0].Path)
			default:
				fmt.Fprintf(&b, "  transcripts (multiple — disambiguate by mtime/size):\n")
				for _, t := range s.Transcripts {
					fmt.Fprintf(&b, "    %s  mtime=%s size=%d\n",
						t.Path, t.MTime.Format("2006-01-02T15:04:05"), t.Size)
				}
			}
		}
	}

	// Postmortem section.
	if len(result.Postmortem) > 0 {
		fmt.Fprintf(&b, "\nPostmortem (recent claude activity, no live processes):\n")
		for _, e := range result.Postmortem {
			fmt.Fprintf(&b, "  cwd: %s\n", e.Cwd)
			for _, t := range e.Transcripts {
				fmt.Fprintf(&b, "    %s  mtime=%s size=%d\n",
					t.Path, t.MTime.Format("2006-01-02T15:04:05"), t.Size)
			}
		}
	}

	// System metrics section.
	sys := result.System
	if sys.MemPagesFree+sys.MemPagesActive+sys.MemPagesInactive+sys.MemPagesWired > 0 {
		total := sys.MemPagesFree + sys.MemPagesActive + sys.MemPagesInactive + sys.MemPagesWired
		pageSize := int64(4096) // macOS default page size
		fmt.Fprintf(&b, "\nSystem memory (4K pages, pressure=%.1f%%):\n", sys.MemPressurePct)
		fmt.Fprintf(&b, "  Free:     %d pages (%dMB)\n", sys.MemPagesFree, sys.MemPagesFree*pageSize/1024/1024)
		fmt.Fprintf(&b, "  Active:   %d pages (%dMB)\n", sys.MemPagesActive, sys.MemPagesActive*pageSize/1024/1024)
		fmt.Fprintf(&b, "  Inactive: %d pages (%dMB)\n", sys.MemPagesInactive, sys.MemPagesInactive*pageSize/1024/1024)
		fmt.Fprintf(&b, "  Wired:    %d pages (%dMB)\n", sys.MemPagesWired, sys.MemPagesWired*pageSize/1024/1024)
		fmt.Fprintf(&b, "  Total:    %d pages (%dMB)\n", total, total*pageSize/1024/1024)
	}

	return b.String(), false, nil
}

func (h *Handler) defineTemplate(args map[string]any) (string, bool, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true, nil
	}
	query, _ := args["query"].(string)
	if query == "" {
		return "query is required", true, nil
	}
	description, _ := args["description"].(string)

	var paramNames []string
	if raw, ok := args["params"]; ok && raw != nil {
		switch v := raw.(type) {
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					paramNames = append(paramNames, s)
				}
			}
		case []string:
			paramNames = v
		}
	}

	if err := h.mem.DefineTemplate(name, description, query, paramNames); err != nil {
		return fmt.Sprintf("define template failed: %v", err), true, nil
	}
	return fmt.Sprintf("Template %q saved.", name), false, nil
}

func (h *Handler) evaluateTemplate(args map[string]any) (string, bool, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true, nil
	}

	params := make(map[string]string)
	if raw, ok := args["params"]; ok && raw != nil {
		if m, ok := raw.(map[string]any); ok {
			for k, v := range m {
				params[k] = fmt.Sprintf("%v", v)
			}
		}
	}

	rows, err := h.mem.EvaluateTemplate(name, params)
	if err != nil {
		return fmt.Sprintf("evaluate template failed: %v", err), true, nil
	}
	if len(rows) == 0 {
		return "No rows returned.", false, nil
	}

	var b strings.Builder
	for _, row := range rows {
		for k, v := range row {
			fmt.Fprintf(&b, "%s: %v  ", k, v)
		}
		b.WriteByte('\n')
	}
	return b.String(), false, nil
}

func (h *Handler) listTemplates() (string, bool, error) {
	templates, err := h.mem.ListTemplates()
	if err != nil {
		return fmt.Sprintf("list templates failed: %v", err), true, nil
	}
	if len(templates) == 0 {
		return "No templates defined.", false, nil
	}
	out, err := json.MarshalIndent(templates, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *Handler) discoverPatterns(args map[string]any) (string, bool, error) {
	days := 90
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)
	minOccurrences := 3
	if m, ok := args["min_occurrences"].(float64); ok && m > 0 {
		minOccurrences = int(m)
	}

	candidates, err := h.mem.DiscoverPatterns(days, repoFilter, minOccurrences)
	if err != nil {
		return fmt.Sprintf("discover patterns failed: %v", err), true, nil
	}
	if len(candidates) == 0 {
		return fmt.Sprintf("No workaround patterns found in the last %d days (min_occurrences=%d). The transcript index may not have enough data yet, or agents are already using mnemo tools effectively.", days, minOccurrences), false, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Discovered Workaround Patterns (%d days, min_occurrences=%d)\n\n", days, minOccurrences)
	for _, c := range candidates {
		fmt.Fprintf(&b, "## %s (%d sessions)\n", c.PatternType, c.Occurrences)
		fmt.Fprintf(&b, "**Description:** %s\n\n", c.Description)
		fmt.Fprintf(&b, "**Suggestion:** %s\n\n", c.Suggestion)
		if c.Evidence != "" {
			fmt.Fprintf(&b, "**Example evidence:**\n```\n%s\n```\n\n", c.Evidence)
		}
		if len(c.Sessions) > 0 {
			shown := c.Sessions
			if len(shown) > 5 {
				shown = shown[:5]
			}
			fmt.Fprintf(&b, "**Sessions (showing %d of %d):** %s\n\n", len(shown), len(c.Sessions), strings.Join(shown, ", "))
		}
		b.WriteString("---\n\n")
	}
	return b.String(), false, nil
}

func (h *Handler) images(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	mode, _ := args["mode"].(string)
	repo, _ := args["repo"].(string)
	session, _ := args["session"].(string)
	searchFields, _ := args["search_fields"].(string)
	days := 90
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	similarTo := 0
	if st, ok := args["similar_to"].(float64); ok && st > 0 {
		similarTo = int(st)
	}

	if mode == "" {
		mode = "text"
	}

	var results []store.ImageSearchResult
	var err error

	switch mode {
	case "semantic":
		if query == "" {
			return "query is required for semantic mode", true, nil
		}
		results, err = h.mem.SearchImagesSemantic(query, repo, session, days, limit)
		if err != nil {
			return fmt.Sprintf("semantic image search failed: %v", err), true, nil
		}
	case "similar":
		if similarTo <= 0 {
			return "similar_to (image ID) is required for similar mode", true, nil
		}
		results, err = h.mem.SearchImagesSimilar(similarTo, repo, session, days, limit)
		if err != nil {
			return fmt.Sprintf("similar image search failed: %v", err), true, nil
		}
	default: // "text"
		results, err = h.mem.SearchImagesFiltered(query, repo, session, days, limit, searchFields)
		if err != nil {
			return fmt.Sprintf("search images failed: %v", err), true, nil
		}
	}

	if len(results) == 0 {
		switch mode {
		case "semantic":
			return "No images found via semantic search. Ensure embeddings are populated (embed backend requires uv + sentence-transformers).", false, nil
		case "similar":
			return fmt.Sprintf("No similar images found for image ID %d. The image may not have an embedding yet.", similarTo), false, nil
		default:
			if query != "" {
				return "No images found matching query. Descriptions require ANTHROPIC_API_KEY; OCR requires Apple Vision (macOS) or tesseract.", false, nil
			}
			return "No images indexed yet. Images are extracted from transcripts during ingest.", false, nil
		}
	}

	var b strings.Builder
	for _, r := range results {
		img := r.Image
		sid := ""
		if len(r.Occurrences) > 0 {
			sid = r.Occurrences[0].SessionID
			if len(sid) > 8 {
				sid = sid[:8]
			}
		}
		fmt.Fprintf(&b, "[image id=%d] %s %dx%d %s (%.1f KB)",
			img.ID, img.MimeType, img.Width, img.Height, img.PixelFormat,
			float64(img.ByteSize)/1024)
		if img.OriginalPath != "" {
			fmt.Fprintf(&b, " path=%s", img.OriginalPath)
		}
		if sid != "" {
			fmt.Fprintf(&b, " session=%s", sid)
		}
		if r.MatchSource != "" {
			fmt.Fprintf(&b, " match=%s", r.MatchSource)
		}
		if r.Score > 0 {
			fmt.Fprintf(&b, " score=%.3f", r.Score)
		}
		b.WriteByte('\n')
		if r.Description != "" {
			fmt.Fprintf(&b, "  [desc] %s\n", r.Description)
		} else {
			b.WriteString("  [desc] (pending)\n")
		}
		if r.OCRText != "" {
			// Truncate long OCR text for display.
			ocrDisplay := r.OCRText
			if len(ocrDisplay) > 300 {
				ocrDisplay = ocrDisplay[:300] + "…"
			}
			fmt.Fprintf(&b, "  [ocr]  %s\n", ocrDisplay)
		}
		for _, occ := range r.Occurrences {
			occSID := occ.SessionID
			if len(occSID) > 8 {
				occSID = occSID[:8]
			}
			fmt.Fprintf(&b, "  seen in %s (%s) at %s\n", occSID, occ.SourceType, occ.OccurredAt)
		}
		b.WriteByte('\n')
	}
	return b.String(), false, nil
}
