// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// ToolProxy forwards ListTools and CallTool over an RPC client.
type ToolProxy struct {
	client *Client
}

// NewToolProxy creates a proxy that forwards tool operations over RPC.
func NewToolProxy(c *Client) *ToolProxy {
	return &ToolProxy{client: c}
}

// ListTools fetches MCP tool definitions from the daemon.
func (p *ToolProxy) ListTools() ([]mcp.Tool, error) {
	raw, err := p.client.Call("ListTools", nil)
	if err != nil {
		return nil, err
	}
	var defs []mcp.Tool
	return defs, json.Unmarshal(raw, &defs)
}

// CallTool executes a tool on the daemon and returns the result.
func (p *ToolProxy) CallTool(name string, args map[string]any) (CallResult, error) {
	raw, err := p.client.Call("CallTool", callToolParams{Name: name, Args: args})
	if err != nil {
		return CallResult{}, err
	}
	var result CallResult
	return result, json.Unmarshal(raw, &result)
}

// ProxyConfig configures the stdio MCP proxy.
type ProxyConfig struct {
	SocketPath string    // Required. Unix domain socket path to daemon.
	ServerName string    // MCP server name (e.g., "mnemo").
	Version    string    // MCP server version.
	Stdin      io.Reader // Optional. Defaults to os.Stdin.
	Stdout     io.Writer // Optional. Defaults to os.Stdout.
}

// RunProxy runs the stdio MCP bridge: connects to the daemon, fetches
// tool definitions, and serves MCP over stdin/stdout. Blocks until
// stdin closes or an error occurs.
func RunProxy(ctx context.Context, cfg ProxyConfig) error {
	client, err := Dial(cfg.SocketPath)
	if err != nil {
		return err
	}
	defer client.Close()

	proxy := NewToolProxy(client)

	toolDefs, err := proxy.ListTools()
	if err != nil {
		return fmt.Errorf("fetch tools: %w", err)
	}

	s := mcpserver.NewMCPServer(
		cfg.ServerName,
		cfg.Version,
		mcpserver.WithToolCapabilities(true),
	)

	for _, tool := range toolDefs {
		name := tool.Name
		s.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := proxy.CallTool(name, req.GetArguments())
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s failed: %v", name, err)), nil
			}
			if result.IsError {
				return mcp.NewToolResultError(result.Text), nil
			}
			return mcp.NewToolResultText(result.Text), nil
		})
	}

	stdin := cfg.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	stdio := mcpserver.NewStdioServer(s)
	return stdio.Listen(ctx, stdin, stdout)
}
