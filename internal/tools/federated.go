// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// FederatedToolNames is the curated set of read-only tools exposed on
// the mTLS federated endpoint (🎯T15.3). Write- or control-shaped
// tools (mnemo_ops op=restore, mnemo_config op=write, mnemo_vault
// op=sync, mnemo_note op=post, mnemo_thread op=new) are deliberately
// absent — federated peers are a different identity domain and must
// not influence local state.
//
// The set is closed by enumeration rather than computed by category,
// so adding a new tool is a deliberate decision: a new mnemo_X tool
// does NOT appear on the federated endpoint until it is added here.
var FederatedToolNames = map[string]struct{}{
	"mnemo_search":          {},
	"mnemo_sessions":        {},
	"mnemo_read_session":    {},
	"mnemo_query":           {},
	"mnemo_repos":           {},
	"mnemo_stats":           {},
	"mnemo_recent_activity": {},
	"mnemo_status":          {},
	"mnemo_usage":           {},
}

// RegisterFederatedTools attaches only the read-only subset of tools
// to the given MCP server. Used by the mTLS federated listener so
// peers cannot exercise any write or control surface.
func (h *Handler) RegisterFederatedTools(s *mcpserver.MCPServer) {
	for _, tool := range Definitions() {
		if _, ok := FederatedToolNames[tool.Name]; !ok {
			continue
		}
		name := tool.Name
		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			cc := CallContext{
				MCPSessionID: req.Header.Get(sessionIDHeader),
				Username:     UsernameFromContext(ctx),
			}
			text, isError, err := h.Call(ctx, cc, name, req.GetArguments())
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
