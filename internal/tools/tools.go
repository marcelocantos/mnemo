// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package tools registers MCP tools for the mnemo server.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/store"
)

// Register adds all mnemo tools to the MCP server.
func Register(s *server.MCPServer, mem *store.Store) {
	s.AddTool(
		mcp.NewTool("mnemo_search",
			mcp.WithDescription("Search across Claude Code session transcripts. By default searches only interactive sessions (excludes subagents, worktrees, ephemeral). Noise messages (interrupts, compaction summaries, tool-loaded markers) are excluded from the index."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query (FTS5 syntax: words, phrases in quotes, OR, NOT)")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
			mcp.WithString("session_type", mcp.Description(`Filter by session type (default "interactive"). Values: "interactive", "subagent", "worktree", "ephemeral", "all"`)),
		),
		handleSearch(mem),
	)

	s.AddTool(
		mcp.NewTool("mnemo_sessions",
			mcp.WithDescription("List transcript sessions, sorted by most recent activity. By default shows only interactive sessions with at least 6 substantive messages."),
			mcp.WithString("session_type", mcp.Description(`Filter by session type (default "interactive"). Values: "interactive", "subagent", "worktree", "ephemeral", "all"`)),
			mcp.WithNumber("min_messages", mcp.Description("Minimum substantive (non-noise) messages to include (default 6)")),
			mcp.WithNumber("limit", mcp.Description("Max sessions to return (default 30)")),
			mcp.WithString("project", mcp.Description("Filter by project name substring")),
			mcp.WithString("repo", mcp.Description("Filter by repo (org/name substring, e.g. \"marcelocantos/mnemo\")")),
			mcp.WithString("work_type", mcp.Description(`Filter by work type: "development", "feature", "bugfix", "refactor", "chore", "docs", "test", "ci", "release", "review", "branch-work"`)),
		),
		handleSessions(mem),
	)

	s.AddTool(
		mcp.NewTool("mnemo_read_session",
			mcp.WithDescription("Read messages from a specific session transcript. Returns messages ordered chronologically."),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID (the JSONL filename stem, or a prefix)")),
			mcp.WithString("role", mcp.Description(`Filter by role: "user" or "assistant". Omit for all roles.`)),
			mcp.WithNumber("offset", mcp.Description("Skip first N messages (default 0)")),
			mcp.WithNumber("limit", mcp.Description("Max messages to return (default 50)")),
		),
		handleReadSession(mem),
	)

	s.AddTool(
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
		handleQuery(mem),
	)

	s.AddTool(
		mcp.NewTool("mnemo_stats",
			mcp.WithDescription("Show transcript index statistics — sessions and messages broken down by session type, with noise vs substantive counts."),
		),
		handleStats(mem),
	)
}

func handleSearch(mem *store.Store) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query, _ := args["query"].(string)
		limit := 20
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		sessionType, _ := args["session_type"].(string)

		if query == "" {
			return mcp.NewToolResultError("query is required"), nil
		}

		results, err := mem.Search(query, limit, sessionType)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}

		if len(results) == 0 {
			return mcp.NewToolResultText("No results found."), nil
		}

		var b strings.Builder
		for _, r := range results {
			sid := r.SessionID
			if len(sid) > 8 {
				sid = sid[:8]
			}
			fmt.Fprintf(&b, "[%s] %s | %s | %s\n%s\n\n",
				r.Role, r.Project, sid, r.Timestamp, r.Text)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}

func handleSessions(mem *store.Store) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
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

		sessions, err := mem.ListSessions(sessionType, minMessages, limit, projectFilter, repoFilter, workTypeFilter)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list sessions failed: %v", err)), nil
		}

		if len(sessions) == 0 {
			return mcp.NewToolResultText("No sessions found."), nil
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
		return mcp.NewToolResultText(b.String()), nil
	}
}

func handleReadSession(mem *store.Store) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		sessionID, _ := args["session_id"].(string)
		if sessionID == "" {
			return mcp.NewToolResultError("session_id is required"), nil
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

		messages, err := mem.ReadSession(sessionID, role, offset, limit)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("read session failed: %v", err)), nil
		}

		if len(messages) == 0 {
			return mcp.NewToolResultText("No messages found for session " + sessionID), nil
		}

		var b strings.Builder
		for _, m := range messages {
			marker := ""
			if m.IsNoise {
				marker = " [noise]"
			}
			fmt.Fprintf(&b, "[%s]%s %s\n%s\n\n", m.Role, marker, m.Timestamp, m.Text)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}

func handleQuery(mem *store.Store) server.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query, _ := args["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("query is required"), nil
		}

		rows, err := mem.Query(query)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("query failed: %v", err)), nil
		}

		if len(rows) == 0 {
			return mcp.NewToolResultText("No rows returned."), nil
		}

		var b strings.Builder
		for _, row := range rows {
			for k, v := range row {
				fmt.Fprintf(&b, "%s: %v  ", k, v)
			}
			b.WriteByte('\n')
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}

func handleStats(mem *store.Store) server.ToolHandlerFunc {
	return func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		stats, err := mem.Stats()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("stats failed: %v", err)), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Total: %d sessions, %d messages\n\n", stats.TotalSessions, stats.TotalMessages)
		fmt.Fprintf(&b, "%-12s %8s %10s %12s %8s\n", "Type", "Sessions", "Total Msgs", "Substantive", "Noise")
		fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 55))
		for _, ts := range stats.ByType {
			fmt.Fprintf(&b, "%-12s %8d %10d %12d %8d\n",
				ts.SessionType, ts.Sessions, ts.TotalMsgs, ts.SubstantiveMsgs, ts.NoiseMsgs)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}
