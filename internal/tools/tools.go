// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package tools defines MCP tool schemas and handlers for the mnemo server.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"

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
Results capped at 100 rows.`),
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
		mcp.NewTool("mnemo_self",
			mcp.WithDescription(`Discover the calling session's ID. Two-phase protocol:

Phase 1: Call with no arguments. Returns a unique nonce. This nonce appears in your transcript as the tool response.
Phase 2: Call again with the nonce. mnemo searches for the session containing it and returns your session ID.

Example: call mnemo_self → get nonce "mnemo:abc123". Call mnemo_self with nonce "mnemo:abc123" → get your session ID. Then use mnemo_read_session to read your own transcript.`),
			mcp.WithString("nonce", mcp.Description("The nonce returned by a previous mnemo_self call. Omit on first call to generate a new nonce.")),
		),
	}
}

// Call executes a tool by name with the given arguments.
// Returns (text, isError, err) where isError means a tool-level error
// (returned to the user) vs err which is a transport/system error.
func (h *Handler) Call(name string, args map[string]any) (string, bool, error) {
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
	case "mnemo_who_ran":
		return h.whoRan(args)
	case "mnemo_permissions":
		return h.permissions(args)
	case "mnemo_ci":
		return h.ci(args)
	case "mnemo_self":
		return h.self(args)
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
		fmt.Fprintf(&b, "%s  %s  %s  %s  %d/%d msgs  %s\n",
			sid, repo, workType, lastMsg, si.SubstantiveMsgs, si.TotalMsgs, topic)
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
	if len(result.Repos) == 0 {
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
