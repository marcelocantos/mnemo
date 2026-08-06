// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"github.com/mark3labs/mcp-go/mcp"
)

// vaultOps is the dispatch table for the consolidated mnemo_vault tool
// (🎯T143.3), replacing the ten mnemo_vault_* tools.
//
// Every one of those ten had zero agent calls in the 2026-08-07 audit,
// which is what makes this the safest consolidation on the board: the
// discoverability being traded away was not being used. Seven of them
// appeared within one month as 🎯T64's sub-targets landed, each adding
// its own tool because that was the natural unit of work — the
// accretion this table exists to stop, so 🎯T64.9 and 🎯T64.12 land as
// ops rather than as two more tools.
var vaultOps = opTable{
	tool: "mnemo_vault",
	ops: []opSpec{
		{
			name: "status",
			desc: "Report vault configuration: enabled state, root path, indexing scope, .mnemoignore state, layout, note counts, detected PKM profile and configured bridges",
		},
		{
			name: "sync",
			desc: "Write/update notes for every session, decision, memory, plan, target, CI run and PR, then re-ingest so human edits are searchable",
		},
		{
			name:   "gc",
			desc:   "Inspect (and with confirm=true, clean up) vault GC orphans. Dry-run by default",
			params: []string{"vault_path", "confirm"},
		},
		{
			name:   "migration_doc",
			desc:   "Return or (with write=true) regenerate _mnemo/MIGRATION.md, the once-written v1→v2 explainer. Preview-only by default",
			params: []string{"write"},
		},
		{
			name: "bridge_list",
			desc: "List the vault bridges mnemo maintains: each collection's anchor file, whether its fenced block is written, and per-bridge errors from the last sync",
		},
		{
			name:   "recluster",
			desc:   "Trigger an immediate document-level themes clustering pass. Local TF-IDF by default; embeddings are opt-in",
			params: []string{"engine", "force_reembed"},
		},
		{
			name:   "themes_inspect",
			desc:   "Full membership, centroid, pin/archive state, and labelling path for a theme id or slug",
			params: []string{"theme"},
		},
		{
			name:   "themes_pin",
			desc:   "Pin/unpin a theme so it is exempt from retire_after auto-archive",
			params: []string{"theme", "unpin", "reason"},
		},
		{
			name:   "themes_split",
			desc:   "STUB — records a theme_overrides row only; live apply ships in a follow-up",
			params: []string{"theme"},
		},
		{
			name:   "themes_merge",
			desc:   "STUB — records a theme_overrides row only; live apply ships in a follow-up",
			params: []string{"theme", "with"},
		},
	},
}

// vaultTool builds the consolidated vault tool definition. The op
// enumeration comes from vaultOps.describe(), so what an agent reads
// and what the dispatcher accepts cannot drift (convention rule 4).
func vaultTool() mcp.Tool {
	return mcp.NewTool("mnemo_vault",
		mcp.WithDescription(`Vault operations — configuration, sync, GC, bridges, migration and themes. Requires vault_path to be configured (see mnemo_config).

Two ops are STUBS and say so: themes_split and themes_merge record an override row and do not apply live. Everything else is fully implemented.

Ops:`+vaultOps.describe()),
		mcp.WithString("op", mcp.Required(), mcp.Description("Operation to perform — see the list above")),
		mcp.WithString("vault_path", mcp.Description("op=gc: vault root to inspect")),
		mcp.WithBoolean("confirm", mcp.Description("op=gc: actually remove orphans (default false = dry run)")),
		mcp.WithBoolean("write", mcp.Description("op=migration_doc: write the doc rather than preview it")),
		mcp.WithString("engine", mcp.Description("op=recluster: override the clustering engine (heuristic|embeddings)")),
		mcp.WithBoolean("force_reembed", mcp.Description("op=recluster: recompute cached embeddings")),
		mcp.WithString("theme", mcp.Description("op=themes_*: theme id or slug")),
		mcp.WithString("with", mcp.Description("op=themes_merge: the theme to merge with")),
		mcp.WithBoolean("unpin", mcp.Description("op=themes_pin: unpin instead of pinning")),
		mcp.WithString("reason", mcp.Description("op=themes_pin: why, recorded with the override")),
	)
}

// vault dispatches a mnemo_vault call to the operation handlers, which
// are unchanged from when each was its own tool — consolidation moves
// the entry point, not the behaviour.
func (h *callHandler) vaultDispatch(args map[string]any, ctl ConfigController) (string, bool, error) {
	op, err := vaultOps.resolve(args)
	if err != nil {
		return err.Error(), true, nil
	}
	switch op {
	case "status":
		return h.vaultStatus(ctl)
	case "sync":
		return h.vaultSync()
	case "gc":
		return h.vaultGC(args)
	case "migration_doc":
		return h.vaultMigrationDoc(args)
	case "bridge_list":
		return h.vaultBridgeList(ctl)
	case "recluster":
		return h.vaultRecluster(args, ctl)
	case "themes_inspect":
		return h.vaultThemesInspect(args)
	case "themes_pin":
		return h.vaultThemesPin(args)
	case "themes_split":
		return h.vaultThemesSplit(args)
	case "themes_merge":
		return h.vaultThemesMerge(args)
	}
	// Unreachable: resolve accepts only the ops above. Kept so a new
	// opSpec without a case here fails loudly rather than silently.
	return "mnemo_vault: op " + op + " has no handler", true, nil
}
