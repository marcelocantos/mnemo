// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
)

// FanoutToolNames lists the read-shaped MCP tools that participate in
// peer fan-out (🎯T15.5). When the daemon has at least one
// LinkedInstance configured AND a tool here is invoked, the response
// is wrapped in a FanoutEnvelope that includes per-peer attributed
// results and a warnings list for peers that failed with a typed
// transport error.
//
// Tools NOT in this set bypass federation entirely — they continue to
// return their original local-only response shape regardless of
// whether peers are configured. This includes write- and
// control-shaped tools (covered by exclusion from
// tools.FederatedToolNames at the server side) and read-shaped tools
// whose result formats don't lend themselves to bucketed merging
// (mnemo_stats, mnemo_status, mnemo_chain, mnemo_query — bespoke
// shapes that callers expect verbatim).
var FanoutToolNames = map[string]struct{}{
	"mnemo_search":            {},
	"mnemo_sessions":          {},
	"mnemo_recent_activity":   {},
	"mnemo_decisions":         {},
	"mnemo_commits":           {},
	"mnemo_prs":               {},
	"mnemo_memories":          {},
	"mnemo_who_ran":           {},
	"mnemo_audit":             {},
	"mnemo_targets":           {},
	"mnemo_plans":             {},
	"mnemo_skills":            {},
	"mnemo_configs":           {},
	"mnemo_ci":                {},
	"mnemo_images":            {},
	"mnemo_discover_patterns": {},
}

// PeerResult holds the raw text response from one peer's instance of
// a tool. Local is intentionally a separate parameter to the merge
// step — the caller invokes the local handler directly (typically in
// parallel with the peer fan-out).
type PeerResult struct {
	// Instance is the peer's configured Name (from LinkedInstance.Name).
	Instance string `json:"instance"`

	// Result is the peer's MCP TextContent payload, parsed as JSON if
	// possible. Non-JSON payloads land as a JSON string.
	Result json.RawMessage `json:"result"`
}

// PeerWarning captures a peer that failed with one of the typed
// transport errors. The error_kind names the sentinel (timeout,
// connection_refused, etc.) so callers can categorise without
// substring-matching the message.
type PeerWarning struct {
	Instance  string `json:"instance"`
	ErrorKind string `json:"error_kind"`
	Message   string `json:"message"`
}

// FanoutEnvelope is the MCP TextContent payload emitted by a
// federated tool when peers are configured. The shape is additive
// over the original local response: callers that don't understand
// federation see "local" as the same JSON they used to get directly.
type FanoutEnvelope struct {
	// Local is the host responder's own result, parsed as JSON when
	// possible (a plain text result lands as a JSON string).
	Local json.RawMessage `json:"local"`

	// Peers is the list of peer results, sorted by instance name for
	// deterministic ordering across calls.
	Peers []PeerResult `json:"peers,omitempty"`

	// Warnings names every peer that failed in a way the caller
	// should be told about (typed transport errors).
	Warnings []PeerWarning `json:"warnings,omitempty"`
}

// Fanout calls every configured peer's instance of toolName in
// parallel with the supplied args. Per-peer timeouts already bound
// each call (DefaultPeerTimeout from CallTool); Fanout itself does
// not impose an additional deadline. Failures are categorised into
// warnings rather than returned — callers can always treat Fanout as
// best-effort.
//
// The returned slices are sorted by instance name for deterministic
// output across runs (matters for snapshot tests and human reading).
func (c *Client) Fanout(
	ctx context.Context,
	toolName string,
	args map[string]any,
) ([]PeerResult, []PeerWarning) {
	if len(c.peers) == 0 {
		return nil, nil
	}

	type out struct {
		name   string
		result *mcp.CallToolResult
		err    error
	}
	ch := make(chan out, len(c.peers))
	var wg sync.WaitGroup
	for name := range c.peers {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			res, err := c.CallTool(ctx, name, toolName, args)
			ch <- out{name: name, result: res, err: err}
		}(name)
	}
	wg.Wait()
	close(ch)

	var (
		results  []PeerResult
		warnings []PeerWarning
	)
	for o := range ch {
		if o.err != nil {
			warnings = append(warnings, PeerWarning{
				Instance:  o.name,
				ErrorKind: classifyErrorKind(o.err),
				Message:   o.err.Error(),
			})
			continue
		}
		results = append(results, PeerResult{
			Instance: o.name,
			Result:   peerResultPayload(o.result),
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Instance < results[j].Instance })
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Instance < warnings[j].Instance })
	return results, warnings
}

// MergePeerResults wraps a local result text + per-peer results into
// a FanoutEnvelope and returns its JSON-encoded form, ready to be
// emitted as MCP TextContent. local should be the raw text the local
// tool would have produced without federation.
//
// If peers and warnings are both empty (which only happens when no
// peers are configured), MergePeerResults returns the original local
// text unchanged — preserving the schema-stable property documented
// on FanoutEnvelope.
func MergePeerResults(local string, peers []PeerResult, warnings []PeerWarning) (string, error) {
	if len(peers) == 0 && len(warnings) == 0 {
		return local, nil
	}
	env := FanoutEnvelope{
		Local:    asJSONOrString(local),
		Peers:    peers,
		Warnings: warnings,
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// peerResultPayload pulls the first TextContent out of a CallToolResult,
// parses it as JSON if possible, and returns it as RawMessage. Multi-
// content responses are concatenated into a single string; non-JSON
// text is wrapped as a JSON string.
func peerResultPayload(res *mcp.CallToolResult) json.RawMessage {
	if res == nil {
		return json.RawMessage("null")
	}
	var text string
	for _, c := range res.Content {
		if t, ok := c.(mcp.TextContent); ok {
			text += t.Text
		}
	}
	return asJSONOrString(text)
}

// asJSONOrString returns s as a JSON RawMessage. If s is itself valid
// JSON, it is returned verbatim; otherwise it is wrapped as a JSON
// string literal.
func asJSONOrString(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	out, _ := json.Marshal(s)
	return out
}

// classifyErrorKind maps a CallTool error onto a stable string token
// for the PeerWarning.ErrorKind field. Order matters: more specific
// sentinels are checked before more general ones.
func classifyErrorKind(err error) string {
	switch {
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, ErrConnectionRefused):
		return "connection_refused"
	case errors.Is(err, ErrTLSHandshake):
		return "tls_handshake"
	case errors.Is(err, ErrServerError):
		return "server_error"
	case errors.Is(err, ErrMalformedResponse):
		return "malformed_response"
	case errors.Is(err, ErrUnknownInstance):
		return "unknown_instance"
	case errors.Is(err, ErrConnectFailed):
		return "connect_failed"
	}
	return "unknown"
}
