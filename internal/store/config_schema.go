// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Configuration schema documentation (🎯T156).
//
// Configuration is file-only: the user edits ~/.mnemo/config.json and the
// daemon watches it. That makes documenting the schema the whole
// interface, so the documentation must not be able to drift from the
// struct.
//
// The key list is therefore REFLECTED from Config's json tags, never
// hand-maintained. That is not a style preference — the tool this
// replaced learned it the hard way: it validated patches against a
// hand-written key list, and when menu_bar_app and threads_root were
// added to the struct but not the list, the tool rejected them as
// "unknown config keys". Prose still has to be written by a human, so
// config_schema_test.go fails the build when a field has no description
// or a description names no field.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// configFieldDoc is the human half of the schema: what a key means and
// whether changing it takes effect without a restart.
type configFieldDoc struct {
	// summary is one line describing what the key controls.
	summary string
	// detail is optional additional prose, wrapped when rendered.
	detail string
	// hotReload is true when the daemon adopts a change to this key
	// without a restart. Must match what registry.Reload actually does.
	hotReload bool
}

// configDocs describes every top-level key. A key here that Config does
// not declare, or a Config field with no entry here, fails the ratchet.
var configDocs = map[string]configFieldDoc{
	"vault_path": {
		summary:   "Directory for the Obsidian-style knowledge vault.",
		detail:    "Empty disables vault export. A leading ~ is expanded.",
		hotReload: true,
	},
	"vault_profile": {
		summary:   "Vault export profile controlling which document kinds are written.",
		hotReload: true,
	},
	"vault_bridges": {
		summary:   "Emit cross-reference bridge links between vault documents.",
		hotReload: true,
	},
	"vault_bridges_max_links": {
		summary:   "Cap on bridge links emitted per document.",
		hotReload: true,
	},
	"vault_clustering": {
		summary: "Clustering engine and label engine for vault themes.",
		detail: "engine: \"heuristic\" (default, fully offline) or \"embeddings\" " +
			"(requires VOYAGE_API_KEY). label.engine: \"bigram\" (default) or " +
			"\"llm\" (requires ANTHROPIC_API_KEY). Both are opt-in egress.",
	},
	"workspace_roots": {
		summary:   "Filesystem roots under which repo-level streams discover repos.",
		hotReload: true,
	},
	"extra_project_dirs": {
		summary:   "Additional Claude Code project directories to ingest.",
		detail:    "Used for cross-platform transcript ingest, e.g. a Windows VM's projects over SMB.",
		hotReload: true,
	},
	"synthesis_roots": {
		summary:   "Filesystem roots walked for synthesis documents.",
		hotReload: true,
	},
	"threads_root": {
		summary:   "Root directory for thread workspaces.",
		hotReload: true,
	},
	"plugins": {
		summary:   "Per-plugin enable/disable map.",
		hotReload: true,
	},
	"terminal": {
		summary:   "Which terminal opens threads and resumes sessions.",
		detail:    "backend: \"iterm2\" (default) or \"cmux\". Read per open; an unsupported value is rejected at load.",
		hotReload: true,
	},
	"menu_bar_app": {
		summary:   "Show the macOS menu-bar status item.",
		detail:    "Chrome only — the shim's process lifecycle is independent.",
		hotReload: true,
	},
	"auto_upgrade": {
		summary:   "Automatic upgrade checking and application.",
		detail:    "enabled plus quiescence (e.g. \"5m\"). Apply runs only on Homebrew non-Windows installs.",
		hotReload: true,
	},
	"linked_instances": {
		summary: "Peer mnemo daemons for federated search.",
		detail: "Persisted but NOT adopted live: a change here needs a daemon " +
			"restart. Absent or empty means zero federation calls.",
	},
	"cost_reconciliation": {
		summary: "Fetch authoritative daily costs from the Anthropic Admin API.",
		detail: "Disabled by default. Needs BOTH enabled:true here and " +
			"ANTHROPIC_ADMIN_API_KEY in the daemon's environment — the key " +
			"alone triggers no outbound call.",
	},
	"pricing": {
		summary: "Fetch the model rate card used to cost token usage.",
		detail: "Disabled by default; with it off every model reports as unpriced " +
			"rather than as $0.00. source_url overrides the upstream for an " +
			"air-gapped mirror; refresh_hours sets the interval (default 24).",
	},
	"image_embeddings": {
		summary: "CLIP image embeddings for semantic image search.",
		detail: "Disabled by default. Enabling it allows a subprocess to resolve " +
			"Python dependencies from PyPI and download ~340 MB of model weights " +
			"from HuggingFace on first use.",
	},
	"budget": {
		summary: "Monthly spend cap and warning threshold.",
		detail:  "monthly_cap_usd, timezone, warn_at_pct. Throttling only governs agents mnemo invokes itself.",
	},
	"dedup_key": {
		summary: "Which identifier deduplicates billable records.",
		detail: "Default \"message_request\". Environment-dependent: validate it " +
			"against your billing source for each serving path. An unrecognised " +
			"value resolves to the default rather than silently disabling dedup.",
	},
	"backup": {
		summary: "Daily backup snapshots: retention, window, quiescence.",
		detail: "keep_dailies defaults to 1 and is shared across all tags " +
			"(daily, pre-migration, manual). The replacement is always written " +
			"and verified before its predecessor is deleted, so the peak on " +
			"disk is two snapshots plus the uncompressed VACUUM temp.",
	},
	"connection_sweep": {
		summary: "Sweep interval and staleness threshold for daemon connection records.",
	},
	"streaming_segmentation": {
		summary: "Streaming topic segmentation: model, drip size, concurrency.",
	},
	"disable_ocr": {
		summary: "Turn off image OCR.",
		detail: "The off switch for a machine whose Vision/Metal stack is broken, " +
			"where every image otherwise costs a doomed process spawn.",
	},
	"disable_health_notifications": {
		summary: "Silence native health notifications.",
		detail: "Opt-out: fail-severity notifications are on by default. The " +
			"dashboard health page and doctor are unaffected.",
	},
	"disable_upgrade_check": {
		summary: "Stop checking for new releases.",
		detail:  "Opt-out. When true, zero outbound release-list calls are made.",
	},
	"signal_sources": {
		summary: "Liveness signals evaluated by the diagnostics surface without a plugin process.",
		detail: "Each stanza names a kind (file_mtime, launchd, newest_artifact, " +
			"last_commit), a path or label, an expected cadence and a grace multiple.",
	},
	"vault_layout": {
		summary: "Which directory layout the vault exporter writes.",
		detail:  "\"v2\", \"both\" during a migration window, or \"v1\" as an emergency escape.",
	},
	"vault_layout_soak_warn_after": {
		summary: "How long to soak on the v1 layout before warning to opt into v2.",
		detail:  "Empty defaults to \"720h\" (30 days). The warning never narrows the layout by itself.",
	},
	"vault_indexing_scope": {
		summary: "How much of the vault is indexed.",
		detail: "\"_mnemo_only\" (safest, the default for fresh vaults), \"includes\", " +
			"or \"full\" (the default for pre-existing v1-populated vaults).",
	},
	"vault_indexing_includes": {
		summary: "Extra vault subpaths to index when vault_indexing_scope is \"includes\".",
		detail:  "Forward slashes; `..` and absolute paths are rejected at validation.",
	},
	"vault_indexing_ignore_file": {
		summary: "Name of the ignore file consulted at the vault root.",
		detail:  "Empty defaults to \".mnemoignore\". Only this single file is consulted; nested ones are not.",
	},
	"todo_globs": {
		summary: "DEPRECATED — accepted but inert.",
		detail: "Extra globs for the TODO indexer (🎯T78), which was removed with " +
			"its tools in 🎯T143.1. The key is still parsed so an existing " +
			"config.json does not fail to load; it has no effect. 🎯T148.4 " +
			"tracks removing the remaining config, watch and schema residue.",
	},
	"summariser": {
		summary: "Provider and model for compaction summaries.",
		detail:  "provider: \"claude\" or \"grok\". Auto mode prefers Grok when its CLI is on PATH.",
	},
	"compression": {
		summary: "Automatic historical-row compression backfill.",
		detail: "auto_backfill defaults on: the daemon packs leftover plain rows " +
			"when it finds a backlog. Set false only to pause the worker. " +
			"VACUUM stays manual — packing empties columns, it does not shrink the file.",
		hotReload: true,
	},
}

// ConfigKeys returns every top-level JSON key Config declares, sorted.
// Reflected, so it cannot drift from the struct.
func ConfigKeys() []string {
	var keys []string
	t := reflect.TypeOf(Config{})
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

// ConfigSchemaDoc renders the configuration reference for
// `mnemo --help-config`.
func ConfigSchemaDoc(path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mnemo configuration\n\n")
	fmt.Fprintf(&b, "  File: %s\n", path)
	b.WriteString("  Format: JSON object; every key is optional and omitted keys take their default.\n")
	b.WriteString("  Editing: the file is the only writer. The daemon watches it and adopts\n")
	b.WriteString("  changes marked [live] below without a restart; the rest need one.\n\n")

	var live, restart []string
	for _, k := range ConfigKeys() {
		if configDocs[k].hotReload {
			live = append(live, k)
		} else {
			restart = append(restart, k)
		}
	}
	render := func(title string, keys []string) {
		if len(keys) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s\n\n", title)
		for _, k := range keys {
			d := configDocs[k]
			fmt.Fprintf(&b, "  %s\n      %s\n", k, d.summary)
			if d.detail != "" {
				for _, line := range wrapAt(d.detail, 68) {
					fmt.Fprintf(&b, "      %s\n", line)
				}
			}
			b.WriteString("\n")
		}
	}
	render("Adopted live [live]", live)
	render("Require a daemon restart", restart)
	return b.String()
}

// wrapAt is a minimal greedy wrapper; the doc text is short enough that
// anything cleverer would be noise.
func wrapAt(s string, width int) []string {
	var out []string
	line := ""
	for _, w := range strings.Fields(s) {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) <= width:
			line += " " + w
		default:
			out = append(out, line)
			line = w
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
