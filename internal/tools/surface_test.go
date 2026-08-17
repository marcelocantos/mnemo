// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"sort"
	"strings"
	"testing"
)

// registeredToolNames returns what Tools() actually registers.
func registeredToolNames(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, tool := range Definitions() {
		out[tool.Name] = true
	}
	return out
}

// TestToolSurfaceRatchet is the 🎯T143.6 ratchet: the registered
// surface must match the committed ledger in surface.go exactly.
//
// A ratchet, not a threshold. A threshold ("fail above 45") gets raised
// the first time it is inconvenient, by the person it is inconvenient
// for, in the very commit that makes it inconvenient. Requiring the
// ledger to be edited in the same diff as the tool makes the growth the
// subject of the review rather than a side effect of it.
//
// Both directions are checked. An unexpected ADDITION is the surface
// regrowing, which is what the audit found after four months of nobody
// looking. An unexpected REMOVAL means a capability left without anyone
// recording that it had.
func TestToolSurfaceRatchet(t *testing.T) {
	registered := registeredToolNames(t)

	var undeclared, missing []string
	for name := range registered {
		if _, ok := toolConsumers[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	for name := range toolConsumers {
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(missing)

	if len(undeclared) > 0 {
		t.Errorf("tools registered but absent from the surface ledger: %s\n"+
			"Adding a tool is a deliberate act: add it to toolConsumers in "+
			"surface.go, naming who actually calls it. If nothing does, that "+
			"is the finding — do not add the entry to silence this.",
			strings.Join(undeclared, ", "))
	}
	if len(missing) > 0 {
		t.Errorf("ledger names tools that are not registered: %s\n"+
			"A capability was removed without the ledger being updated in the "+
			"same change.", strings.Join(missing, ", "))
	}
}

// TestToolSurfaceSize pins the headline number 🎯T143 was filed
// against, so a slow drift back toward 70 has to be argued for rather
// than accumulated.
func TestToolSurfaceSize(t *testing.T) {
	// Baseline set by 🎯T143 on 2026-08-07, down from 70 pre-audit.
	const baseline = 18
	if got := len(registeredToolNames(t)); got != baseline {
		t.Errorf("registered tool count is %d, ledger baseline is %d.\n"+
			"If this change is intended, move the baseline in the same commit "+
			"and say why in the message. The pre-🎯T143 surface was 70 tools, "+
			"30 of which no agent had ever called.", got, baseline)
	}
}

// TestConsolidationPreservesCapabilities is the parent target's
// capability-preservation oracle. Consolidation moves affordances
// behind an op; it must not delete them.
//
// The op lists below are the operations that existed as their own
// tools before 🎯T143.3/.4/.5. Each must still resolve.
func TestConsolidationPreservesCapabilities(t *testing.T) {
	cases := []struct {
		table opTable
		// wasTool maps each op to the tool it replaced, so a failure
		// names the capability that went missing rather than an op.
		wasTool map[string]string
	}{
		{vaultOps, map[string]string{
			"status":         "mnemo_vault_status",
			"sync":           "mnemo_vault_sync",
			"gc":             "mnemo_vault_gc",
			"migration_doc":  "mnemo_vault_migration_doc",
			"bridge_list":    "mnemo_vault_bridge_list",
			"recluster":      "mnemo_vault_recluster",
			"themes_inspect": "mnemo_vault_themes_inspect",
			"themes_pin":     "mnemo_vault_themes_pin",
			"themes_split":   "mnemo_vault_themes_split",
			"themes_merge":   "mnemo_vault_themes_merge",
		}},
		{threadOps, map[string]string{
			"list":    "mnemo_thread_list",
			"show":    "mnemo_thread_show",
			"new":     "mnemo_thread_new",
			"archive": "mnemo_thread_archive",
			"go":      "mnemo_thread_go",
		}},
		{noteOps, map[string]string{
			"post": "mnemo_note_post",
			"recv": "mnemo_note_recv",
			"list": "mnemo_note_list",
		}},
		{opsOps, map[string]string{
			"doctor":        "mnemo_doctor",
			"compactor":     "mnemo_compactor_status",
			"divergence":    "mnemo_divergence",
			"backup_status": "mnemo_backup_status",
			"backup_now":    "mnemo_backup_now",
			"restore":       "mnemo_restore",
			// 🎯T140: deliberate new MCP home after tools were removed
			// (not a silent re-add of mnemo_budget / mnemo_agent_trees names).
			"budget":      "mnemo_budget",
			"agent_trees": "mnemo_agent_trees",
		}},
	}

	for _, tc := range cases {
		have := map[string]bool{}
		for _, o := range tc.table.ops {
			have[o.name] = true
		}
		for op, wasTool := range tc.wasTool {
			if !have[op] {
				t.Errorf("%s lost the capability that used to be %s (op=%s)",
					tc.table.tool, wasTool, op)
				continue
			}
			// It must not merely be listed — it must dispatch.
			if _, err := tc.table.resolve(map[string]any{"op": op}); err != nil {
				t.Errorf("%s op=%s (was %s) does not resolve: %v",
					tc.table.tool, op, wasTool, err)
			}
		}
		// And nothing extra crept in unannounced.
		for op := range have {
			if _, ok := tc.wasTool[op]; !ok {
				t.Errorf("%s declares op=%s which replaced no prior tool; "+
					"add it to the inventory if it is a deliberate new capability",
					tc.table.tool, op)
			}
		}
	}
}

// TestRemovedToolsStayRemoved guards 🎯T143.1 against a reintroduction
// by copy-paste. These twelve had no consumer; if one is genuinely
// wanted again it needs a ledger entry and a reason, not a silent
// return.
func TestRemovedToolsStayRemoved(t *testing.T) {
	registered := registeredToolNames(t)
	for _, name := range []string{
		"mnemo_plans", "mnemo_ci", "mnemo_define", "mnemo_evaluate", "mnemo_self",
		"mnemo_list_templates", "mnemo_images", "mnemo_get_memory",
		"mnemo_tool_result", "mnemo_source_drift",
		"mnemo_todos", "mnemo_todo_add", "mnemo_todo_set",
		// Folded into consolidated entry points.
		"mnemo_vault_status", "mnemo_vault_sync", "mnemo_vault_gc",
		"mnemo_docs", "mnemo_configs", "mnemo_skills", "mnemo_audit", "mnemo_targets",
		"mnemo_memories", "mnemo_commits", "mnemo_prs", "mnemo_who_ran", "mnemo_synthesis",
		"mnemo_chain", "mnemo_budget", "mnemo_agent_trees", "mnemo_session_go",
		"mnemo_whatsup", "mnemo_permissions", "mnemo_discover_patterns",
		"mnemo_segments", "mnemo_decisions",
		"mnemo_thread_list", "mnemo_thread_go",
		"mnemo_note_post", "mnemo_note_recv", "mnemo_note_list",
		"mnemo_doctor", "mnemo_compactor_status", "mnemo_divergence",
		"mnemo_backup_now", "mnemo_backup_status", "mnemo_restore",
	} {
		if registered[name] {
			t.Errorf("%s is registered again; 🎯T143 removed or folded it", name)
		}
	}
}
