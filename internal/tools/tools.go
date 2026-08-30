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

// ConfigController is the read-only view of mnemo's runtime configuration
// that the vault and themes tools need. Injected from main so the tools
// package stays free of any dependency on the registry or filesystem
// layout.
//
// It used to carry Put as well, for mnemo_config op=write. Configuration
// is file-only now (🎯T156): the daemon watches ~/.mnemo/config.json and
// adopts changes itself, so nothing writes config through the tool
// surface and there is exactly one writer — the user's editor.
type ConfigController interface {
	Get() store.Config
}

type Handler struct {
	resolve          func(username string) (store.Backend, error)
	resolveVault     func(username string) VaultSyncer             // nil when vault disabled
	resolveCompactor func(username string) CompactorHealthReporter // nil when compactor health not wired
	diagRunner       DiagRunner                                    // nil when diagnostics not wired
	cfgCtl           ConfigController                              // nil when mnemo_config disabled
	// budgetCfg + throttleReport feed mnemo_ops op=budget (🎯T140).
	budgetCfg      store.BudgetConfig
	throttleReport func() (level, detail, remediation string)
	seen           sync.Map
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

// SetBudgetWiring configures spend + throttle reporting for mnemo_ops
// op=budget / op=agent_trees (🎯T140). throttleReport may be nil (budget
// still reports; throttle section says "unavailable").
func (h *Handler) SetBudgetWiring(cfg store.BudgetConfig, throttleReport func() (level, detail, remediation string)) {
	h.budgetCfg = cfg
	h.throttleReport = throttleReport
	setOpsBudgetWiring(cfg, throttleReport)
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

By default searches only interactive sessions (excludes subagents, worktrees, ephemeral). Noise messages (interrupts, compaction summaries, tool-loaded markers) are excluded from the index.

SPANS THE INDEX (🎯T144). One search covers messages AND the other indexed corpora, returning typed hits each labelled with the corpus they came from. Default kinds: message, segment, decision, doc, target, commit, pr, memory. On request: plan, config, skill, audit. (These are the exact values the kinds parameter accepts.) Use the kinds parameter to scope. This replaces the per-corpus search tools that were removed: their content is reachable here rather than only through raw SQL.

RANKING ACROSS CORPORA. BM25 scores are not comparable between indexes (the length-normalisation baseline is per-index), so hits are ranked by CALIBRATED QUANTILE: a score maps to its position within its own corpus's distribution, and quantiles compete. Each hit reports a "ranking" field — "calibrated", or "fusion" when that corpus has no fresh distribution yet, in which case "degraded" names the corpus and why. Never raw score comparison.

COST. One FTS query per corpus in scope: 8 by default. Narrow kinds when you know where to look.`),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query — plain words use OR (fuzzy). Use AND/NOT/NEAR/quotes for precise control.")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
			mcp.WithString("session_type", mcp.Description(`Filter by session type (default "interactive"). Values: "interactive", "subagent", "worktree", "ephemeral", "all"`)),
			mcp.WithString("repo", mcp.Description(`Filter by repo. Flexible matching against session working directory and extracted repo name. Accepts: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), host/org/repo ("github.com/marcelocantos/mnemo"), or a path fragment ("~/work/myproject").`)),
			mcp.WithNumber("context_before", mcp.Description("Number of messages before each hit to include (default 3)")),
			mcp.WithNumber("context_after", mcp.Description("Number of messages after each hit to include (default 3)")),
			mcp.WithString("context_filter", mcp.Description(`Filter for context messages. "substantive" (default): only non-noise user/assistant messages. "all": include everything (tool calls, system messages, noise).`)),
			mcp.WithString("expand", mcp.Description(`Expand each hit to a topic segment (🎯T64.10). "none" (default): ±N context only. "segment": smallest enclosing sealed segment. "segment:coarse": top-level span. Default remains "none" until boundary-quality gates clear.`)),
			mcp.WithString("kinds", mcp.Description("Comma-separated corpora to search (\U0001F3AFT144). Omit for the default set: message, segment, decision, doc, target, commit, pr, memory. Also available on request: plan, config, skill, audit \u2014 outside the default because each corpus is another FTS query on the most-called tool in the product. Message hits keep their full shape (context, session, repo filters) and carry the enclosing topic span when one exists.")),
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

Schema: call mnemo_query(describe=true) — or read the mnemo://schema resource —
for the table/column catalogue, generated from the live database so it cannot
drift. Read entries through entries_v, message text through messages_v and doc
content through docs_v: the base tables store those columns compressed.

Join pattern — message with its entry metadata:
  SELECT m.text, e.model, e.input_tokens FROM messages_v m JOIN entries_v e ON e.id = m.entry_id

Token usage query:
  SELECT date(timestamp) AS day, SUM(input_tokens) AS input, SUM(output_tokens) AS output
  FROM entries_v WHERE type = 'assistant' GROUP BY day ORDER BY day DESC

File history — which sessions touched a file:
  SELECT sf.session_id, sf.backup_time, sm.repo
  FROM snapshot_files sf JOIN session_meta sm ON sm.session_id = sf.session_id
  WHERE sf.file_path LIKE '%store.go'

Session types (derived from project path): interactive, subagent, worktree, ephemeral.
is_noise = 1 for interrupts, compaction summaries, tool-loaded markers, slash command markup.
Results capped at 100 rows.

Tip: If you find yourself running the same complex query pattern repeatedly, save it as a template with mnemo_define for reuse.`),
			mcp.WithBoolean("describe", mcp.Description("Return the database schema catalogue instead of running a query; generated from the live database.")),
			mcp.WithString("query",
				mcp.Description("SQL SELECT/WITH query, or sqldeep nested syntax (FROM ... SELECT { ... })")),
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
		mcp.NewTool("mnemo_usage",
			mcp.WithDescription(`Token usage analytics across sessions: input, output, cache-read and cache-creation tokens, with costs.

Two disclosure fields matter as much as the totals, and a total read without them is wrong:
  "unpriced_models" — counted but NOT costed, because the rate card has no entry. Normal for a newly released model, which is exactly the spend you want to see.
  "uncounted" — volume EXCLUDED from every row and total, per source, with the reason. A record with no message id cannot be deduplicated, and deduplication is worth 1.95x-2.83x.

Each row carries "source": "estimated" (from token counts) or "reconciled" (authoritative, Admin API). A top-level "freshness" timestamp bounds ingest lag.

Costing needs {"pricing": {"enabled": true}} in ~/.mnemo/config.json; with it off every model reports unpriced rather than $0.00. Full method: docs/design/token-cost-specification.md.`),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30). Ignored when since/until are supplied.")),
			mcp.WithString("since", mcp.Description("RFC3339 timestamp lower bound (inclusive). Overrides days when set.")),
			mcp.WithString("until", mcp.Description("RFC3339 timestamp upper bound (inclusive). Defaults to now when only since is set.")),
			mcp.WithString("repo", mcp.Description(`Filter by repo. Accepts: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), or path fragment.`)),
			mcp.WithString("model", mcp.Description(`Filter by model prefix (e.g. "claude-opus-4", "claude-sonnet-4")`)),
			mcp.WithString("group_by", mcp.Description(`Group results by: "day" (default), "model", "repo", "session" (one row per Claude Code session ID), or "block" (one row per 5-hour Anthropic billing block, boundaries aligned to UTC and matching what /cost and ccusage report).`)),
		),
		mcp.NewTool("mnemo_compacted_session",
			mcp.WithDescription(`Return the compacted view of a session: its compaction summaries (the dense, durable layer) followed by the addenda tail — the substantive messages past the latest compaction cursor, computed live from the index.

This is the token-volume retrieval form (🎯T72): a converged session is mostly summary plus a bounded tail; a session below the size floor has no summary and the addenda ARE the whole session (its raw entries are its retrieval form). Use this instead of mnemo_read_session when you want the distilled view rather than the raw transcript.`),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID (exact or prefix, consistent with mnemo_read_session).")),
			mcp.WithNumber("addenda_limit", mcp.Description("Max addenda messages past the cursor to include (default 200).")),
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
		threadTool(),
		opsTool(),
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
	case "mnemo_sessions":
		return ch.sessions(args)
	case "mnemo_read_session":
		return ch.readSession(args)
	case "mnemo_query":
		// describe is checked first, and query is deliberately NOT
		// mcp.Required(): a required sibling would force an agent asking
		// for the schema to invent a dummy query, which a strict client
		// rejects outright. The handler enforces "one of" instead.
		if describe, _ := args["describe"].(bool); describe {
			return ch.describeSchema()
		}
		if q, _ := args["query"].(string); strings.TrimSpace(q) == "" {
			return "mnemo_query needs either a query, or describe=true for the schema catalogue", true, nil
		}
		return ch.query(args)
	case "mnemo_repos":
		return ch.repos(args)
	case "mnemo_recent_activity":
		return ch.recentActivity(args)
	case "mnemo_status":
		return ch.status(args)
	case "mnemo_stats":
		return ch.stats()
	case "mnemo_usage":
		return ch.usage(args)
	case "mnemo_compacted_session":
		return ch.compactedSession(args)
	case "mnemo_locate_uuid":
		return ch.locateUUID(args)
	case "mnemo_session_structure":
		return ch.sessionStructure(args)
	case "mnemo_rework_history":
		return ch.reworkHistory(args)
	case "mnemo_thread":
		return ch.threadDispatch(args)
	case "mnemo_ops":
		return ch.opsDispatch(args, h.resolveCompactor, h.diagRunner)
	case "mnemo_vault":
		return ch.vaultDispatch(args, h.cfgCtl)
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

	// 🎯T144: search spans the index by DEFAULT, not only when asked.
	//
	// This routed to the message-only path unless `kinds` was passed,
	// out of caution about regressing the tool that carries 55% of all
	// agent calls. Measurement removed the reason: after dropping a
	// per-search COUNT(*), a 12-corpus search costs ~16ms against ~15ms
	// for messages alone — corpus count is not the cost. Defaulting to
	// one corpus was protecting against an expense that does not exist,
	// at the price of the feature not being on.
	//
	// `expand` still selects the legacy single-corpus renderer, since
	// segment expansion has its own output shape.
	if expand == store.SegmentExpandNone || expand == "" {
		kinds, _ := args["kinds"].(string)
		return h.unifiedSearch(query, kinds, limit, sessionType, repoFilter,
			contextBefore, contextAfter, substantiveOnly)
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
		// 🎯T74: timeout is distinct from SQL errors so agents refine
		// rather than retry the same unbounded statement.
		if store.IsQueryTimeout(err) {
			return fmt.Sprintf("query timed out: %v", err), true, nil
		}
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

// osUserHome is split into a tiny helper so tests can stub home
// resolution if needed; the current callers only need a best-effort
// path for read-side rendering.
func osUserHome() (string, error) {
	return store.EffectiveHome()
}

// describeSchema serves the generated catalogue (🎯T156). It reads the
// live database rather than a checked-in string, so it cannot describe a
// schema the database does not have.
func (h *callHandler) describeSchema() (string, bool, error) {
	doc, err := schemaCatalogue(h.mem.Query)
	if err != nil {
		return fmt.Sprintf("schema unavailable: %v", err), true, nil
	}
	return doc, false, nil
}
