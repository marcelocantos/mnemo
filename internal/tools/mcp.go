// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// CallContext identifies the MCP session originating a tool call,
// plus the user identity that the call should index against.
// MCPSessionID was previously sourced from a UDS connection accept;
// under HTTP MCP it comes from the Mcp-Session-Id header. Username
// is extracted from the `?user=<name>` query parameter on the MCP
// endpoint URL, falling back to the daemon's own user when absent
// (except under a Windows Service, which has no sensible default).
type CallContext struct {
	// MCPSessionID is the value of the Mcp-Session-Id header — a
	// stable identifier for the duration of the MCP session
	// (spanning /clear boundaries inside the same Claude Code
	// process). Empty for pre-session calls or unit tests.
	MCPSessionID string

	// Username is the identity the tool call applies to. Empty is
	// allowed and means "the daemon's own user" — which the
	// resolver rejects on Windows-Service deployments.
	Username string
}

// sessionIDHeader is the header name defined by the MCP spec for
// streamable HTTP sessions.
const sessionIDHeader = "Mcp-Session-Id"

// ctxKey is a private type for context-value keys so we don't
// collide with any other package's ctx values.
type ctxKey string

const usernameCtxKey ctxKey = "mnemo.username"

// UsernameContextFunc is an mcp-go HTTPContextFunc that extracts the
// `?user=<name>` query parameter from the incoming HTTP request and
// stashes it on the ctx. Register it on the StreamableHTTPServer via
// server.WithHTTPContextFunc so tool handlers see the identity in
// the ctx they receive.
func UsernameContextFunc(ctx context.Context, r *http.Request) context.Context {
	if u := r.URL.Query().Get("user"); u != "" {
		return context.WithValue(ctx, usernameCtxKey, u)
	}
	return ctx
}

// UsernameFromContext returns the username previously set by
// UsernameContextFunc, or the empty string if none was set (i.e. the
// request did not carry ?user=... — the caller then applies its own
// default).
func UsernameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(usernameCtxKey).(string); ok {
		return v
	}
	return ""
}

// RegisterTools attaches every mnemo tool handler to the given MCP
// server. Tool arguments and results are translated between MCP's
// CallToolRequest/CallToolResult and the Handler.Call API. The MCP
// session ID is pulled from the Mcp-Session-Id request header and
// the username from the ctx value set by UsernameContextFunc; both
// are threaded through as CallContext. On the first tool call for a
// given (username, MCP session) pair, the session is recorded in
// that user's daemon_connections table so the compactor's watcher
// can find it.
func (h *Handler) RegisterTools(s *mcpserver.MCPServer) {
	for _, tool := range Definitions() {
		name := tool.Name
		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			cc := CallContext{
				MCPSessionID: req.Header.Get(sessionIDHeader),
				Username:     UsernameFromContext(ctx),
			}
			text, isError, err := h.Call(cc, name, req.GetArguments())
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s failed: %v", name, err)), nil
			}
			if isError {
				return mcp.NewToolResultError(text), nil
			}
			return mcp.NewToolResultText(text), nil
		})
	}
}
