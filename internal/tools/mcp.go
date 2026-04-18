// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// CallContext identifies the MCP session originating a tool call. It
// was previously sourced from a UDS connection accept; under HTTP MCP,
// it comes from the Mcp-Session-Id header.
type CallContext struct {
	// MCPSessionID is the value of the Mcp-Session-Id header — a
	// stable identifier for the duration of the MCP session
	// (spanning /clear boundaries inside the same Claude Code
	// process). Empty for pre-session calls or unit tests.
	MCPSessionID string
}

// sessionIDHeader is the header name defined by the MCP spec for
// streamable HTTP sessions.
const sessionIDHeader = "Mcp-Session-Id"

// RegisterTools attaches every mnemo tool handler to the given MCP
// server. Tool arguments and results are translated between MCP's
// CallToolRequest/CallToolResult and the Handler.Call API. The MCP
// session ID is extracted from the Mcp-Session-Id request header (if
// present) and threaded through as CallContext.
//
// On the first tool call for a given MCP session ID, the session is
// recorded in the store's daemon_connections table so the compactor's
// watcher can find it. Subsequent calls are free.
func (h *Handler) RegisterTools(s *mcpserver.MCPServer) {
	var seen sync.Map
	for _, tool := range Definitions() {
		name := tool.Name
		s.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionID := req.Header.Get(sessionIDHeader)
			if sessionID != "" {
				if _, loaded := seen.LoadOrStore(sessionID, struct{}{}); !loaded {
					h.mem.RecordConnectionOpen(sessionID, 0, time.Now())
				}
			}
			cc := CallContext{MCPSessionID: sessionID}
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
