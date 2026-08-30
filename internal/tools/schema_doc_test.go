// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestSchemaCatalogueIsGeneratedFromTheDatabase is the 🎯T156 oracle for
// the catalogue that used to live in mnemo_query's description.
//
// The point is not that it renders — it is that it renders from the
// database. The old hand-written listing still documented messages.text
// and entries.raw after 🎯T151/🎯T152 compressed them, so an agent
// following it wrote a query that silently returned empty strings. A
// generated catalogue cannot describe a schema the database does not
// have.
func TestSchemaCatalogueIsGeneratedFromTheDatabase(t *testing.T) {
	// A stub standing in for the live database: whatever it reports is
	// what must appear, and nothing else.
	q := func(sql string, args ...any) ([]map[string]any, error) {
		if strings.Contains(sql, "pragma_table_info") {
			return []map[string]any{{"name": "id"}, {"name": "invented_column"}}, nil
		}
		// Paginated: return the page after args[0], then nothing.
		after, _ := args[0].(string)
		all := []map[string]any{
			{"name": "entries_v", "type": "view"},
			{"name": "invented_table", "type": "table"},
			{"name": "messages_v", "type": "view"},
		}
		var page []map[string]any
		for _, r := range all {
			if n, _ := r["name"].(string); n > after {
				page = append(page, r)
			}
		}
		return page, nil
	}
	doc, err := schemaCatalogue(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"entries_v", "messages_v", "invented_table", "invented_column"} {
		if !strings.Contains(doc, want) {
			t.Errorf("catalogue omits %q reported by the database", want)
		}
	}
	// Nothing the database did not report may appear as a listed object.
	for _, unwanted := range []string{"  audit_entries (", "  ci_runs (", "  plans ("} {
		if strings.Contains(doc, unwanted) {
			t.Errorf("catalogue lists %q, which the database did not report — it is not generated", strings.TrimSpace(unwanted))
		}
	}
	// The curated guidance survives for tables that are present.
	if !strings.Contains(doc, "READ ENTRIES HERE") {
		t.Error("curated note for entries_v is missing")
	}
}

// TestTableNotesNameRealTables: curated prose is the half a machine
// cannot generate, so it is also the half that can go stale. A note for
// a table the shipped schema no longer declares is a stale note.
func TestTableNotesNameRealTables(t *testing.T) {
	schema := shippedSchemaObjects(t)
	for name := range tableNotes {
		if !schema[name] {
			t.Errorf("tableNotes documents %q, which the shipped schema does not declare", name)
		}
	}
}

// shippedSchemaObjects returns every table and view name declared by
// schema.sql, read from the store package's embedded copy via a
// throwaway database.
func shippedSchemaObjects(t *testing.T) map[string]bool {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "t.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AwaitStartup()
	// Ask only about the names under test: Store.Query caps at 100 rows
	// and the schema has ~169 objects, so an unfiltered listing silently
	// drops the tail — which is where the views live.
	out := map[string]bool{}
	for name := range tableNotes {
		rows, err := s.Query(`SELECT name FROM sqlite_master WHERE name = ?`, name)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			out[name] = true
		}
	}
	return out
}
