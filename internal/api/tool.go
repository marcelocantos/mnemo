// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ToolCaller is the transport-neutral tool entry point, narrowed to what
// a transport actually needs. It matches tools.Handler.Call with the
// CallContext flattened to the one field an HTTP caller can supply.
//
// It is a function field rather than an interface over internal/tools so
// the api package keeps no dependency on the tool surface, and so a test
// can drive the endpoint without standing up a store.
type ToolCaller func(ctx context.Context, user, name string, args map[string]any) (text string, isError bool, err error)

// SetToolCaller wires the tool bridge. Call once during startup wiring,
// before serving requests. Leaving it unset disables the endpoint.
func (h *Handler) SetToolCaller(c ToolCaller) { h.tools = c }

// toolCallResult is the bridge's response envelope.
//
// `ok` reports the tool's own verdict, not the transport's. A tool that
// answers "no results" and a tool that rejects its arguments both arrive
// as HTTP 200 — the difference is this flag, which is what lets the CLI
// set an exit status without parsing prose. Transport failures (no such
// tool, malformed body) are HTTP errors and carry no envelope.
type toolCallResult struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

// toolCall serves POST /api/tool/<name> (🎯T187).
//
// This is what the CLI counterparts call. It is deliberately NOT an MCP
// transport: no JSON-RPC envelope, no initialize handshake, no session.
// 🎯T160 settled that mnemo speaks HTTP and nothing else, and that a
// second modality is not worth its ambiguity. What this shares with the
// /mcp mount is the layer beneath both — tools.Handler.Call — which is
// the point: a CLI counterpart cannot drift from the tool an agent
// calls, because there is only one implementation.
//
// The request body is the tool's arguments as a JSON object, so numbers
// arrive as float64. That matters: Call type-asserts numeric arguments
// as float64, and a Go int reaching it would fail the assertion and be
// silently replaced by the tool's default.
func (h *Handler) toolCall(w http.ResponseWriter, r *http.Request) {
	if h.tools == nil {
		http.Error(w, "tool bridge not wired", http.StatusNotImplemented)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/tool/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "usage: POST /api/tool/<tool name>", http.StatusBadRequest)
		return
	}

	args := map[string]any{}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// An absent body means "no arguments" — every tool but the four
	// op-dispatched ones has an all-optional schema, so `mnemo stats`
	// sends nothing at all.
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &args); err != nil {
			http.Error(w, "arguments must be a JSON object: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	text, isErr, err := h.tools(r.Context(), r.URL.Query().Get("user"), name, args)
	if err != nil {
		// Call returns a non-nil error only for an unknown tool; every
		// in-tool failure comes back as isError with text.
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, toolCallResult{OK: !isErr, Text: text})
}
