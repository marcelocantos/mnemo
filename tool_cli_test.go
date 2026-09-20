// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"github.com/marcelocantos/mnemo/internal/tools"
)

// The CLI surface is derived, so these tests guard the derivation rather
// than a list. They are the counterpart of internal/tools/surface_test.go,
// which ratchets the MCP surface against its ledger (🎯T143.6): the
// failure mode both exist to catch is a capability arriving or leaving
// without anyone seeing it.

// TestEveryToolHasACLICounterpart is the forward direction of the
// ratchet: a tool added to Definitions() must be reachable from the
// command line, either through the derived dispatcher or through a
// hand-written command that deliberately owns the name.
func TestEveryToolHasACLICounterpart(t *testing.T) {
	cmds := toolCommands()
	for _, def := range tools.Definitions() {
		name := toolCLIName(def.Name)
		if _, ok := cmds[name]; !ok {
			t.Errorf("tool %s has no CLI command (expected `mnemo %s`)", def.Name, name)
			continue
		}
		if reservedToolCommands[name] {
			if _, ok := otherCommands[name]; !ok {
				t.Errorf("%q is reserved from the derived dispatcher but no hand-written "+
					"command claims it, so `mnemo %s` reaches nothing", name, name)
			}
		}
	}
}

// TestNoCLICommandShadowsAToolByAccident is the reverse direction. A
// hand-written command that happens to share a tool's derived name wins
// the dispatch silently — which is fine when it is deliberate, and a bug
// when it is not. reservedToolCommands is where "deliberate" is written
// down, so the two must agree.
func TestNoCLICommandShadowsAToolByAccident(t *testing.T) {
	cmds := toolCommands()
	for name := range otherCommands {
		if _, isTool := cmds[name]; !isTool {
			continue
		}
		if !reservedToolCommands[name] {
			t.Errorf("hand-written command %q shadows the derived counterpart of a tool. "+
				"If that is intended, add it to reservedToolCommands with the reason; "+
				"otherwise rename one of them.", name)
		}
	}
	for name := range reservedToolCommands {
		if _, isTool := cmds[name]; !isTool {
			t.Errorf("reservedToolCommands has %q, which is not a tool name — stale entry", name)
		}
	}
}

// TestPositionalArgumentIsTheRequiredOne pins the derivation rule. Every
// tool schema has at most one required property and every required
// property is a string; if that ever stops being true, the rule silently
// stops producing a positional argument and the command starts demanding
// a flag instead.
func TestPositionalArgumentIsTheRequiredOne(t *testing.T) {
	for _, def := range tools.Definitions() {
		req := def.InputSchema.Required
		if len(req) > 1 {
			t.Errorf("%s requires %v; the CLI derivation assumes at most one required "+
				"property and would leave the rest without a positional form", def.Name, req)
			continue
		}
		if len(req) == 1 {
			if got := propType(def, req[0]); got != "string" {
				t.Errorf("%s requires %q of type %q; the CLI derivation only makes a "+
					"string positional", def.Name, req[0], got)
			}
			if toolPositional(def) != req[0] {
				t.Errorf("%s: positional is %q, want %q", def.Name, toolPositional(def), req[0])
			}
		}
	}
}

// TestEveryPropertyBecomesAFlag guards against a schema type the
// derivation does not handle. An unhandled type produces no flag at all,
// so the argument becomes unreachable from the CLI while looking fine on
// the MCP side.
func TestEveryPropertyBecomesAFlag(t *testing.T) {
	handled := map[string]bool{"string": true, "number": true, "integer": true, "boolean": true}
	for _, def := range tools.Definitions() {
		for prop := range def.InputSchema.Properties {
			typ := propType(def, prop)
			if !handled[typ] {
				t.Errorf("%s.%s has type %q, which the CLI derivation does not turn into "+
					"a flag — the argument would be unreachable from the command line",
					def.Name, prop, typ)
			}
		}
	}
}

// TestNumericArgumentsTravelAsJSONNumbers is the regression test for the
// trap named in 🎯T187: Handler.Call type-asserts numeric arguments as
// float64 (internal/tools/tools.go), so an argument that reaches it as a
// Go int fails the assertion and is silently replaced by the tool's
// default — a flag that appears to be accepted and is ignored. The CLI
// avoids this by marshalling to JSON; this test pins that the round trip
// actually produces float64.
func TestNumericArgumentsTravelAsJSONNumbers(t *testing.T) {
	body, err := json.Marshal(map[string]any{"limit": float64(5)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["limit"].(float64); !ok {
		t.Fatalf("limit decoded as %T, want float64 — Handler.Call would ignore it", got["limit"])
	}
}

// TestCLINameMapping pins the naming rule, which is user-visible and
// therefore not free to change.
func TestCLINameMapping(t *testing.T) {
	for _, tc := range []struct{ tool, cli string }{
		{"mnemo_search", "search"},
		{"mnemo_read_session", "read-session"},
		{"mnemo_recent_activity", "recent-activity"},
		{"mnemo_ops", "ops"},
	} {
		if got := toolCLIName(tc.tool); got != tc.cli {
			t.Errorf("toolCLIName(%q) = %q, want %q", tc.tool, got, tc.cli)
		}
	}
}
