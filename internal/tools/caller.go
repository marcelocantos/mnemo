// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// sessionIDShape accepts the ids mnemo stores: Claude Code and Cursor
// UUIDs, Codex thread ids, and the same alphabet with a short minimum
// so a progress token or a single word cannot exclude a session by
// accident. Mcp-Session-Id is deliberately not read; it is the MCP
// transport session, which outlives /clear and changes on reconnect.
var sessionIDShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

// sessionMetaKeys are params._meta keys that carry the transcript
// session, most specific first. Claude Code does not send one today
// (it sends claudecode/toolUseId and progressToken). The keys are
// accepted so a client or a future build that does send one is picked
// up without a daemon change.
var sessionMetaKeys = []string{
	"claudecode/sessionId",
	"claudecode/session_id",
	"claude-code/sessionId",
	"claude-code/session-id",
	"claude/sessionId",
	"cursor/sessionId",
	"cursor/conversationId",
	"codex/sessionId",
	"codex/threadId",
	"grok/sessionId",
	"sessionId",
	"session_id",
}

// sessionHeaders are the same fact as an HTTP header, for a SessionStart
// hook or a proxy that can inject it when the client cannot.
var sessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"X-Claude-Session-Id",
	"Claude-Code-Session-Id",
	"X-Cursor-Session-Id",
	"X-Cursor-Conversation-Id",
}

// toolUseMetaKeys name the tool call that is in flight. The indexed
// messages.tool_use_id row for that id is the caller's session, when
// ingest has already written it. A miss reads the newest session
// JSONLs under the configured Claude roots: Claude Code appends the
// tool_use line before the MCP call. A miss there does not exclude.
var toolUseMetaKeys = []string{
	"claudecode/toolUseId",
	"claude-code/toolUseId",
	"cursor/toolCallId",
	"toolUseId",
}

// requestMeta copies params._meta from both the CallToolParams envelope
// and the embedded Request. The outer params win. progressToken is
// dropped by mcp-go before AdditionalFields is filled.
func requestMeta(req mcp.CallToolRequest) map[string]any {
	out := map[string]any{}
	copyMeta(out, req.Request.Params.Meta)
	copyMeta(out, req.Params.Meta)
	return out
}

func copyMeta(dst map[string]any, m *mcp.Meta) {
	if m == nil {
		return
	}
	for k, v := range m.AdditionalFields {
		dst[k] = v
	}
}

// IdentifyCaller returns the transcript session id to exclude, and a
// short label of which source produced it. An empty id means the caller
// could not be identified and search must not exclude anything.
func IdentifyCaller(meta map[string]any, header http.Header, lookup func(toolUseID string) (string, error)) (string, string) {
	if id, src := sessionFromMeta(meta); id != "" {
		return id, src
	}
	if header != nil {
		for _, k := range sessionHeaders {
			if id := validSessionID(header.Get(k)); id != "" {
				return id, "header"
			}
		}
		if id := sessionFromBaggage(header.Get("Baggage")); id != "" {
			return id, "baggage"
		}
	}
	if lookup != nil {
		if id := validSessionID(toolUseSession(meta, lookup)); id != "" {
			return id, "tool_use"
		}
	}
	return "", ""
}

func sessionFromMeta(meta map[string]any) (string, string) {
	if id := sessionFromMap(meta); id != "" {
		return id, "meta"
	}
	// One level of nesting: some bridges wrap client metadata.
	for _, v := range meta {
		child, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if id := sessionFromMap(child); id != "" {
			return id, "meta"
		}
	}
	return "", ""
}

func sessionFromMap(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	for _, k := range sessionMetaKeys {
		if id := validSessionID(asString(meta[k])); id != "" {
			return id
		}
	}
	return ""
}

func sessionFromBaggage(v string) string {
	// W3C baggage: key=value pairs separated by commas.
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		switch key {
		case "session_id", "session.id", "claude.session_id":
			if id := validSessionID(strings.TrimSpace(val)); id != "" {
				return id
			}
		}
	}
	return ""
}

func toolUseSession(meta map[string]any, lookup func(string) (string, error)) string {
	var id string
	for _, k := range toolUseMetaKeys {
		if s := asString(meta[k]); s != "" {
			id = s
			break
		}
	}
	if id == "" {
		return ""
	}
	sid, err := lookup(id)
	if err != nil {
		return ""
	}
	return sid
}

func validSessionID(s string) string {
	s = strings.TrimSpace(s)
	if sessionIDShape.MatchString(s) {
		return s
	}
	return ""
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func includeCallerArg(args map[string]any) bool {
	switch v := args["include_caller"].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes"
	case float64:
		return v != 0
	default:
		return false
	}
}

func callerResultNote(session string, include bool, how string) string {
	switch {
	case include && session != "":
		return "Calling session included (include_caller)."
	case include:
		return "include_caller is set; the calling session was not identified."
	case session != "":
		return "Calling session excluded (" + how + "). Pass include_caller=true to include it."
	default:
		return "Calling session was not identified, so it was not excluded."
	}
}

func (h *callHandler) identifyCaller() (string, string) {
	lookup := func(toolUseID string) (string, error) {
		s, ok := h.mem.(interface {
			SessionForToolUse(string) (string, error)
		})
		if !ok {
			return "", nil
		}
		return s.SessionForToolUse(toolUseID)
	}
	return IdentifyCaller(h.cc.Meta, h.cc.Header, lookup)
}
