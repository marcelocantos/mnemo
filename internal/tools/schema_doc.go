// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

// Generated schema catalogue for mnemo_query (🎯T156).
//
// The catalogue used to live inside mnemo_query's tool description: ~5 KB
// of table listings carried in every session's context whether or not the
// agent ever wrote SQL. It was also the single largest item on the tool
// surface, and — being hand-maintained — it was wrong. After 🎯T151 and
// 🎯T152 it still documented `messages.text` and `entries.raw` and had
// never heard of messages_v, docs_v or entries_v, so an agent following
// it wrote a query that silently returned empty strings.
//
// Both problems have one cause and one fix: generate the structure from
// the live database. Table and column names now come from sqlite_master
// and pragma_table_info at call time, so they cannot drift from what the
// database actually exposes. Prose that a machine cannot infer — which
// view to read, what an FTS table is for — stays hand-written in
// tableNotes, with schema_doc_test.go failing the build when a note
// names a table that no longer exists.
//
// It is reachable two ways because MCP clients vary: as the
// `mnemo://schema` resource, and as mnemo_query(describe=true) for
// clients that do not surface resources.

import (
	"fmt"
	"sort"
	"strings"
)

// tableNotes is the curated half: what a machine cannot read off the
// schema. Only tables worth a sentence appear here; the rest are listed
// with their columns and nothing else.
var tableNotes = map[string]string{
	"entries_v": "READ ENTRIES HERE. Every JSONL line, with raw decoded and the hot " +
		"fields (uuid, model, token counts, …) served from materialised columns. " +
		"The base table `entries` stores raw compressed with those columns NULL. " +
		"Entry types: user, assistant, progress, system, file-history-snapshot. " +
		"Use json_extract(raw, '$.path') for fields without a column.",
	"messages_v": "READ MESSAGE TEXT HERE. Content blocks from user/assistant entries; " +
		"entry_id links to entries_v. The base table `messages` stores text " +
		"compressed with text = '' for those rows. tool_use fields: tool_name, " +
		"tool_use_id, tool_input (JSONB), content_type, plus tool_* columns " +
		"extracted from tool_input.",
	"docs_v":       "READ DOC CONTENT HERE; the base table `docs` holds it compressed.",
	"messages_fts": "FTS5 over message text, excluding noise. Use: WHERE messages_fts MATCH 'terms'.",
	"docs_fts":     "FTS5 on title, content, repo.",
	"sessions":     "View joining session_summary and session_meta.",
	"session_meta": "Per-session metadata: repo, cwd, git_branch, work_type, topic.",
	"snapshot_files": "Auto-extracted from file-history-snapshot entries by trigger; " +
		"pair with snapshot_files_fts to find which sessions touched a file.",
	"targets":        "Convergence targets parsed from docs/targets.md. Note that bullseye.yaml is NOT indexed.",
	"compression_gc": "Cursor for the per-row compression backfill; see mnemo_ops op=compress_status.",
	"reconciled_costs": "Authoritative daily costs, present only when cost reconciliation is " +
		"enabled; absent means usage figures are estimated from transcript tokens.",
}

// schemaCatalogue renders the catalogue from the live database.
func schemaCatalogue(q func(string, ...any) ([]map[string]any, error)) (string, error) {
	// Paginated deliberately: Store.Query truncates at 100 rows with a
	// bare break — no error, no marker — and the live database has ~169
	// schema objects. A single query would have silently omitted the tail,
	// and since sqlite_master is in creation order and the views are
	// declared last, the tail is exactly entries_v / messages_v / docs_v:
	// the ones this catalogue exists to point agents at. Keying off name
	// makes the cap irrelevant whatever it is set to.
	var objs []map[string]any
	last := ""
	for {
		page, err := q(`
			SELECT name, type FROM sqlite_master
			WHERE type IN ('table','view')
			  AND name NOT LIKE 'sqlite_%'
			  AND name NOT LIKE '%_data' AND name NOT LIKE '%_idx'
			  AND name NOT LIKE '%_content' AND name NOT LIKE '%_docsize'
			  AND name NOT LIKE '%_config'
			  AND name > ?
			ORDER BY name
			LIMIT 50`, last)
		if err != nil {
			return "", fmt.Errorf("read schema: %w", err)
		}
		if len(page) == 0 {
			break
		}
		objs = append(objs, page...)
		n, _ := page[len(page)-1]["name"].(string)
		if n == "" || n == last {
			break
		}
		last = n
	}

	var b strings.Builder
	b.WriteString("mnemo database schema (generated from the live database)\n\n")
	b.WriteString("Read-only. SELECT/WITH or sqldeep nested syntax.\n\n")

	// Lead with the views agents must use, then everything else.
	lead := []string{"entries_v", "messages_v", "docs_v", "sessions"}
	seen := map[string]bool{}
	names := make([]string, 0, len(objs))
	for _, row := range objs {
		n, _ := row["name"].(string)
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	render := func(name string) {
		cols, err := q(`SELECT name FROM pragma_table_info(?)`, name)
		if err != nil {
			return
		}
		var cn []string
		for _, c := range cols {
			if s, _ := c["name"].(string); s != "" {
				cn = append(cn, s)
			}
		}
		fmt.Fprintf(&b, "  %s (%s)\n", name, strings.Join(cn, ", "))
		if note := tableNotes[name]; note != "" {
			for _, line := range wrapText(note, 68) {
				fmt.Fprintf(&b, "      %s\n", line)
			}
		}
	}

	b.WriteString("Primary surfaces:\n")
	for _, n := range lead {
		for _, have := range names {
			if have == n {
				render(n)
				seen[n] = true
			}
		}
	}
	b.WriteString("\nEverything else:\n")
	for _, n := range names {
		if !seen[n] {
			render(n)
		}
	}
	b.WriteString(`
Join pattern — message with its entry metadata:
  SELECT m.text, e.model, e.input_tokens FROM messages_v m JOIN entries_v e ON e.id = m.entry_id

Token usage:
  SELECT date(timestamp) AS day, SUM(input_tokens) AS input, SUM(output_tokens) AS output
  FROM entries_v WHERE type = 'assistant' GROUP BY day ORDER BY day DESC
`)
	return b.String(), nil
}

// wrapText is a small greedy wrapper for note prose.
func wrapText(s string, width int) []string {
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
