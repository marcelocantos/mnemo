// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

// TestUsageIndexPresentAndAnalyzable verifies the 🎯T93 fix end to end at
// unit scale: the usage covering index ships in the fresh schema, Optimize
// runs cleanly, and ANALYZE produces a planner-statistics row for the
// index — the missing piece that previously left the index unused (the
// planner fell back to a full assistant-table scan). A plan-choice
// assertion is intentionally omitted: SQLite rightly prefers a full scan
// on a tiny test table, so index selection is validated on the
// production-size DB, not here.
func TestUsageIndexPresentAndAnalyzable(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// The index is part of the fresh schema.
	var name string
	if err := s.readDB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_entries_assistant_usage'`,
	).Scan(&name); err != nil {
		t.Fatalf("usage index missing from fresh schema: %v", err)
	}

	// Seed assistant entries so the index has rows to analyse.
	raw := `{"message":{"model":"claude","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}}`
	now := time.Now().UTC().Format(time.RFC3339)
	for range 50 {
		if _, err := s.writeDB.Exec(
			`INSERT INTO entries (session_id, project, type, timestamp, raw) VALUES (?,?,?,?,?)`,
			"s1", "p", "assistant", now, raw); err != nil {
			t.Fatalf("insert entry: %v", err)
		}
	}

	// Optimize must be a clean no-op-or-analyze (best-effort maintenance).
	s.Optimize()

	// An explicit ANALYZE must record statistics for the usage index — the
	// signal the query planner needs to choose it over the type index.
	if _, err := s.writeDB.Exec("ANALYZE"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	var n int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM sqlite_stat1 WHERE idx='idx_entries_assistant_usage'`).Scan(&n); err != nil {
		t.Fatalf("read sqlite_stat1: %v", err)
	}
	if n == 0 {
		t.Fatalf("expected a sqlite_stat1 row for idx_entries_assistant_usage after ANALYZE")
	}
}
