// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package mcpbridge provides a reusable bimodal MCP server architecture:
// a persistent daemon that owns tool definitions and handling, connected
// to a thin stdio proxy over Unix domain socket RPC.
//
// The proxy is what an MCP client (e.g., Claude Code) launches. The daemon
// is what a service manager (e.g., brew services, systemd) runs. The proxy
// fetches tool definitions from the daemon at startup and forwards all
// tool calls, so it rarely needs updating.
//
// Usage:
//
//	// Daemon side
//	srv, _ := mcpbridge.NewServer(mcpbridge.DaemonConfig{
//	    SocketPath: socketPath,
//	    Tools:      myToolDefs,
//	    Handler:    myHandler,
//	})
//	srv.Serve()
//
//	// Proxy side (stdio)
//	mcpbridge.RunProxy(ctx, mcpbridge.ProxyConfig{
//	    SocketPath: socketPath,
//	    ServerName: "myserver",
//	    Version:    "1.0.0",
//	})
package mcpbridge

import (
	"encoding/json"
)

// ProtocolVersion is bumped only when the RPC wire format between the
// proxy and daemon changes (new/renamed methods, changed param types).
// It is independent of the MCP protocol and the application version.
const ProtocolVersion = 1

// ToolHandler dispatches MCP tool calls by name.
type ToolHandler interface {
	Call(name string, args map[string]any) (text string, isError bool, err error)
}

// MethodFunc handles a custom RPC method. It receives raw JSON params
// and returns a result to be JSON-marshaled.
type MethodFunc func(params json.RawMessage) (any, error)

// CallResult is the wire format for tool call results over RPC.
type CallResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// Handshake is the first message sent by the client after connecting.
type Handshake struct {
	ProtocolVersion int `json:"protocol_version"`
}

// Request is a JSON-RPC-like request sent over the UDS.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Response is a JSON-RPC-like response sent over the UDS.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// callToolParams is the wire format for CallTool requests.
type callToolParams struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}
