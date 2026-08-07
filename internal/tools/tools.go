// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package tools defines MCP tool schemas and handlers for the mnemo server.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/vault"
)

// VaultSyncer is satisfied by *vault.Exporter when vault is configured.
// Defined as an interface so test code can substitute a fake. The tools
// package does import the vault package solely for vault.ErrSyncInFlight
// — a one-symbol sentinel needed to distinguish coalesced calls from
// real errors in the MCP response.
type VaultSyncer interface {
	Sync(ctx context.Context) error
	Path() string
	// Layout returns the active vault_layout ("v1", "both", or
	// "v2"). Surfaced by mnemo_vault_status. (🎯T64.2)
	Layout() string
	// SoakWarnAfter returns the configured soak window after which
	// "both"-layout vaults get the weekly warning. (🎯T64.2)
	SoakWarnAfter() time.Duration
	// StatePath returns the absolute path to the daemon's state.json
	// sidecar so the status tool can read vault_layout_first_seen.
	// (🎯T64.2)
	StatePath() (string, error)
	// RegenerateMigrationDoc unconditionally writes
	// _mnemo/MIGRATION.md, overwriting any existing file. Returns the
	// content written. Used by mnemo_vault_migration_doc(write: true).
	// (🎯T64.2)
	RegenerateMigrationDoc() (string, error)
	// MigrationDocSnapshot returns the MIGRATION.md content mnemo
	// would write for this vault right now, without touching the
	// filesystem. Used by mnemo_vault_migration_doc(write: false).
	// (🎯T64.2)
	MigrationDocSnapshot() string
}

// CompactorHealthReporter is satisfied by *compact.Watcher.
// Surfaced as an interface so this package doesn't need to import
// the compact package (which would create a circular dependency
// hierarchy in tests and main). Backed by Watcher.Health() — see
// internal/compact/watcher.go.
type CompactorHealthReporter interface {
	Health() CompactorHealth
}

// DiagRunner runs mnemo's self-diagnostics for the mnemo_doctor tool
// (🎯T83). Satisfied by *diag.Registry; an interface so this package
// stays decoupled from how the registry is assembled.
type DiagRunner interface {
	Run(ctx context.Context, full bool, now time.Time) diag.Report
}

// CompactorHealth is the externally-visible snapshot returned by the
// mnemo_compactor_status MCP tool (🎯T67). Mirrors
// compact.HealthSnapshot field-for-field; duplicated here to keep
// the tools→compact import direction clean.
type CompactorHealth struct {
	LastScanAt            time.Time
	LastScanCount         int
	Backlog               int
	Quarantined           int
	LastTickAt            time.Time
	LastTickOutcome       string
	InFlightSession       string
	Counts                map[string]int64
	ScanInterval          time.Duration
	TickTimeout           time.Duration
	AddendaBudgetTokens   int64
	MaxCompactionsPerScan int
	MaxTokenRatio         float64
}

// ConfigReport mirrors registry.ReloadReport without importing the
// registry package. The mnemo_config write handler returns these four
// slices verbatim so the caller can see which fields were applied live,
// which require a restart, and which adoption attempts failed despite
// the config write itself succeeding.
type ConfigReport struct {
	Changed         []string
	Adopted         []string
	RequiresRestart []string
	Warnings        []string
}

// ConfigController is the dependency the mnemo_config tool uses to read
// and atomically apply config changes. Injected from main so the tools
// package stays free of any direct dependency on the registry or
// filesystem layout. Get returns a snapshot of the live Config. Put
// validates+persists newCfg to disk and applies in-process adoption
// across every per-user Store.
type ConfigController interface {
	Get() store.Config
	Put(newCfg store.Config) (ConfigReport, error)
}

// Handler handles tool calls, dispatching each incoming call to the
// per-user Store resolved from the call's Username. The resolver is
// injected so the tools package does not need to import the
// registry package (which would create an awkward dependency
// hierarchy in tests and future refactors). seen deduplicates the
// first-call RecordConnectionOpen per (username, MCP session) pair.
type Handler struct {
	resolve          func(username string) (store.Backend, error)
	resolveVault     func(username string) VaultSyncer             // nil when vault disabled
	resolveCompactor func(username string) CompactorHealthReporter // nil when compactor health not wired
	diagRunner       DiagRunner                                    // nil when diagnostics not wired
	cfgCtl           ConfigController                              // nil when mnemo_config disabled
	seen             sync.Map
	// upgradeNotices injects a one-time "mnemo upgraded vN -> vN+1"
	// banner into tool results after a backend swap (🎯T97.6).
	upgradeNotices UpgradeNoticeSource
}

// UpgradeNoticeSource is the tools-facing surface of upgrade.NoticeTracker.
type UpgradeNoticeSource interface {
	Consume(sessionID string) (msg string, ok bool)
}

// NewHandler creates a tool handler that resolves each call's
// backing Store by username via the supplied resolver. Main wires
// this up to Registry.ForUser.
func NewHandler(resolve func(string) (store.Backend, error)) *Handler {
	return &Handler{resolve: resolve}
}

// SetVaultResolver configures a per-user vault syncer resolver. Calling
// this is optional; when not called (or when the resolver returns nil)
// the vault tools report "vault not configured".
func (h *Handler) SetVaultResolver(fn func(string) VaultSyncer) {
	h.resolveVault = fn
}

// SetCompactorResolver configures a per-user compactor health
// resolver. Calling this is optional; when not called (or when the
// resolver returns nil) mnemo_compactor_status reports that the
// watcher's runtime state is not available (typically the daemon's
// startup hasn't completed yet for the calling user).
func (h *Handler) SetCompactorResolver(fn func(string) CompactorHealthReporter) {
	h.resolveCompactor = fn
}

// SetDiagRunner wires the self-diagnostics registry for mnemo_doctor
// (🎯T83). Optional; when unset the tool reports diagnostics unavailable.
func (h *Handler) SetDiagRunner(r DiagRunner) {
	h.diagRunner = r
}

// SetConfigController wires the mnemo_config tool to a live config
// source. Calling this is optional; when not called, mnemo_config
// reports that runtime reconfiguration is not available.
func (h *Handler) SetConfigController(c ConfigController) {
	h.cfgCtl = c
}

// SetUpgradeNotices wires one-time upgrade banners (🎯T97.6).
func (h *Handler) SetUpgradeNotices(n UpgradeNoticeSource) {
	h.upgradeNotices = n
}

// callHandler is the per-call delegate that owns the user-resolved
// Store for the lifetime of one tool invocation. Every per-tool
// method is defined on *callHandler so the method bodies read
// naturally as `h.mem.Search(...)` without threading the store
// through every signature. Call() builds the callHandler once and
// dispatches into the switch.
type callHandler struct {
	mem   store.Backend
	cc    CallContext
	vault VaultSyncer     // nil when vault is not configured for this user
	ctx   context.Context // request context; honours MCP-caller cancellation
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
			mcp.WithString("expand", mcp.Description(`Expand each hit to a topic segment (🎯T64.10). "none" (default): ±N context only. "segment": smallest enclosing sealed segment. "segment:coarse": top-level span. Default remains "none" until boundary-quality gates clear.`)),
		),
		mcp.NewTool("mnemo_segments",
			mcp.WithDescription(`Query hierarchical topic segments (🎯T64.10, folded into summarisation by 🎯T64.11). Segments are precomputed topic-coherent spans over a session's substantive messages.

Query shapes (provide one primary filter):
- query: FTS over segment label/summary — the thematic search shape. Returns spans from many sessions ranked by relevance; use this to find "that thing about X" without knowing the session.
- session_id: topic-AST of one session
- containing_msg_id: spans that enclose a message id
- theme_id / overlaps_theme_a + overlaps_theme_b: DORMANT. Cross-session theme clustering is off (🎯T64.11) — thematic retrieval is served by the query shape above, so these return nothing on a current index.

Spans come from three layers, in precedence order: 'llm' (drawn by the summariser inside a window it compacted), 'compaction' (a window-level span projected from a compaction, covering all summarised history), and 'structural' (always-on local pass, zero egress, covers everything else). The method field on each result says which. expand on mnemo_search stays default-off until quality gates pass.`),
			mcp.WithString("session_id", mcp.Description("Session ID to list segments for")),
			mcp.WithString("theme_id", mcp.Description("Theme ID — return segment members")),
			mcp.WithNumber("containing_msg_id", mcp.Description("Message id — enclosing segments")),
			mcp.WithString("query", mcp.Description("FTS over label/summary")),
			mcp.WithString("overlaps_theme_a", mcp.Description("First theme id for intersection query")),
			mcp.WithString("overlaps_theme_b", mcp.Description("Second theme id for intersection query")),
			mcp.WithBoolean("sealed_only", mcp.Description("Only sealed segments (default false)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 50)")),
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
  patterns (id, pattern_type, signature, occurrence_count, session_count, repos, sessions, first_seen, last_seen, representative_excerpts, computed_at)
    — workaround patterns mined hourly; see mnemo_discover_patterns
    — pattern_type: direct_jsonl_read, transcript_grep, repeated_query, repeated_search
    — repos / sessions / representative_excerpts are JSON arrays; use json_each()
  patterns_fts — FTS5 on pattern_type, signature, representative_excerpts

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
			mcp.WithDescription(`List repositories that have been worked on in Claude Code sessions. Returns, per repo: name, filesystem path, session count, last session activity, last git commit date, and a one-line summary derived from the repo's root CLAUDE.md (first non-blank, non-heading sentence, capped at ~120 chars).

This is the canonical at-a-glance "what repos do I have and what is each one about?" view — sufficient to replace an externally-generated active-projects.md or similar overview file. Summaries refresh automatically when CLAUDE.md is re-indexed.`),
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
			mcp.WithDescription(`Rich status report of recent work across repos, AND the first-line health check for ingest freshness. Returns repos → sessions → conversation excerpts with drill-down offsets, plus a top-level diagnostics block.

The diagnostics block (🎯T75) answers "is the index stale, where, and by how much?" before you resort to grepping ~/.claude/projects or ad-hoc SQL:
- freshness: daemon now_utc, freshest indexed timestamp, and lag.
- divergence: per-stream gap, including transcript_index pending bytes/files.
- transcript_sources: one row per configured project dir — total files, files never ingested, files behind, pending bytes, newest on-disk mtime, and forensic examples of the largest behind files (path, session_id, size, offset, pending bytes, state: new/append_behind/truncated/rewritten).
- repo_diagnostic (when repo is supplied): the Claude project dirs that map to the repo, latest indexed vs latest on-disk mtime, and an explicit note when no source maps to the filter or when on-disk transcripts are newer than the index.

User messages are shown in full. Assistant messages are truncated (default 200 chars). Each message includes its database ID — use mnemo_read_session with offset to retrieve the full text.

Use this when you need context about recent work, OR to check whether fresh transcript content has actually been ingested for a repo. Don't dump the output to the user — use it to inform your own understanding.`),
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

Returns per-period breakdown and totals. Costs come from a fetched model rate card, matched on the EXACT model identifier — there is no prefix matching and no fallback, because both produced large silent errors (opus-4-5 priced as opus-4 is a 3x overcharge; an unknown model priced as Sonnet is how another provider's corpus got billed at Anthropic's rates).

Two fields report what a total leaves out, and both matter more than the total:

"unpriced_models" names models whose tokens are counted but NOT costed, because the rate card has no entry. This is normal for a newly released model — which is exactly the spend you want to see. Their tokens are in the counts; their cost is in nobody's.

"uncounted" reports volume EXCLUDED from every row and total, per source, with the reason. A record with no message id cannot be deduplicated, and deduplication is worth 1.95x-2.83x, so sources that supply no key (Codex, Grok today) are reported separately rather than summed into a figure that claims to be deduplicated. Codex volume is additionally inflated by an ingest artifact.

Pricing requires opting in via {"pricing": {"enabled": true}} in ~/.mnemo/config.json, which lets mnemo fetch the rate card. Without it, token counts are exact and every model reports as unpriced.

Each row includes a "source" field: "estimated" (computed from token counts), "reconciled" (authoritative cost from Anthropic Admin API), or "mixed" (aggregation spans both). Reconciliation requires ANTHROPIC_ADMIN_API_KEY env var; absent by default (all rows report "estimated").

A top-level "freshness" field reports the RFC3339 timestamp of the most recently ingested assistant message, bounding indexer lag for real-time consumers.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30). Ignored when since/until are supplied.")),
			mcp.WithString("since", mcp.Description("RFC3339 timestamp lower bound (inclusive). Overrides days when set.")),
			mcp.WithString("until", mcp.Description("RFC3339 timestamp upper bound (inclusive). Defaults to now when only since is set.")),
			mcp.WithString("repo", mcp.Description(`Filter by repo. Accepts: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), or path fragment.`)),
			mcp.WithString("model", mcp.Description(`Filter by model prefix (e.g. "claude-opus-4", "claude-sonnet-4")`)),
			mcp.WithString("group_by", mcp.Description(`Group results by: "day" (default), "model", "repo", "session" (one row per Claude Code session ID), or "block" (one row per 5-hour Anthropic billing block, boundaries aligned to UTC and matching what /cost and ccusage report).`)),
		),
		mcp.NewTool("mnemo_budget",
			mcp.WithDescription(`Spend against a resetting budget period, with projection and culprits.

Answers "where am I, and am I heading for trouble" for the current calendar month (resets on the 1st, in the configured timezone).

Alerting is on the PROJECTION, not on a threshold already crossed: "at $47/day, 2026-07 exceeds its $500 cap on the 19th" is something you can act on, where "80% consumed" arrives after the decision that caused it. The burn rate is measured over a trailing 7 days rather than the whole period, so a change in behaviour shows up within days instead of being averaged away.

When severity is not "ok", the report names culprit sessions — largest spend first, each resolved to a repo, a working directory, and a live pid where the session is still running. A live session can be attached to (mnemo_session_go) or killed; a finished one cannot, and is labelled as such.

Configure with {"budget": {"monthly_cap_usd": 500, "timezone": "Australia/Sydney", "warn_at_pct": 100}}. With no cap, spend is still reported and nothing alerts.

Carries the same "unpriced_models" and "uncounted" disclosures as mnemo_usage — a budget figure that silently omits a provider is worse than no figure, because it gets believed.`),
		),
		mcp.NewTool("mnemo_agent_trees",
			mcp.WithDescription(`Sub-agent fan-outs reconstructed and costed as a WHOLE, ranked by aggregate tree cost.

For the failure a per-session ranking cannot see: a fan-out where every individual agent looks reasonable and forty together trip the wire. Ranking sessions by cost shows forty modest entries and nothing obviously wrong; only the aggregate at the root makes the shape visible.

Each tree reports the root cause where the transcript records it — the skill that started it, the agent types spawned, the turn that spawned them and when. "You spent a lot" is not actionable; "the release skill spawned 40 agents at 14:03" is.

tree_cost_usd is the fan-out's aggregate; direct_cost_usd is the session's own main-line spend, so a session that is expensive by itself is distinguishable from one that is expensive because of its children. max_depth is reported because a tree three deep is a different problem from a wide shallow one, and nested fan-outs roll up through every level.

Trees still running carry live=true and a pid, and can be stopped (mnemo_session_go, or kill). Finished ones say so — their spend is already incurred.

CLAUDE ONLY, deliberately. The parentage fields come from Claude Code's record shape; Codex records carry no message id and no parent linkage, and a tree built over them would be noise presented as structure. Trees whose spend cannot be priced report priced=false rather than a plausible $0.00.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 7).")),
			mcp.WithString("since", mcp.Description("RFC3339 lower bound. Overrides days.")),
			mcp.WithString("until", mcp.Description("RFC3339 upper bound.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment.")),
			mcp.WithNumber("limit", mcp.Description("Max trees to return (default 20).")),
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
		mcp.NewTool("mnemo_docs",
			mcp.WithDescription(`Search across project documentation files (markdown, plain-text, PDF) indexed from all tracked repos. Covers README, CHANGELOG, design notes, and any files under docs/, design/, notes/, papers/ directories. Deduplicates .md/.pdf pairs with same stem — always prefers .md. Use this to find project documentation, design decisions, and release notes across repos.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list recent.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
			mcp.WithString("kind", mcp.Description("Filter by file kind: md, txt, pdf")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_synthesis",
			mcp.WithDescription(`Search across synthesis documents — analysis, research, design, and planning artifacts that follow the four-dir taxonomy (docs/{papers,design,analysis,plans}) plus docs/audit-log.md and docs/convergence-report.md. Indexed from workspace repos and additional synthesis roots (e.g. ~/think).

Use this instead of mnemo_docs when you want a global view of the user's thinking: cross-repo research themes, recurring design decisions, target retros, external-material summaries. Results include the inferred taxonomy, inline metadata (Date, Status, Target, Source) when present, and the full document content.

Taxonomy values: paper | design | analysis | plans | audit-log | convergence-report.`),
			mcp.WithString("query", mcp.Description("Search query (fuzzy OR matching). Omit to list recent.")),
			mcp.WithString("taxonomy", mcp.Description("Filter by taxonomy: paper, design, analysis, plans, audit-log, convergence-report")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment")),
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
		mcp.NewTool("mnemo_chain",
			mcp.WithDescription(`Retrieve the /clear-bounded session chain for any session ID.

Session chain detection has two layers:
  - DEFINITIVE: rows in session_chains written live by the daemon
    when a proxy connection observes successive sessions. These
    carry mechanism='mcp_connection', confidence='definitive'.
  - HEURISTIC: query-time inference for sessions the daemon never
    saw live (first installs, daemon downtime, sessions that never
    called a mnemo tool). Uses the cwd-most-recent rule.

mode:
  - "auto" (default) — returns the definitive chain; if the query
    session has no definitive predecessor, surfaces heuristic
    candidates.
  - "strict" — definitive only; empty when no connection observed
    the rollover.
  - "candidates" — always returns both definitive rows and
    heuristic candidates, labelled by mechanism, so the caller can
    see ambiguity explicitly.`),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Any session ID in the chain (or a prefix)")),
			mcp.WithString("mode", mcp.Description(`"auto" (default), "strict", or "candidates".`)),
		),
		mcp.NewTool("mnemo_compacted_session",
			mcp.WithDescription(`Return the compacted view of a session: its compaction summaries (the dense, durable layer) followed by the addenda tail — the substantive messages past the latest compaction cursor, computed live from the index.

This is the token-volume retrieval form (🎯T72): a converged session is mostly summary plus a bounded tail; a session below the size floor has no summary and the addenda ARE the whole session (its raw entries are its retrieval form). Use this instead of mnemo_read_session when you want the distilled view rather than the raw transcript.`),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID (exact or prefix, consistent with mnemo_read_session).")),
			mcp.WithNumber("addenda_limit", mcp.Description("Max addenda messages past the cursor to include (default 200).")),
		),
		mcp.NewTool("mnemo_whatsup",
			mcp.WithDescription(`Report which active Claude Code sessions are doing expensive work right now.

Shows per-session CPU%, RSS memory, CPU time, cwd, and resolved transcript path alongside system-wide memory pressure. Cross-references live session PIDs with session metadata (repo, topic, work type) and reads PWD from each process's environment. Results are sorted by CPU% descending so the busiest session appears first.

Use postmortem=true when no live sessions are detected (e.g. after a machine crash) to recover which directories had recent Claude activity based on transcript file mtimes within the last 24 hours.

Use this to answer "what is Claude doing right now?" — especially useful when the machine is hot or fans are spinning.`),
			mcp.WithBoolean("postmortem", mcp.Description("When true and no live sessions exist, report directories with recent Claude activity from transcript mtimes (last 24h).")),
		),
		mcp.NewTool("mnemo_discover_patterns",
			mcp.WithDescription(`Workaround patterns mined from transcript history — places where an agent reached around mnemo instead of through it, and therefore candidate missing features.

Detects:
- direct_jsonl_read: Bash commands that read JSONL transcript files directly (bypassing mnemo)
- transcript_grep: grep/rg over transcript directories instead of using mnemo_search
- repeated_query: the same mnemo_query shape run repeatedly (candidate for a template)
- repeated_search: the same mnemo_search terms run repeatedly (may warrant a dedicated tool)

Served from the persisted patterns table, refreshed hourly by a reconciler, so patterns accumulate a real first_seen instead of being re-derived per call. The reported mine timestamp says how fresh the answer is.

occurrence_count and session_count are different numbers and both are reported: one session that read six transcript files directly is 6 occurrences across 1 session. The emission gate is occurrence >= 3 across >= 2 sessions — a pattern with no corroborating second session is not reported, because a single session's habit is not yet a pattern.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days, applied to last_seen (default 90)")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment; matches any repo the pattern spans")),
			mcp.WithNumber("min_occurrences", mcp.Description("Minimum occurrence count to report (default 3). The >= 2 distinct sessions requirement is not adjustable.")),
		),
		mcp.NewTool("mnemo_session_structure",
			mcp.WithDescription(`Return a structural summary of a session's entry types and content-block shapes.

Answers "what is in this session?" without reading full transcript text. Returns:
- entry_types: count per JSONL entry type (user, assistant, system, progress, ...)
- assistant_stop_reasons: count per stop_reason (end_turn, tool_use, max_tokens, ...)
- system_subtypes: count per $.subtype for system entries
- content_block_kinds: count per content-block type (text, tool_use, tool_result, thinking)
- tool_names: count per tool name in tool_use blocks

Use this to quickly understand a session's shape before deep-reading it, to compare session structures, or to verify that a session contains the content type you expect.`),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID (exact or prefix, consistent with mnemo_read_session)")),
		),
		mcp.NewTool("mnemo_locate_uuid",
			mcp.WithDescription(`Locate an entry by UUID across all sessions. Searches every UUID field in the transcript index and returns the session, entry, and which field matched.

UUID fields searched:
  - entry_uuid       — the entry's own $.uuid
  - parent_uuid      — the parent entry's UUID ($.parentUuid)
  - top_tool_use_id  — entry-level tool use ID ($.toolUseID, for progress/result entries)
  - parent_tool_use_id — entry-level parent tool use ID ($.parentToolUseID)
  - tool_use_id      — a tool_use content block's id inside $.message.content
  - tool_result_id   — a tool_result content block's tool_use_id inside $.message.content

Supports partial UUID prefixes — the first 8 characters are usually enough to uniquely identify an entry.

Returns a structured result with session_id, entry_id, entry type, timestamp, match_kind (which field matched), the full matched UUID, and surrounding context messages. Returns "not found" when no entry matches.`),
			mcp.WithString("uuid", mcp.Required(), mcp.Description("Full UUID or prefix to locate (e.g. \"abc12345\" or the full UUID)")),
			mcp.WithNumber("context_before", mcp.Description("Number of messages before the entry to include (default 3)")),
			mcp.WithNumber("context_after", mcp.Description("Number of messages after the entry to include (default 3)")),
		),
		mcp.NewTool("mnemo_rework_history",
			mcp.WithDescription(`Return prior rework attempts for a bullseye target, ordered most-recent first.

Each result is one compaction span (a background-summarised session segment) in which the target appeared as actively worked on (in targets_active or targets_progressed). Provides: session ID, timestamp, repo, the per-target progress note if the compactor recorded one, the prose summary of the span, and any open threads left unresolved.

Use this to build a rework diagnosis context: the bullseye_rework tool accepts the output as its mnemo_history parameter so the rework agent sees what was tried before, what failed, and what was left open — avoiding repeating the same failed approaches.`),
			mcp.WithString("target_id", mcp.Required(), mcp.Description("Bullseye target ID (e.g. \"T1.5\"). Exact match against targets_active and targets_progressed keys.")),
			mcp.WithString("repo", mcp.Description("Filter by repo name or path fragment (optional).")),
			mcp.WithNumber("limit", mcp.Description("Max attempts to return (default 20).")),
		),
		vaultTool(),
		mcp.NewTool("mnemo_config",
			mcp.WithDescription(`Read or update mnemo's runtime configuration (~/.mnemo/config.json).

Modes:
  - op=read (default): return the current effective config as JSON, plus a list of resolved-paths (workspace_roots, vault_path, synthesis_roots) with ~ expanded.
  - op=write: merge "patch" into the current config, validate, persist to disk, and adopt the change in the running daemon.

Patch semantics: patch is a JSON object with the same shape as ~/.mnemo/config.json. Only keys present in the patch are changed; unset keys are left untouched. Array fields are replaced wholesale — to add or remove a single entry, read the current config first and write the full updated array. To clear a field, set it to its zero value (empty string for vault_path, empty array for the slices).

Hot-reload coverage:
  - vault_path: applied live. The existing vault workers stop, a fresh exporter is built at the new path, and an initial sync starts in the background. Set vault_path to "" to disable vault export entirely.
  - workspace_roots, extra_project_dirs, synthesis_roots: applied live; subsequent ingest passes pick up the new roots.
  - linked_instances: persisted to disk but requires a daemon restart to take effect (the federation client is built once at startup).
  - menu_bar_app: show the multi-purpose native shim's menu-bar status item (default false). The shim itself is always supervised when Mnemo.app is installed — it presents health notifications and consumes the health stream — so this flag is chrome-only, not an on/off switch for the shim. Applied live via SSE, no restart (hiding the status item won't force-quit a running app). The Threads daemon API — mnemo_thread_* tools, the "mnemo thread" CLI, the HTTP thread routes — stays available regardless of this flag.
  - image_embeddings.enabled (🎯T121): opt-in (default false) for the CLIP image embedder. It shells out to "uv run --script tools/embed-clip/embed.py", which resolves PyPI dependencies and downloads CLIP model weights from the HuggingFace Hub (~2 GB of caches), so it stays off until you ask for it — same posture as cost_reconciliation. Applied live (read per attempt), no restart. Image extraction, OCR, descriptions and FTS work regardless; only embedding-based semantic/similar image search depends on this.
  - plugins (🎯T102.2): list of {name, enabled, transport, command|url|script, args?, params?}. Applied live — enable starts an instance, disable tears one down, no restart. Metadata (facets, UI, config_schema) is discovered from each plugin's manifest endpoint, not stored in config. Optional default home: ~/.mnemo/plugins/<name>/.

Response includes which fields changed, which were adopted live, and which require a restart.`),
			mcp.WithString("op", mcp.Description("Operation: \"read\" (default) or \"write\".")),
			mcp.WithObject("patch", mcp.Description("For op=write: object with the keys to update. Same shape as ~/.mnemo/config.json. Omitted keys are left unchanged.")),
		),
		noteTool(),
		threadTool(),
		opsTool(),
		mcp.NewTool("mnemo_session_go",
			mcp.WithDescription(`Reopen a past conversation (🎯T125): resolve a loose reference to one session, open an iTerm2 tab in the directory that session ran in, and resume it there.

Use this when someone wants to pick a conversation back up but does not have its id — which is the normal case. The "session" argument is interpreted by content:
- omitted, "latest", "recent" — the most recent substantive session
- "latest:<scope>" or "latest <scope>" — most recent in a matching repo/project, e.g. "latest mnemo"
- a session id or unique prefix — that session (an exact id always wins)
- anything else — treated as a repo/project fragment, newest match

Reopening happens in the session's OWN working directory, not the caller's: a conversation is about a working tree, and resuming it elsewhere gives the agent context that contradicts its own transcript. A directory that no longer exists is reported rather than silently substituted.

Requires iTerm2 and the daemon's Automation permission (as mnemo_thread_go does). Resumes Claude Code and Grok CLI sessions (claude --resume / grok --resume). Codex/ChatGPT sessions are indexed but have no verified terminal resume — they appear to be Desktop/IDE conversations rather than CLI ones — so they are refused by name rather than opened as a bare shell. Returns {action: focused|spawned, path, session_id, repo, topic, command}.`),
			mcp.WithString("session", mcp.Description(`Which session to reopen: an id/prefix, a repo or project fragment, "latest", or "latest:<scope>". Omit for the most recent session.`)),
		),
	}
}

// Call executes a tool by name with the given arguments.
// Returns (text, isError, err) where isError means a tool-level error
// (returned to the user) vs err which is a transport/system error.
//
// The CallContext carries the MCP session ID (from the Mcp-Session-Id
// header). Most tools ignore it; mnemo_self uses it to bind a Claude
// Code session to its owning MCP session, which the compactor and
// mnemo_restore rely on for /clear-boundary context preservation.
func (h *Handler) Call(ctx context.Context, cc CallContext, name string, args map[string]any) (string, bool, error) {
	mem, err := h.resolve(cc.Username)
	if err != nil {
		return fmt.Sprintf("resolve user %q: %v", cc.Username, err), true, nil
	}
	if cc.MCPSessionID != "" {
		key := cc.Username + "\x00" + cc.MCPSessionID
		if _, loaded := h.seen.LoadOrStore(key, struct{}{}); !loaded {
			mem.RecordConnectionOpen(cc.MCPSessionID, 0, time.Now())
		}
	}
	// Resolve vault syncer for this user (nil when vault not configured).
	var vs VaultSyncer
	if h.resolveVault != nil {
		vs = h.resolveVault(cc.Username)
	}
	ch := &callHandler{mem: mem, cc: cc, vault: vs, ctx: ctx}
	switch name {
	case "mnemo_search":
		return ch.search(args)
	case "mnemo_segments":
		return ch.segments(args)
	case "mnemo_sessions":
		return ch.sessions(args)
	case "mnemo_read_session":
		return ch.readSession(args)
	case "mnemo_query":
		return ch.query(args)
	case "mnemo_repos":
		return ch.repos(args)
	case "mnemo_recent_activity":
		return ch.recentActivity(args)
	case "mnemo_status":
		return ch.status(args)
	case "mnemo_stats":
		return ch.stats()
	case "mnemo_memories":
		return ch.memories(args)
	case "mnemo_skills":
		return ch.skills(args)
	case "mnemo_usage":
		return ch.usage(args)
	case "mnemo_budget":
		return ch.budget()
	case "mnemo_agent_trees":
		return ch.agentTrees(args)
	case "mnemo_configs":
		return ch.configs(args)
	case "mnemo_audit":
		return ch.auditLogs(args)
	case "mnemo_targets":
		return ch.targets(args)
	case "mnemo_docs":
		return ch.docs(args)
	case "mnemo_synthesis":
		return ch.synthesis(args)
	case "mnemo_who_ran":
		return ch.whoRan(args)
	case "mnemo_permissions":
		return ch.permissions(args)
	case "mnemo_prs":
		return ch.prs(args)
	case "mnemo_commits":
		return ch.commits(args)
	case "mnemo_decisions":
		return ch.decisions(args)
	case "mnemo_chain":
		return ch.chain(args)
	case "mnemo_compacted_session":
		return ch.compactedSession(args)
	case "mnemo_whatsup":
		return ch.whatsup(args)
	case "mnemo_discover_patterns":
		return ch.discoverPatterns(args)
	case "mnemo_locate_uuid":
		return ch.locateUUID(args)
	case "mnemo_session_structure":
		return ch.sessionStructure(args)
	case "mnemo_rework_history":
		return ch.reworkHistory(args)
	case "mnemo_thread":
		return ch.threadDispatch(args)
	case "mnemo_note":
		return ch.noteDispatch(args)
	case "mnemo_ops":
		return ch.opsDispatch(args, h.resolveCompactor, h.diagRunner)
	case "mnemo_vault":
		return ch.vaultDispatch(args, h.cfgCtl)
	case "mnemo_config":
		return ch.config(args, h.cfgCtl)
	case "mnemo_session_go":
		return ch.sessionGo(args)
	default:
		return "", false, fmt.Errorf("unknown tool: %s", name)
	}
}

func (h *callHandler) search(args map[string]any) (string, bool, error) {
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
	expand, _ := args["expand"].(string)
	if expand == "" {
		expand = store.DefaultSegmentExpand
	}

	results, err := h.mem.Search(query, limit, sessionType, repoFilter, contextBefore, contextAfter, substantiveOnly)
	if err != nil {
		return fmt.Sprintf("search failed: %v", err), true, nil
	}
	if expand != store.SegmentExpandNone && expand != "" {
		results, err = h.mem.AttachSegmentExpand(results, expand)
		if err != nil {
			return fmt.Sprintf("segment expand failed: %v", err), true, nil
		}
	}
	if len(results) == 0 {
		return "No results found. Try different terms — the content may use different vocabulary than expected.", false, nil
	}

	var b strings.Builder
	for _, r := range results {
		// Vault annotation hits use SessionID to carry the file path; render
		// them as a single "[vault] <path>" header rather than the message
		// format with empty Project/Timestamp/sessionID fields.
		if r.Role == "vault" {
			fmt.Fprintf(&b, ">> [vault] %s\n>> %s\n\n", r.SessionID, r.Text)
			continue
		}
		// Compaction summaries (🎯T72) are the dense layer: render as a
		// "[compaction] <session>" header carrying the summary prose.
		if r.Role == "compaction" {
			sid := r.SessionID
			if len(sid) > 8 {
				sid = sid[:8]
			}
			fmt.Fprintf(&b, ">> [compaction] %s\n>> %s\n\n", sid, r.Text)
			continue
		}
		sid := r.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		for _, cm := range r.Before {
			fmt.Fprintf(&b, "  [%s] %s\n", cm.Role, cm.Text)
		}
		if r.Segment != nil {
			fmt.Fprintf(&b, "  [segment %s L%d] msgs %d–%d · %s\n",
				r.Segment.ID, r.Segment.Level, r.Segment.FromMsgID, r.Segment.ToMsgID, r.Segment.Label)
			if r.Segment.Summary != "" {
				fmt.Fprintf(&b, "  [segment summary] %s\n", r.Segment.Summary)
			}
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

func (h *callHandler) segments(args map[string]any) (string, bool, error) {
	q := store.SegmentQuery{}
	q.SessionID, _ = args["session_id"].(string)
	q.ThemeID, _ = args["theme_id"].(string)
	if v, ok := args["containing_msg_id"].(float64); ok && v > 0 {
		q.ContainingMsgID = int(v)
	}
	q.FTSQuery, _ = args["query"].(string)
	q.OverlapsThemeA, _ = args["overlaps_theme_a"].(string)
	q.OverlapsThemeB, _ = args["overlaps_theme_b"].(string)
	if v, ok := args["sealed_only"].(bool); ok {
		q.SealedOnly = v
	}
	if l, ok := args["limit"].(float64); ok && l > 0 {
		q.Limit = int(l)
	}
	segs, err := h.mem.QuerySegments(q)
	if err != nil {
		return fmt.Sprintf("segments query failed: %v", err), true, nil
	}
	if len(segs) == 0 {
		return "No segments matched.", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Topic segments (%d)\n\n", len(segs))
	for _, s := range segs {
		sealed := "open"
		if s.Sealed {
			sealed = "sealed"
		}
		fmt.Fprintf(&b, "## %s · L%d · %s\n", s.ID, s.Level, sealed)
		fmt.Fprintf(&b, "- session: `%s`\n", s.SessionID)
		fmt.Fprintf(&b, "- range: msgs %d–%d\n", s.FromMsgID, s.ToMsgID)
		if s.ParentID != "" {
			fmt.Fprintf(&b, "- parent: `%s`\n", s.ParentID)
		}
		if s.Label != "" {
			fmt.Fprintf(&b, "- label: %s\n", s.Label)
		}
		if s.Summary != "" {
			fmt.Fprintf(&b, "- summary: %s\n", s.Summary)
		}
		fmt.Fprintf(&b, "- method: %s · confidence: %.2f\n\n", s.Method, s.Confidence)
	}
	return b.String(), false, nil
}

func (h *callHandler) sessions(args map[string]any) (string, bool, error) {
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

func (h *callHandler) readSession(args map[string]any) (string, bool, error) {
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

func (h *callHandler) query(args map[string]any) (string, bool, error) {
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

func (h *callHandler) repos(args map[string]any) (string, bool, error) {
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
		// Date column shows last_commit when available (truer signal
		// for "is this repo alive?"), falling back to last_activity.
		dateCol := r.LastCommit
		dateLabel := "commit"
		if dateCol == "" {
			dateCol = lastActivity
			dateLabel = "session"
		}
		fmt.Fprintf(&b, "%-45s  %4d sessions  %s %s  %s\n",
			r.Repo, r.Sessions, dateLabel, dateCol, r.Path)
		if r.Summary != "" {
			marker := ""
			switch r.SummaryVerdict {
			case "stale":
				marker = "  [stale, reviewed " + r.SummaryReviewedAt + "]"
			case "rewritten":
				marker = "  [needs rewrite, reviewed " + r.SummaryReviewedAt + "]"
			}
			fmt.Fprintf(&b, "    %s%s\n", r.Summary, marker)
		}
	}
	return b.String(), false, nil
}

func (h *callHandler) stats() (string, bool, error) {
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

func (h *callHandler) status(args map[string]any) (string, bool, error) {
	days := 7
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)
	maxSessions := 2
	if m, ok := args["max_sessions"].(float64); ok && m > 0 {
		maxSessions = int(m)
	}
	maxExcerpts := 6
	if m, ok := args["max_excerpts"].(float64); ok && m > 0 {
		maxExcerpts = int(m)
	}
	truncateLen := 160
	if t, ok := args["truncate_len"].(float64); ok && t > 0 {
		truncateLen = int(t)
	}

	result, err := h.mem.Status(days, repoFilter, maxSessions, maxExcerpts, truncateLen)
	if err != nil {
		return fmt.Sprintf("status failed: %v", err), true, nil
	}
	// 🎯T75: always return the report — the diagnostics block is the
	// answer to "is the index stale/behind?" precisely when there's no
	// recent activity to show. Only short-circuit if even diagnostics
	// couldn't be computed.
	if result.Diagnostics == nil && len(result.Repos) == 0 && len(result.Streams) == 0 {
		return "No recent activity found.", false, nil
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) recentActivity(args map[string]any) (string, bool, error) {
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

func (h *callHandler) memories(args map[string]any) (string, bool, error) {
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

func (h *callHandler) skills(args map[string]any) (string, bool, error) {
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

func (h *callHandler) configs(args map[string]any) (string, bool, error) {
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

func (h *callHandler) usage(args map[string]any) (string, bool, error) {
	p := store.UsageParams{}
	if d, ok := args["days"].(float64); ok && d > 0 {
		p.Days = int(d)
	}
	p.Since, _ = args["since"].(string)
	p.Until, _ = args["until"].(string)
	p.RepoFilter, _ = args["repo"].(string)
	p.Model, _ = args["model"].(string)
	p.GroupBy, _ = args["group_by"].(string)

	result, err := h.mem.Usage(p)
	if err != nil {
		return fmt.Sprintf("usage query failed: %v", err), true, nil
	}
	// Only "no data" when there is genuinely none. A query whose every
	// record was quarantined for want of a dedup key has data — it has a
	// LOT of data, which is the point — and reporting that as absence is
	// the silent-exclusion failure this whole feature exists to prevent
	// (🎯T135). Fall through so the caller sees what was withheld and why.
	if len(result.Rows) == 0 && len(result.Uncounted) == 0 {
		return "No usage data found.", false, nil
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) budget() (string, bool, error) {
	cfg, err := store.LoadConfig()
	if err != nil {
		return fmt.Sprintf("read config: %v", err), true, nil
	}
	st, err := h.mem.BudgetStatusNow(cfg.Budget, time.Now())
	if err != nil {
		return fmt.Sprintf("budget status failed: %v", err), true, nil
	}
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) agentTrees(args map[string]any) (string, bool, error) {
	p := store.AgentTreeParams{}
	if d, ok := args["days"].(float64); ok && d > 0 {
		p.Days = int(d)
	}
	if l, ok := args["limit"].(float64); ok && l > 0 {
		p.Limit = int(l)
	}
	p.Since, _ = args["since"].(string)
	p.Until, _ = args["until"].(string)
	p.RepoFilter, _ = args["repo"].(string)

	trees, err := h.mem.AgentTrees(p)
	if err != nil {
		return fmt.Sprintf("agent tree query failed: %v", err), true, nil
	}
	if len(trees) == 0 {
		return "No sub-agent fan-outs found in this window.", false, nil
	}
	out, err := json.MarshalIndent(trees, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) auditLogs(args map[string]any) (string, bool, error) {
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

func (h *callHandler) targets(args map[string]any) (string, bool, error) {
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

func (h *callHandler) docs(args map[string]any) (string, bool, error) {
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

func (h *callHandler) synthesis(args map[string]any) (string, bool, error) {
	query, _ := args["query"].(string)
	taxonomy, _ := args["taxonomy"].(string)
	repoFilter, _ := args["repo"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	results, err := h.mem.SearchSynthesis(query, taxonomy, repoFilter, limit)
	if err != nil {
		return fmt.Sprintf("synthesis search failed: %v", err), true, nil
	}
	if len(results) == 0 {
		return "No synthesis docs found.", false, nil
	}

	var b strings.Builder
	for _, d := range results {
		title := d.Title
		if title == "" {
			title = filepath.Base(d.FilePath)
		}
		fmt.Fprintf(&b, "## %s [%s] (%s)\n", title, d.Taxonomy, d.Repo)
		fmt.Fprintf(&b, "**Path**: %s\n", d.FilePath)
		if d.DocDate != "" {
			fmt.Fprintf(&b, "**Date**: %s  ", d.DocDate)
		}
		if d.DocStatus != "" {
			fmt.Fprintf(&b, "**Status**: %s  ", d.DocStatus)
		}
		if d.DocTarget != "" {
			fmt.Fprintf(&b, "**Target**: %s  ", d.DocTarget)
		}
		if d.DocSource != "" {
			fmt.Fprintf(&b, "**Source**: %s", d.DocSource)
		}
		fmt.Fprintf(&b, "\n\n")
		content := d.Content
		if len(content) > 2000 {
			content = content[:2000] + "\n…(truncated)"
		}
		fmt.Fprintf(&b, "%s\n\n", content)
	}
	return b.String(), false, nil
}

func (h *callHandler) permissions(args map[string]any) (string, bool, error) {
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

func (h *callHandler) prs(args map[string]any) (string, bool, error) {
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

func (h *callHandler) commits(args map[string]any) (string, bool, error) {
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

func (h *callHandler) decisions(args map[string]any) (string, bool, error) {
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

func validTodoStatus(s string) bool {
	switch s {
	case "open", "done", "cancelled", "in_progress":
		return true
	}
	return false
}

func validTodoPriority(s string) bool {
	switch s {
	case "highest", "high", "medium", "low", "lowest", "none":
		return true
	}
	return false
}

func (h *callHandler) notePost(args map[string]any) (string, bool, error) {
	inbox, _ := args["inbox"].(string)
	body, _ := args["body"].(string)
	if inbox == "" {
		return "inbox is required", true, nil
	}
	if body == "" {
		return "body is required", true, nil
	}
	p := store.NotePostParams{Inbox: inbox, Body: body, ConnectionID: h.cc.MCPSessionID}
	p.FromSession, _ = args["from_session"].(string)
	p.FromRepo, _ = args["from_repo"].(string)
	n, err := h.mem.PostNote(p)
	if err != nil {
		return fmt.Sprintf("note post failed: %v", err), true, nil
	}
	return fmt.Sprintf("Posted note #%d to inbox %s", n.ID, n.Inbox), false, nil
}

func (h *callHandler) noteRecv(args map[string]any) (string, bool, error) {
	inbox, _ := args["inbox"].(string)
	if inbox == "" {
		return "inbox is required", true, nil
	}
	p := store.NoteRecvParams{
		Inbox:        inbox,
		ConnectionID: h.cc.MCPSessionID,
		UnreadOnly:   true,
		MarkRead:     true,
	}
	if v, ok := args["unread_only"].(bool); ok {
		p.UnreadOnly = v
	}
	if v, ok := args["mark_read"].(bool); ok {
		p.MarkRead = v
	}
	if v, ok := args["limit"].(float64); ok && v > 0 {
		p.Limit = int(v)
	}
	notes, err := h.mem.RecvNotes(p)
	if err != nil {
		return fmt.Sprintf("note recv failed: %v", err), true, nil
	}
	if len(notes) == 0 {
		return "No notes.", false, nil
	}
	out, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) noteList(args map[string]any) (string, bool, error) {
	p := store.NoteListParams{ConnectionID: h.cc.MCPSessionID, Days: 30}
	p.Inbox, _ = args["inbox"].(string)
	if v, ok := args["days"].(float64); ok && v > 0 {
		p.Days = int(v)
	}
	notes, err := h.mem.ListNotes(p)
	if err != nil {
		return fmt.Sprintf("note list failed: %v", err), true, nil
	}
	if len(notes) == 0 {
		return "No notes.", false, nil
	}
	out, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) whoRan(args map[string]any) (string, bool, error) {
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

func (h *callHandler) restore(args map[string]any) (string, bool, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "session_id is required", true, nil
	}

	// Walk the session chain and union compactions across every
	// session in the chain. Under the activity-driven compactor
	// (🎯T59), compactions are tagged with connection_id only
	// best-effort: a session that never had an MCP binding still
	// produces compactions, just with NULL connection_id. So we
	// resolve via session_id (which always exists on a compaction
	// row) rather than via connection_id (which may not).
	//
	// session_chains drives the chain walk, so /clear boundaries
	// are honoured regardless of whether any connection ever
	// observed both halves.
	var sessionIDs []string
	if chain, err := h.mem.Chain(sessionID); err == nil && len(chain) > 0 {
		for _, link := range chain {
			sessionIDs = append(sessionIDs, link.SessionID)
		}
	} else {
		sessionIDs = []string{sessionID}
	}

	var compactions []store.Compaction
	seen := map[int64]bool{}
	for _, sid := range sessionIDs {
		cc, err := h.mem.ListCompactions(sid, 0)
		if err != nil {
			continue
		}
		for _, c := range cc {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			compactions = append(compactions, c)
		}
	}

	if len(compactions) == 0 {
		return "No compactions available yet for this session. The background compactor scans session activity periodically; a session below the substantive-message threshold or outside the recency window will not yet have a compaction.", false, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Compacted context for this connection (%d span(s)):\n\n", len(compactions))

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
				Targets           []string          `json:"targets"`
				TargetsActive     []string          `json:"targets_active"`
				TargetsProgressed map[string]string `json:"targets_progressed"`
				TargetsNext       string            `json:"targets_next"`
				Files             []string          `json:"files"`
				OpenThreads       []string          `json:"open_threads"`
				Decisions         []struct {
					What string `json:"what"`
					Why  string `json:"why"`
				} `json:"decisions"`
			}
			if err := json.Unmarshal([]byte(c.PayloadJSON), &payload); err == nil {
				if len(payload.TargetsActive) > 0 {
					fmt.Fprintf(&b, "Targets active: %s\n", strings.Join(payload.TargetsActive, ", "))
				} else if len(payload.Targets) > 0 {
					fmt.Fprintf(&b, "Targets: %s\n", strings.Join(payload.Targets, ", "))
				}
				if len(payload.TargetsProgressed) > 0 {
					ids := make([]string, 0, len(payload.TargetsProgressed))
					for id := range payload.TargetsProgressed {
						ids = append(ids, id)
					}
					sort.Strings(ids)
					b.WriteString("Targets progressed:\n")
					for _, id := range ids {
						fmt.Fprintf(&b, "  - %s: %s\n", id, payload.TargetsProgressed[id])
					}
				}
				if payload.TargetsNext != "" {
					fmt.Fprintf(&b, "Next target: %s\n", payload.TargetsNext)
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

// compactedSession implements mnemo_compacted_session (🎯T72): the
// distilled retrieval form of a session — compaction summaries plus the
// live addenda tail past the latest cursor.
func (h *callHandler) compactedSession(args map[string]any) (string, bool, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "session_id is required", true, nil
	}
	limit := 200
	if l, ok := args["addenda_limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	v, err := h.mem.CompactedView(sessionID, limit)
	if err != nil {
		return fmt.Sprintf("compacted view failed: %v", err), true, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Compacted view of session %s\n\n", v.SessionID)
	if len(v.Summaries) == 0 {
		b.WriteString("(no compaction yet — below the size floor; the addenda below ARE the session's retrieval form)\n\n")
	} else {
		b.WriteString("== Compaction summaries (durable layer) ==\n")
		for i, sm := range v.Summaries {
			fmt.Fprintf(&b, "%d. %s\n", i+1, sm)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "== Addenda: %d message(s), ~%d tokens past cursor msg:%d ==\n",
		len(v.Addenda), v.AddendaTokens, v.Cursor)
	for _, m := range v.Addenda {
		text := m.Text
		if len(text) > 500 {
			text = text[:497] + "..."
		}
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, text)
	}
	return b.String(), false, nil
}

func (h *callHandler) chain(args map[string]any) (string, bool, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "session_id is required", true, nil
	}
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "auto"
	}

	links, err := h.mem.Chain(sessionID)
	if err != nil {
		return fmt.Sprintf("chain lookup failed: %v", err), true, nil
	}
	if len(links) == 0 {
		return fmt.Sprintf("No session found for ID %s", sessionID), true, nil
	}

	// Definitive chain has a predecessor if the head of the chain is
	// not the queried session. Otherwise, the queried session has no
	// definitive predecessor — heuristic fallback may be relevant.
	hasDefinitivePred := len(links) > 1 && links[0].SessionID != sessionID

	var candidates []store.ChainCandidate
	runHeuristic := mode == "candidates" || (mode == "auto" && !hasDefinitivePred)
	if runHeuristic {
		if cc, err := h.mem.InferChainHeuristic(sessionID, 3); err == nil {
			candidates = cc
		}
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
	if len(candidates) > 0 {
		fmt.Fprintf(&b, "\nHeuristic candidates (cwd_most_recent):\n")
		for _, c := range candidates {
			pid := c.PredecessorID
			if len(pid) > 10 {
				pid = pid[:10]
			}
			fmt.Fprintf(&b, "  ? %s  gap=%dms  confidence=%s  mechanism=%s\n",
				pid, c.GapMs, c.Confidence, c.Mechanism)
		}
	}
	return b.String(), false, nil
}

func (h *callHandler) whatsup(args map[string]any) (string, bool, error) {
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

func (h *callHandler) discoverPatterns(args map[string]any) (string, bool, error) {
	days := 90
	if d, ok := args["days"].(float64); ok && d > 0 {
		days = int(d)
	}
	repoFilter, _ := args["repo"].(string)
	minOccurrences := store.PatternEmitMinOccurrences
	if m, ok := args["min_occurrences"].(float64); ok && m > 0 {
		minOccurrences = int(m)
	}

	candidates, err := h.mem.DiscoverPatterns(days, repoFilter, minOccurrences)
	if err != nil {
		return fmt.Sprintf("discover patterns failed: %v", err), true, nil
	}
	if len(candidates) == 0 {
		return fmt.Sprintf("No workaround patterns found in the last %d days (min_occurrences=%d, min_sessions=%d). The transcript index may not have enough data yet, or agents are already using mnemo tools effectively.",
			days, minOccurrences, store.PatternEmitMinSessions), false, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Discovered Workaround Patterns (%d days, min_occurrences=%d, min_sessions=%d)\n\n",
		days, minOccurrences, store.PatternEmitMinSessions)
	// Freshness is disclosed rather than assumed: these rows come from
	// the patterns table, refreshed hourly by a reconciler, so a caller
	// comparing against something they did five minutes ago needs to
	// know how old the mine is.
	if len(candidates) > 0 && candidates[0].ComputedAt != "" {
		fmt.Fprintf(&b, "*Mined %s (persisted; refreshed hourly).*\n\n", candidates[0].ComputedAt)
	}
	for _, c := range candidates {
		fmt.Fprintf(&b, "## %s (%d occurrences across %d sessions)\n", c.PatternType, c.Occurrences, c.SessionCount)
		fmt.Fprintf(&b, "**Description:** %s\n\n", c.Description)
		fmt.Fprintf(&b, "**Suggestion:** %s\n\n", c.Suggestion)
		if c.FirstSeen != "" || c.LastSeen != "" {
			fmt.Fprintf(&b, "**Seen:** %s → %s\n\n", c.FirstSeen, c.LastSeen)
		}
		if len(c.Repos) > 0 {
			fmt.Fprintf(&b, "**Repos:** %s\n\n", strings.Join(c.Repos, ", "))
		}
		if c.Evidence != "" {
			fmt.Fprintf(&b, "**Example evidence:**\n```\n%s\n```\n\n", c.Evidence)
		}
		if len(c.Sessions) > 0 {
			shown := c.Sessions
			if len(shown) > 5 {
				shown = shown[:5]
			}
			// Denominator is SessionCount, not len(c.Sessions): the
			// stored list is itself capped, so counting it would report
			// the sample size as the population.
			fmt.Fprintf(&b, "**Sessions (showing %d of %d):** %s\n\n",
				len(shown), c.SessionCount, strings.Join(shown, ", "))
		}
		b.WriteString("---\n\n")
	}
	return b.String(), false, nil
}

func (h *callHandler) sessionStructure(args map[string]any) (string, bool, error) {
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "session_id is required", true, nil
	}
	result, err := h.mem.SessionStructure(sessionID)
	if err != nil {
		return fmt.Sprintf("session_structure failed: %v", err), true, nil
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) locateUUID(args map[string]any) (string, bool, error) {
	prefix, _ := args["uuid"].(string)
	if prefix == "" {
		return "uuid is required", true, nil
	}
	contextBefore := 3
	if cb, ok := args["context_before"].(float64); ok && cb >= 0 {
		contextBefore = int(cb)
	}
	contextAfter := 3
	if ca, ok := args["context_after"].(float64); ok && ca >= 0 {
		contextAfter = int(ca)
	}

	matches, err := h.mem.LocateUUID(prefix, contextBefore, contextAfter)
	if err != nil {
		return fmt.Sprintf("locate_uuid failed: %v", err), true, nil
	}
	if len(matches) == 0 {
		return fmt.Sprintf("UUID %q not found in any session.", prefix), false, nil
	}

	out, err := json.MarshalIndent(matches, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

func (h *callHandler) reworkHistory(args map[string]any) (string, bool, error) {
	targetID, _ := args["target_id"].(string)
	if targetID == "" {
		return "target_id is required", true, nil
	}
	repo, _ := args["repo"].(string)
	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	attempts, err := h.mem.ReworkHistory(targetID, repo, limit)
	if err != nil {
		return fmt.Sprintf("rework_history failed: %v", err), true, nil
	}
	if len(attempts) == 0 {
		return fmt.Sprintf("No prior rework attempts found for target %s.", targetID), false, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Prior rework attempts for %s (%d span(s)):\n\n", targetID, len(attempts))
	for i, a := range attempts {
		sid := a.SessionID
		if len(sid) > 10 {
			sid = sid[:10]
		}
		fmt.Fprintf(&b, "── Attempt %d  [%s]  %s", i+1, sid, a.GeneratedAt)
		if a.Repo != "" {
			fmt.Fprintf(&b, "  (%s)", a.Repo)
		}
		b.WriteString(" ──\n")
		if a.Progress != "" {
			fmt.Fprintf(&b, "Progress: %s\n", a.Progress)
		}
		if a.Summary != "" {
			fmt.Fprintf(&b, "Summary: %s\n", a.Summary)
		}
		if len(a.OpenThreads) > 0 {
			fmt.Fprintf(&b, "Open threads: %s\n", strings.Join(a.OpenThreads, "; "))
		}
		b.WriteByte('\n')
	}
	return b.String(), false, nil
}

// vaultNotConfigured is the standard response when vault_path is absent.
const vaultNotConfigured = `Vault export is not configured. Mnemo runs fine without it — vault export is an optional Obsidian/Logseq integration that materialises sessions, decisions, memories, plans, and targets as Markdown notes in a directory you choose, and re-ingests human annotations you add below the <!-- mnemo:generated --> fence so they become searchable across all your transcripts.

To enable now without restarting the daemon:
  mnemo_config(op="write", patch={"vault_path": "~/Documents/mnemo-vault"})

Or edit ~/.mnemo/config.json and restart the daemon.`

func (h *callHandler) vaultSync() (string, bool, error) {
	if h.vault == nil {
		return vaultNotConfigured, false, nil
	}
	start := time.Now()
	if err := h.vault.Sync(h.ctx); err != nil {
		if errors.Is(err, vault.ErrSyncInFlight) {
			// Another sync (periodic ticker or initial pass) is already
			// running. Reporting this honestly is important: a falsely
			// successful 0s response would mislead callers into thinking
			// their sync request ran.
			return fmt.Sprintf("vault sync skipped: another sync is already in flight; this call did not run.\nVault path: %s",
				h.vault.Path()), false, nil
		}
		return fmt.Sprintf("vault sync failed: %v", err), true, nil
	}
	return fmt.Sprintf("vault sync complete in %s.\nVault path: %s",
		time.Since(start).Round(time.Millisecond), h.vault.Path()), false, nil
}

func (h *callHandler) vaultStatus(ctl ConfigController) (string, bool, error) {
	if h.vault == nil {
		return vaultNotConfigured, false, nil
	}
	vaultPath := h.vault.Path()
	sections := []string{"sessions", "decisions", "memories", "skills", "configs", "plans", "targets", "ci", "prs", "repos"}
	var b strings.Builder
	fmt.Fprintf(&b, "vault path: %s\n\n", vaultPath)

	// Indexing scope (🎯T64.1): report the configured surface so the
	// user can audit what mnemo can see. When ctl is wired (the normal
	// runtime), resolve against the live config; otherwise fall back
	// to the defaults so the section is never silently missing.
	var cfg store.Config
	if ctl != nil {
		cfg = ctl.Get()
	}
	scope := cfg.ResolvedVaultIndexingScope(vaultPath)
	ignoreFile := cfg.ResolvedVaultIndexingIgnoreFile()
	b.WriteString("Indexing scope:\n")
	fmt.Fprintf(&b, "  scope:       %s", scope)
	if cfg.VaultIndexingScope == "" {
		b.WriteString("  (auto-default)")
	}
	b.WriteString("\n")
	if scope == store.VaultIndexingScopeIncludes {
		fmt.Fprintf(&b, "  includes:    %v\n", cfg.VaultIndexingIncludes)
	}
	ignorePath := filepath.Join(vaultPath, ignoreFile)
	ignoreState := "absent"
	if _, err := os.Stat(ignorePath); err == nil {
		ignoreState = "present"
	}
	fmt.Fprintf(&b, "  ignore_file: %s (%s)\n\n", ignoreFile, ignoreState)

	// Vault layout block (🎯T64.2): active layout, soak elapsed under
	// "both", and the recommendation state machine.
	layout := h.vault.Layout()
	soak := h.vault.SoakWarnAfter()
	b.WriteString("Layout:\n")
	fmt.Fprintf(&b, "  active:                  %s", layout)
	if cfg.VaultLayout == "" {
		b.WriteString("  (auto-default)")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  soak_warn_after_hours:   %d\n", int(soak.Hours()))
	statePath, perr := h.vault.StatePath()
	var hoursInBoth time.Duration
	var firstSeenBoth time.Time
	if perr == nil {
		if st, err := store.LoadState(statePath); err == nil {
			firstSeenBoth = st.LayoutFirstSeen(store.VaultLayoutBoth)
			if !firstSeenBoth.IsZero() {
				hoursInBoth = time.Since(firstSeenBoth)
			}
		}
	}
	if !firstSeenBoth.IsZero() {
		days := int((hoursInBoth.Hours() / 24) + 0.5)
		fmt.Fprintf(&b, "  first_seen_both:         %s\n", firstSeenBoth.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "  days_in_both:            %d\n", days)
	}
	rec := vaultLayoutRecommendation(layout, hoursInBoth, soak, store.HasV1Leftovers(vaultPath))
	if rec != "" {
		fmt.Fprintf(&b, "  recommendation:          %s\n", rec)
	}
	b.WriteString("\n")

	// PKM profile block (🎯T64.5): which tool dialect the exporter
	// renders for, and — when auto-detected — the signal file + mtime
	// that decided it, so the user can spot a mis-detection.
	prof := cfg.DetectVaultProfile(vaultPath)
	b.WriteString("PKM profile:\n")
	fmt.Fprintf(&b, "  active:      %s", prof.Profile)
	switch prof.Source {
	case "config":
		b.WriteString("  (configured)")
	case "auto":
		b.WriteString("  (auto-detected)")
	case "default":
		b.WriteString("  (default; no signal file found)")
	}
	b.WriteString("\n")
	if prof.SignalFile != "" {
		fmt.Fprintf(&b, "  signal:      %s (mtime %s)\n",
			prof.SignalFile, prof.SignalMtime.UTC().Format(time.RFC3339))
	}
	if len(prof.Alternatives) > 0 {
		fmt.Fprintf(&b, "  alternates:  %s\n", strings.Join(prof.Alternatives, ", "))
	}
	b.WriteString("\n")

	// Bridges block (🎯T64.6): configured collection→anchor mappings and
	// any errors from the last sync.
	if len(cfg.VaultBridges) > 0 {
		b.WriteString("Bridges:\n")
		names := make([]string, 0, len(cfg.VaultBridges))
		for n := range cfg.VaultBridges {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "  %-12s → %s\n", n, cfg.VaultBridges[n])
		}
		if sp, perr := h.vault.StatePath(); perr == nil {
			if st, err := store.LoadState(sp); err == nil && len(st.BridgeErrors) > 0 {
				b.WriteString("  errors:\n")
				for _, be := range st.BridgeErrors {
					fmt.Fprintf(&b, "    %s (%s): %s\n", be.Name, be.AnchorPath, be.Reason)
				}
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("Notes on disk:\n")
	total := 0
	for _, sec := range sections {
		count := countMDFiles(filepath.Join(vaultPath, sec))
		total += count
		fmt.Fprintf(&b, "  %-12s %d\n", sec, count)
	}
	fmt.Fprintf(&b, "  %-12s %d\n", "total", total)
	return b.String(), false, nil
}

// vaultBridgeList implements mnemo_vault_bridge_list (🎯T64.6): the
// configured collection→anchor bridges, whether each has been written to
// disk yet (from the state.json written-bridge record), and any errors
// recorded on the last sync.
func (h *callHandler) vaultBridgeList(ctl ConfigController) (string, bool, error) {
	if h.vault == nil {
		return vaultNotConfigured, false, nil
	}
	var cfg store.Config
	if ctl != nil {
		cfg = ctl.Get()
	}

	// Load the written-bridge record + errors from state.json.
	var written map[string]string
	var bridgeErrs []store.BridgeError
	if sp, perr := h.vault.StatePath(); perr == nil {
		if st, err := store.LoadState(sp); err == nil {
			written = st.WrittenBridges
			bridgeErrs = st.BridgeErrors
		}
	}

	type bridgeView struct {
		Collection string `json:"collection"`
		Anchor     string `json:"anchor"`
		Written    bool   `json:"written"`
		Known      bool   `json:"known_collection"`
	}
	out := struct {
		VaultPath string              `json:"vault_path"`
		MaxLinks  int                 `json:"max_links"`
		Bridges   []bridgeView        `json:"bridges"`
		Errors    []store.BridgeError `json:"errors"`
	}{
		VaultPath: h.vault.Path(),
		MaxLinks:  cfg.ResolvedVaultBridgesMaxLinks(),
		Bridges:   []bridgeView{},
		Errors:    bridgeErrs,
	}
	names := make([]string, 0, len(cfg.VaultBridges))
	for n := range cfg.VaultBridges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		_, isWritten := written[n]
		out.Bridges = append(out.Bridges, bridgeView{
			Collection: n,
			Anchor:     cfg.VaultBridges[n],
			Written:    isWritten,
			Known:      store.IsVaultBridgeCollection(n),
		})
	}
	if out.Errors == nil {
		out.Errors = []store.BridgeError{}
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(buf), false, nil
}

// doctor implements mnemo_doctor (🎯T83): it runs the full self-
// diagnostics suite and returns the per-check report as JSON. The same
// report backs the dashboard health page and the OS notifications.
func (h *callHandler) doctor(runner DiagRunner) (string, bool, error) {
	if runner == nil {
		return "Diagnostics not available (the daemon was started without the diagnostics registry wired).", false, nil
	}
	rep := runner.Run(h.ctx, true, time.Now())
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(out), false, nil
}

// vaultLayoutRecommendation returns the recommendation string for the
// vault_layout state machine (🎯T64.2):
//
//   - "both" + elapsed < soak           → "still within soak"
//   - "both" + elapsed ≥ soak           → "opt into v2"
//   - "v2"   + v1 root-level dirs exist → "run gc_legacy"
//   - otherwise                         → ""  (no recommendation owed)
//
// The empty-string row covers vault_layout="v1" (no migration owed),
// "v2" with the legacy dirs already cleaned up (migration complete),
// and "both" with first_seen.both not yet stamped (transient first-
// sync state — the next sync will populate the timestamp and flip
// the recommendation to one of the warn rows).
func vaultLayoutRecommendation(layout string, hoursInBoth, soak time.Duration, v1LeftoversPresent bool) string {
	switch layout {
	case store.VaultLayoutBoth:
		if hoursInBoth == 0 {
			return ""
		}
		if hoursInBoth < soak {
			return "still within soak"
		}
		return "opt into v2"
	case store.VaultLayoutV2:
		if v1LeftoversPresent {
			return "run gc_legacy"
		}
	}
	return ""
}

func (h *callHandler) vaultMigrationDoc(args map[string]any) (string, bool, error) {
	if h.vault == nil {
		return vaultNotConfigured, false, nil
	}
	write, _ := args["write"].(bool)
	if !write {
		return h.vault.MigrationDocSnapshot(), false, nil
	}
	content, err := h.vault.RegenerateMigrationDoc()
	if err != nil {
		return fmt.Sprintf("regenerate MIGRATION.md failed: %v", err), true, nil
	}
	return fmt.Sprintf("wrote MIGRATION.md (%d bytes) to %s/_mnemo/MIGRATION.md\n\n%s",
		len(content), h.vault.Path(), content), false, nil
}

// compactorStatus implements mnemo_compactor_status (🎯T67). It
// returns a snapshot of the compactor watcher's runtime state so
// callers can answer "is the compactor working?" without grepping
// the daemon log.
//
// The resolver may be nil (daemon started without compactor health
// wired) or may return nil (the user's workers haven't started
// yet); both surface as a helpful "not available" message rather
// than an error.
func (h *callHandler) compactorStatus(resolve func(username string) CompactorHealthReporter) (string, bool, error) {
	if resolve == nil {
		return "Compactor status not available (daemon was started without the compactor health resolver wired).", false, nil
	}
	reporter := resolve(h.cc.Username)
	if reporter == nil {
		return "Compactor status not available (the watcher hasn't started for this user yet — usually the daemon is still booting).", false, nil
	}
	hs := reporter.Health()

	var b strings.Builder
	b.WriteString("Compactor watcher status:\n\n")

	now := time.Now()
	formatAge := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return fmt.Sprintf("%s (%s ago)", t.Format(time.RFC3339), now.Sub(t).Round(time.Second))
	}

	fmt.Fprintf(&b, "  last_scan_at:        %s\n", formatAge(hs.LastScanAt))
	fmt.Fprintf(&b, "  last_scan_count:     %d\n", hs.LastScanCount)
	fmt.Fprintf(&b, "  backlog:             %d (sessions whose addenda exceed the budget)\n", hs.Backlog)
	fmt.Fprintf(&b, "  quarantined:         %d (sessions excluded after repeated failures — 🎯T77)\n", hs.Quarantined)
	fmt.Fprintf(&b, "  last_tick_at:        %s\n", formatAge(hs.LastTickAt))
	if hs.LastTickOutcome != "" {
		fmt.Fprintf(&b, "  last_tick_outcome:   %s\n", hs.LastTickOutcome)
	}
	if hs.InFlightSession != "" {
		fmt.Fprintf(&b, "  in_flight_session:   %s\n", hs.InFlightSession)
	} else {
		b.WriteString("  in_flight_session:   (idle)\n")
	}

	b.WriteString("\nLifetime tick counts:\n")
	outcomes := []string{"compacted", "nothing_to_compact", "budget_exceeded", "failed", "timeout", "rate_limited", "deferred"}
	for _, o := range outcomes {
		fmt.Fprintf(&b, "  %-20s %d\n", o+":", hs.Counts[o])
	}
	// 🎯T72: the failure ratio is the load-bearing health signal now that
	// compactions are rare and durable. A healthy steady state keeps
	// failed well below compacted (target ≤ 1:5).
	if comp := hs.Counts["compacted"]; comp > 0 {
		fmt.Fprintf(&b, "  %-20s %.2f (failed/compacted; healthy ≤ 0.20)\n",
			"failure_ratio:", float64(hs.Counts["failed"])/float64(comp))
	}

	b.WriteString("\nConfiguration:\n")
	fmt.Fprintf(&b, "  scan_interval:            %s\n", hs.ScanInterval)
	fmt.Fprintf(&b, "  tick_timeout:             %s\n", hs.TickTimeout)
	fmt.Fprintf(&b, "  addenda_budget_tokens:    %d\n", hs.AddendaBudgetTokens)
	fmt.Fprintf(&b, "  max_compactions_per_scan: %d\n", hs.MaxCompactionsPerScan)
	fmt.Fprintf(&b, "  max_token_ratio:          %.2f\n", hs.MaxTokenRatio)

	// Health heuristic (🎯T71): the watcher is genuinely stuck only when
	// neither the per-scan loop NOR the per-tick loop has progressed in
	// a while. A single scan can return up to MaxCompactionsPerScan
	// candidates and then spend many minutes ticking through them
	// (TickTimeout per LLM call), so last_scan_at can legitimately sit
	// well past 2× scan_interval on a busy daemon. Use last_tick_at as
	// the proof-of-life signal alongside last_scan_at — if either is
	// recent, we're working, not wedged.
	if !hs.LastScanAt.IsZero() {
		stale := 2 * hs.ScanInterval
		scanStale := now.Sub(hs.LastScanAt) > stale
		tickStale := hs.LastTickAt.IsZero() || now.Sub(hs.LastTickAt) > stale
		if scanStale && tickStale {
			tickAge := "never"
			if !hs.LastTickAt.IsZero() {
				tickAge = now.Sub(hs.LastTickAt).Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(&b, "\n⚠ Watcher appears stuck: last scan was %s ago and last tick was %s (> 2× scan_interval).\n",
				now.Sub(hs.LastScanAt).Round(time.Second),
				tickAge)
		}
	}

	return b.String(), false, nil
}

// divergence implements mnemo_divergence (🎯T68.4): a uniform
// per-stream actual-vs-desired gap report across the derived data
// plane. Streams without a cheap gap metric are shown as "unknown"
// rather than a fabricated number.
func (h *callHandler) divergence() (string, bool, error) {
	rows := h.mem.StreamDivergences()

	var b strings.Builder
	b.WriteString("Derived-stream divergence (gap to fixed point):\n\n")
	if len(rows) == 0 {
		b.WriteString("  (no streams reported)\n")
		return b.String(), false, nil
	}

	converged := 0
	for _, d := range rows {
		if !d.Known {
			fmt.Fprintf(&b, "  %-22s unknown — %s\n", d.Stream+":", d.Note)
			continue
		}
		if d.Gap == 0 {
			converged++
		}
		last := d.LastReconciled
		if last == "" {
			last = "never"
		}
		fmt.Fprintf(&b, "  %-22s gap=%d %s (last reconciled: %s)\n",
			d.Stream+":", d.Gap, d.Unit, last)
		if d.Note != "" {
			fmt.Fprintf(&b, "  %-22s   %s\n", "", d.Note)
		}
	}
	fmt.Fprintf(&b, "\n%d stream(s) reported; %d converged (gap=0).\n", len(rows), converged)
	return b.String(), false, nil
}

// vaultGC implements mnemo_vault_gc (🎯T68.6): inspect orphans and
// optionally clean up manifest rows whose file is gone. Never deletes
// files on disk — that path needs higher-level policy and is not in
// this version.
func (h *callHandler) vaultGC(args map[string]any) (string, bool, error) {
	vaultPath, _ := args["vault_path"].(string)
	if vaultPath == "" {
		return "", false, fmt.Errorf("vault_path is required")
	}
	confirm, _ := args["confirm"].(bool)

	rep, err := h.mem.ScanVaultOrphans(vaultPath)
	if err != nil {
		return "", false, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Vault GC scan (%s):\n\n", vaultPath)
	fmt.Fprintf(&b, "  manifest_path_missing: %d (manifest rows whose file is gone)\n",
		len(rep.ManifestPathMissing))
	fmt.Fprintf(&b, "  disk_not_in_manifest:  %d (*.md files with no manifest entry — informational only)\n",
		len(rep.DiskNotInManifest))

	if len(rep.ManifestPathMissing) > 0 {
		b.WriteString("\nManifest rows pointing at missing files:\n")
		for i, m := range rep.ManifestPathMissing {
			if i >= 20 {
				fmt.Fprintf(&b, "  … and %d more\n", len(rep.ManifestPathMissing)-i)
				break
			}
			fmt.Fprintf(&b, "  %s [%s/%s]\n", m.NotePath, m.EntityKind, m.EntityID)
		}
	}
	if len(rep.DiskNotInManifest) > 0 {
		b.WriteString("\nDisk files with no manifest entry (informational — never auto-deleted):\n")
		for i, p := range rep.DiskNotInManifest {
			if i >= 20 {
				fmt.Fprintf(&b, "  … and %d more\n", len(rep.DiskNotInManifest)-i)
				break
			}
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}

	if !confirm {
		if len(rep.ManifestPathMissing) > 0 {
			b.WriteString("\n[dry-run] Re-run with confirm=true to remove the manifest rows above.\n")
		} else {
			b.WriteString("\nNothing to clean up.\n")
		}
		return b.String(), false, nil
	}

	removed := 0
	for _, m := range rep.ManifestPathMissing {
		if err := h.mem.RemoveVaultManifestRow(m.NotePath); err != nil {
			fmt.Fprintf(&b, "\n⚠ failed to remove manifest row for %s: %v", m.NotePath, err)
			continue
		}
		removed++
	}
	fmt.Fprintf(&b, "\nRemoved %d manifest row(s). Disk side untouched.\n", removed)
	return b.String(), false, nil
}

// countMDFiles counts *.md files recursively under dir.
func countMDFiles(dir string) int {
	count := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			count++
		}
		return nil
	})
	return count
}

// config implements the mnemo_config tool. ctl is nil when main did not
// wire up the controller; in that case both modes report unavailability
// rather than crashing.
//
// The write path merges patch onto a fresh snapshot of the live config.
// Only keys present in the patch JSON are applied — unspecified keys
// preserve their current value. This makes "configure vault_path"
// safe to call without re-stating workspace_roots/etc.
func (h *callHandler) config(args map[string]any, ctl ConfigController) (string, bool, error) {
	if ctl == nil {
		return "mnemo_config not available (server started without config controller)", true, nil
	}
	op, _ := args["op"].(string)
	if op == "" {
		op = "read"
	}
	switch op {
	case "read":
		return renderConfigRead(ctl.Get(), h.callerHome()), false, nil
	case "write":
		patch, _ := args["patch"].(map[string]any)
		if len(patch) == 0 {
			return "op=write requires a non-empty \"patch\" object", true, nil
		}
		current := ctl.Get()
		merged, err := mergeConfigPatch(current, patch)
		if err != nil {
			return fmt.Sprintf("patch invalid: %v", err), true, nil
		}
		report, err := ctl.Put(merged)
		if err != nil {
			return fmt.Sprintf("write failed: %v", err), true, nil
		}
		return renderConfigWrite(merged, report), false, nil
	default:
		return fmt.Sprintf("unknown op %q: expected \"read\" or \"write\"", op), true, nil
	}
}

// callerHome resolves the home directory for the request's
// Username, falling back to the daemon's own home if the user is
// unset or unresolvable. The read path uses this only for ~
// expansion in the displayed "Resolved paths" block; on Windows
// Service deployments the daemon runs as LocalSystem, and a per-
// user mnemo_config call should see vault_path resolved against the
// caller's home, not the service account's.
func (h *callHandler) callerHome() string {
	if h.cc.Username != "" {
		if home, err := store.ResolveHomeFor(h.cc.Username); err == nil {
			return home
		}
	}
	home, _ := osUserHome()
	return home
}

// knownConfigKeys is the closed set of JSON keys mnemo_config accepts
// in a patch. Anything else is rejected up-front so a typo like
// "vaultpath" produces an error rather than being silently dropped by
// json.Unmarshal's unknown-field handling.
// knownConfigKeys is the set of top-level keys a mnemo_config patch may
// set. It is derived from store.Config's json tags via reflection so it can
// never drift from the struct: adding a Config field automatically makes it
// patchable. A hand-maintained parallel list silently left menu_bar_app and
// threads_root unpatchable (the tool rejected them as "unknown config keys")
// after they were added to the struct but not the list — reflection removes
// that failure mode entirely.
var knownConfigKeys = configKeySet()

// configKeySet reflects over store.Config's exported fields and collects
// their json key names (the part before any comma; "-" and untagged fields
// are skipped).
func configKeySet() map[string]struct{} {
	keys := make(map[string]struct{})
	t := reflect.TypeOf(store.Config{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" || name == "-" {
			continue
		}
		keys[name] = struct{}{}
	}
	return keys
}

// mergeConfigPatch round-trips current through JSON so the patch's
// keys overlay only the fields the user actually specified. This is
// simpler and safer than reflective field-by-field merging: any new
// Config field added later participates automatically as long as it
// has a json tag, and the resulting Config goes through json.Unmarshal
// which catches obvious type mismatches early.
//
// CONTRACT: every exported Config field must carry a `json:"name"`
// tag. A field tagged `json:"-"` (runtime-only / derived) is silently
// zeroed on every patch round-trip, even when the patch does not
// touch it. If a future Config field needs to survive merges without
// being patchable, switch this function to reflective field-by-field
// merge.
//
// Patch keys are validated against knownConfigKeys before merging so
// typos surface as tool errors instead of silent no-ops. Add a new
// entry to knownConfigKeys when adding a Config field.
func mergeConfigPatch(current store.Config, patch map[string]any) (store.Config, error) {
	var unknown []string
	for k := range patch {
		if _, ok := knownConfigKeys[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return store.Config{}, fmt.Errorf("unknown config keys: %s", strings.Join(unknown, ", "))
	}
	curJSON, err := json.Marshal(current)
	if err != nil {
		return store.Config{}, fmt.Errorf("marshal current: %w", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(curJSON, &asMap); err != nil {
		return store.Config{}, fmt.Errorf("unmarshal current: %w", err)
	}
	if asMap == nil {
		asMap = map[string]any{}
	}
	for k, v := range patch {
		asMap[k] = v
	}
	mergedJSON, err := json.Marshal(asMap)
	if err != nil {
		return store.Config{}, fmt.Errorf("marshal merged: %w", err)
	}
	var merged store.Config
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		return store.Config{}, fmt.Errorf("decode merged: %w", err)
	}
	return merged, nil
}

func renderConfigRead(cfg store.Config, home string) string {
	var b strings.Builder
	b.WriteString("Current mnemo config (~/.mnemo/config.json):\n\n")
	data, _ := json.MarshalIndent(cfg, "", "  ")
	b.Write(data)
	b.WriteString("\n\n")
	b.WriteString("Resolved paths:\n")
	fmt.Fprintf(&b, "  workspace_roots:    %v\n", cfg.ResolvedWorkspaceRoots())
	fmt.Fprintf(&b, "  synthesis_roots:    %v\n", cfg.ResolvedSynthesisRoots())
	vp := cfg.ResolvedVaultPath(home)
	if vp == "" {
		b.WriteString("  vault_path:         (vault disabled)\n")
	} else {
		fmt.Fprintf(&b, "  vault_path:         %s\n", vp)
	}
	return b.String()
}

func renderConfigWrite(merged store.Config, report ConfigReport) string {
	var b strings.Builder
	b.WriteString("mnemo config updated and persisted to ~/.mnemo/config.json.\n\n")
	if len(report.Changed) == 0 {
		b.WriteString("No field values changed (patch matched the existing config).\n")
	} else {
		fmt.Fprintf(&b, "Changed fields:          %s\n", strings.Join(report.Changed, ", "))
		if len(report.Adopted) > 0 {
			fmt.Fprintf(&b, "Adopted live:            %s\n", strings.Join(report.Adopted, ", "))
		}
		if len(report.RequiresRestart) > 0 {
			fmt.Fprintf(&b, "Requires daemon restart: %s\n", strings.Join(report.RequiresRestart, ", "))
		}
		if len(report.Warnings) > 0 {
			b.WriteString("\nAdoption warnings (config persisted but live adoption failed):\n")
			for _, w := range report.Warnings {
				fmt.Fprintf(&b, "  - %s\n", w)
			}
		}
	}
	b.WriteString("\nNew config:\n")
	data, _ := json.MarshalIndent(merged, "", "  ")
	b.Write(data)
	b.WriteString("\n")
	return b.String()
}

// osUserHome is split into a tiny helper so tests can stub home
// resolution if needed; the current callers only need a best-effort
// path for read-side rendering.
func osUserHome() (string, error) {
	return store.EffectiveHome()
}
