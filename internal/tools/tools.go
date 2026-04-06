// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package tools defines MCP tool schemas and handlers for the mnemo server.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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
			mcp.WithDescription("Search across Claude Code session transcripts. By default searches only interactive sessions (excludes subagents, worktrees, ephemeral). Noise messages (interrupts, compaction summaries, tool-loaded markers) are excluded from the index."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query (FTS5 syntax: words, phrases in quotes, OR, NOT)")),
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
			mcp.WithDescription(`Run a read-only SQL query against the transcript database.

Tables:
  messages (id, session_id, project, role, text, timestamp, type, is_noise)
  messages_fts — FTS5 virtual table (excludes noise). Use: WHERE messages_fts MATCH 'terms'
  sessions — view: session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg
  ingest_state (path, offset)

Session types (derived from project path): interactive, subagent, worktree, ephemeral.
is_noise = 1 for interrupts, compaction summaries, tool-loaded markers, slash command markup.
Results capped at 100 rows.`),
			mcp.WithString("query", mcp.Required(), mcp.Description("SQL SELECT query")),
		),
		mcp.NewTool("mnemo_repos",
			mcp.WithDescription(`List repositories that have been worked on in Claude Code sessions. Returns repo name, filesystem path, session count, and last activity. Use this to discover repo locations, find related projects, or get an overview of recent work.`),
			mcp.WithString("filter", mcp.Description(`Optional filter. Supports: bare name ("mnemo"), org/repo ("marcelocantos/mnemo"), path fragment ("/work/github"), or glob ("marcelocantos/sql*"). Omit to list all repos.`)),
		),
		mcp.NewTool("mnemo_stats",
			mcp.WithDescription("Show transcript index statistics — sessions and messages broken down by session type, with noise vs substantive counts."),
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

// Register adds all mnemo tools to an MCP server, using the handler for calls.
// Used by the daemon for direct MCP serving (if needed in future).
func Register(s *server.MCPServer, h *Handler) {
	for _, tool := range Definitions() {
		name := tool.Name
		s.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text, isErr, err := h.Call(name, req.GetArguments())
			if err != nil {
				return nil, err
			}
			if isErr {
				return mcp.NewToolResultError(text), nil
			}
			return mcp.NewToolResultText(text), nil
		})
	}
}

// CallResult is the wire format for tool call results over RPC.
type CallResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
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
	case "mnemo_stats":
		return h.stats()
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
		return "No results found.", false, nil
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
