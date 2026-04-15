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
	"time"
)

// ProtocolVersion is bumped only when the RPC wire format between the
// proxy and daemon changes (new/renamed methods, changed param types).
// It is independent of the MCP protocol and the application version.
//
// Version 2: Handshake carries ProxyPID; handlers receive ConnContext.
const ProtocolVersion = 2

// ConnContext identifies the proxy connection that originated a
// request. It is minted by the daemon at connection accept and
// threaded into every handler invocation, so handlers can attribute
// work to a stable connection identity that survives /clear inside the
// same Claude Code process.
type ConnContext struct {
	// ID is a connection-scoped identifier minted at accept. Unique
	// for the life of the daemon; regenerated on reconnect.
	ID string
	// PID is the peer process's PID, recovered from the kernel via
	// LOCAL_PEERPID (Darwin) or SO_PEERCRED (Linux). Falls back to
	// the PID self-reported in the handshake if the sockopt fails.
	// Zero if neither path yielded a value.
	PID int
	// AcceptedAt is when the daemon accepted the connection.
	AcceptedAt time.Time
}

// ToolHandler dispatches MCP tool calls by name.
type ToolHandler interface {
	Call(cc ConnContext, name string, args map[string]any) (text string, isError bool, err error)
}

// MethodFunc handles a custom RPC method. It receives the connection
// context, raw JSON params, and returns a result to be JSON-marshaled.
type MethodFunc func(cc ConnContext, params json.RawMessage) (any, error)

// CallResult is the wire format for tool call results over RPC.
type CallResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// Handshake is the first message sent by the client after connecting.
type Handshake struct {
	ProtocolVersion int `json:"protocol_version"`
	// ProxyPID is the proxy process's PID. The daemon cross-checks
	// this against the kernel-reported peer PID and logs a mismatch
	// but does not reject the connection — peer creds are the
	// authoritative source. Omitted (zero) for pre-v2 clients.
	ProxyPID int `json:"proxy_pid,omitempty"`
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
