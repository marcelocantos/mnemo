// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package teamserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/store"
)

// The team-scope MCP surface (🎯T36.3).
//
// These tools are NOT the local mnemo tools pointed at other tables.
// They are a separate, smaller set, named differently, registered on a
// different server, over a different port. Three properties follow from
// that, and all three were requirements:
//
//  1. Author is a first-class filter and appears on every result. "What
//     did alice work on last week?" is a core team query, and an
//     unattributed hit in a multi-author index is not interpretable.
//  2. Nothing here can write, except the retraction a contributor
//     applies to their own content. There is no restore, no config, no
//     vault sync — a peer is a different identity domain.
//  3. Adding a local tool does not silently add a team tool. The set is
//     closed by enumeration in teamToolConsumers, and a ratchet fails on
//     drift, the same discipline internal/tools/surface.go applies to
//     the local surface and for the same reason: a surface nobody
//     reviews grows.
//
// Tool names carry the mnemo_team_ prefix rather than reusing mnemo_*.
// The design left this open, preferring server-name disambiguation; the
// prefix is chosen instead because an agent with both servers
// registered sees a flat tool list, and `mnemo_search` appearing twice
// with different meanings is a trap the agent cannot see. The prefix
// costs a few tokens and removes the ambiguity entirely.

// callerCtxKey carries the authenticated contributor into tool handlers.
type callerCtxKey struct{}

// callerContextFunc stashes the authenticated caller on the request
// context so a tool handler knows whose identity it is acting under —
// needed by retract, which is author-scoped.
func (s *Server) callerContextFunc(ctx context.Context, r *http.Request) context.Context {
	c, err := s.authenticate(r)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, callerCtxKey{}, c)
}

func callerFrom(ctx context.Context) (caller, bool) {
	c, ok := ctx.Value(callerCtxKey{}).(caller)
	return c, ok
}

// teamToolConsumers is the team surface's ledger. Same contract as
// internal/tools/surface.go: a tool added without a line here fails the
// ratchet, so the surface cannot grow without a reviewer seeing it.
var teamToolConsumers = map[string]string{
	"mnemo_team_search":          "agent — the reason team-mnemo exists: search the team's collective sessions",
	"mnemo_team_sessions":        "agent — list pushed sessions by author, repo, recency",
	"mnemo_team_read_session":    "agent — read one pushed session after search surfaced it",
	"mnemo_team_recent_activity": "agent — the newcomer's first question: who is working on what",
	"mnemo_team_authors":         "agent — who has pushed to this index at all",
	"mnemo_team_stats":           "user — index size and span, for the operator",
	"mnemo_team_retract":         "user — a contributor withdrawing their own pushed session",
}

// teamToolDefinitions returns the team-scope tool set.
func teamToolDefinitions() []mcp.Tool {
	return []mcp.Tool{
		mcp.NewTool("mnemo_team_search",
			mcp.WithDescription(`Full-text search across the TEAM index — sessions pushed by every contributor, not just yours.

Use this to pick up an ongoing program of work without archaeology: "what is the state of the authentication refactor?" answered from the sessions that did it, rather than from commit messages and Slack.

Every hit carries the author who pushed it. Content is redacted by each contributor before upload, so a hit may show `+"`<REDACTED>`"+` where a secret was — the session's redaction tally (see mnemo_team_sessions) says what was removed.`),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query. Plain words use OR; AND/NOT/NEAR/quotes for precision.")),
			mcp.WithString("author", mcp.Description(`Filter to one contributor, as they appear in mnemo_team_authors. Omit for the whole team.`)),
			mcp.WithString("repo", mcp.Description("Filter by repo, matched as a substring.")),
			mcp.WithNumber("days", mcp.Description("Recency window in days. Omit for all time.")),
			mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
		),
		mcp.NewTool("mnemo_team_sessions",
			mcp.WithDescription("List sessions in the team index, most recently active first. Each row carries its author, repo, work type, entry count, and the contributor's redaction tally — what their privacy filter removed before upload."),
			mcp.WithString("author", mcp.Description("Filter to one contributor.")),
			mcp.WithString("repo", mcp.Description("Filter by repo, matched as a substring.")),
			mcp.WithNumber("days", mcp.Description("Recency window in days. Omit for all time.")),
			mcp.WithNumber("limit", mcp.Description("Max sessions (default 20)")),
		),
		mcp.NewTool("mnemo_team_read_session",
			mcp.WithDescription("Read a pushed session's messages in transcript order. Thinking blocks and images are never present — contributors do not push them."),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session id, as returned by mnemo_team_search or mnemo_team_sessions.")),
			mcp.WithNumber("offset", mcp.Description("Entry offset to start from (default 0)")),
			mcp.WithNumber("limit", mcp.Description("Max entries (default 50)")),
		),
		mcp.NewTool("mnemo_team_recent_activity",
			mcp.WithDescription("Who is working on what, grouped by repo and author. The first call to make when joining a program of work or returning to one after time away."),
			mcp.WithNumber("days", mcp.Description("Recency window in days (default 30)")),
			mcp.WithString("repo", mcp.Description("Filter by repo, matched as a substring.")),
			mcp.WithString("author", mcp.Description("Filter to one contributor.")),
		),
		mcp.NewTool("mnemo_team_authors",
			mcp.WithDescription("List contributors present in the team index, with session counts and last activity. Use it to learn the names the author filter accepts."),
		),
		mcp.NewTool("mnemo_team_stats",
			mcp.WithDescription("Team index statistics: sessions, entries, contributors, repos, and the time span covered."),
		),
		mcp.NewTool("mnemo_team_retract",
			mcp.WithDescription(`Withdraw a session you pushed from the team index, removing its entries and making it unsearchable.

Author-scoped: you can retract what you published, not what a colleague published. An operator listed as an admin on the team server may retract on a contributor's behalf.`),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session id to withdraw.")),
		),
	}
}

func (s *Server) registerTeamTools(srv *mcpserver.MCPServer) {
	for _, tool := range teamToolDefinitions() {
		name := tool.Name
		srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text, err := s.callTeamTool(ctx, name, req.GetArguments())
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s failed: %v", name, err)), nil
			}
			return mcp.NewToolResultText(text), nil
		})
	}
}

func (s *Server) callTeamTool(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "mnemo_team_search":
		hits, err := s.index.SearchTeam(store.TeamSearchParams{
			Query:  argString(args, "query"),
			Author: argString(args, "author"),
			Repo:   argString(args, "repo"),
			Days:   argInt(args, "days", 0),
			Limit:  argInt(args, "limit", 20),
		})
		if err != nil {
			return "", err
		}
		return renderJSON(map[string]any{"hits": hits, "count": len(hits)})

	case "mnemo_team_sessions":
		sessions, err := s.index.ListTeamSessions(store.TeamSessionsParams{
			Author: argString(args, "author"),
			Repo:   argString(args, "repo"),
			Days:   argInt(args, "days", 0),
			Limit:  argInt(args, "limit", 20),
		})
		if err != nil {
			return "", err
		}
		return renderJSON(map[string]any{"sessions": sessions, "count": len(sessions)})

	case "mnemo_team_read_session":
		id := argString(args, "session_id")
		if id == "" {
			return "", fmt.Errorf("session_id is required")
		}
		entries, err := s.index.ReadTeamSession(id, argInt(args, "offset", 0), argInt(args, "limit", 50))
		if err != nil {
			return "", err
		}
		return renderJSON(map[string]any{"session_id": id, "entries": entries, "count": len(entries)})

	case "mnemo_team_recent_activity":
		acts, err := s.index.TeamRecentActivity(
			argInt(args, "days", 30), argString(args, "repo"), argString(args, "author"))
		if err != nil {
			return "", err
		}
		return renderJSON(map[string]any{"activity": acts, "count": len(acts)})

	case "mnemo_team_authors":
		authors, err := s.index.TeamAuthors()
		if err != nil {
			return "", err
		}
		return renderJSON(map[string]any{"authors": authors, "count": len(authors)})

	case "mnemo_team_stats":
		stats, err := s.index.TeamStats()
		if err != nil {
			return "", err
		}
		return renderJSON(stats)

	case "mnemo_team_retract":
		id := argString(args, "session_id")
		if id == "" {
			return "", fmt.Errorf("session_id is required")
		}
		c, ok := callerFrom(ctx)
		if !ok {
			// Belt and braces: the HTTP layer already refused
			// unauthenticated callers. Reaching here means the context
			// plumbing broke, and a write must not proceed on an
			// identity we cannot name.
			return "", fmt.Errorf("caller identity unavailable; refusing to retract")
		}
		n, err := s.index.RetractTeamSession(id, c.Author, s.admins[c.PeerName])
		if err != nil {
			return "", err
		}
		return renderJSON(map[string]any{
			"session_id":      id,
			"entries_removed": n,
			"retracted_by":    c.Author,
			"as_admin":        s.admins[c.PeerName],
		})

	default:
		valid := make([]string, 0, len(teamToolConsumers))
		for n := range teamToolConsumers {
			valid = append(valid, n)
		}
		return "", fmt.Errorf("unknown team tool %q; valid tools: %s", name, strings.Join(valid, ", "))
	}
}

func renderJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return int(n)
		}
	}
	return def
}
