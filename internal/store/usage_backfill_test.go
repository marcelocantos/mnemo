// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// The usage twins (🎯T165) are written at ingest and backfilled onto
// historical rows by the entries.usage pass. This test covers the three
// things that make the pass safe to run against a live index: rows an
// older binary wrote still read correctly through entries_v before it
// runs, the pass fills them, and filling them changes no usage figure.
func TestEntriesUsageTwinsBackfilled(t *testing.T) {
	projectDir := t.TempDir()
	first := entryFixture("u-1", "claude-fable-5", longText("u1", 20), "2026-04-01T10:00:00Z", 100, 20)
	first["requestId"] = "req-1"
	first["message"].(map[string]any)["usage"].(map[string]any)["cache_creation"] =
		map[string]any{"ephemeral_5m_input_tokens": 11, "ephemeral_1h_input_tokens": 22}
	writeJSONL(t, projectDir, "proj", "sess-u1", []map[string]any{
		first,
		entryFixture("u-2", "claude-fable-5", longText("u2", 20), "2026-04-01T10:01:00Z", 200, 40),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Ingest writes the twins, so a fresh store has nothing to backfill.
	if n, err := s.EntriesUsageOutstanding(); err != nil || n != 0 {
		t.Fatalf("outstanding after ingest = %d, %v; want 0", n, err)
	}
	window := UsageParams{GroupBy: "day", Since: "2026-01-01T00:00:00Z", Until: "2026-12-31T00:00:00Z"}
	want, err := s.Usage(window)
	if err != nil {
		t.Fatal(err)
	}

	// Rewind to what a pre-🎯T165 binary left behind.
	if _, err := s.writeDB.Exec(`UPDATE entries SET message_id_m = NULL, request_id_m = NULL,
		cache_write_5m_m = NULL, cache_write_1h_m = NULL WHERE type = 'assistant'`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.EntriesUsageOutstanding(); err != nil || n != 2 {
		t.Fatalf("outstanding after rewind = %d, %v; want 2", n, err)
	}

	// entries_v answers from the decode fallback while the twins are
	// NULL: the pass is a performance fix, not a correctness one.
	assertRow := func(stage string) {
		t.Helper()
		var msgID, reqID string
		var cw5m, cw1h int64
		if err := s.readDB.QueryRow(`SELECT message_id, request_id, cache_write_5m, cache_write_1h
			FROM entries_v WHERE uuid = 'u-1'`).Scan(&msgID, &reqID, &cw5m, &cw1h); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if msgID != "msg-u-1" || reqID != "req-1" || cw5m != 11 || cw1h != 22 {
			t.Fatalf("%s: entries_v = %q %q %d %d", stage, msgID, reqID, cw5m, cw1h)
		}
	}
	assertRow("before backfill")
	if got, err := s.Usage(window); err != nil || got.Total != want.Total {
		t.Fatalf("usage before backfill = %+v, %v; want %+v", got.Total, err, want.Total)
	}

	res, err := s.MaterialiseEntriesUsage(context.Background())
	if err != nil || !res.Done || res.Compressed != 2 {
		t.Fatalf("backfill: %+v %v", res, err)
	}
	if n, err := s.EntriesUsageOutstanding(); err != nil || n != 0 {
		t.Fatalf("outstanding after backfill = %d, %v; want 0", n, err)
	}
	assertRow("after backfill")
	if got, err := s.Usage(window); err != nil || got.Total != want.Total {
		t.Fatalf("usage after backfill = %+v, %v; want %+v", got.Total, err, want.Total)
	}

	// The twins are now the source, not the fallback.
	var msgID, reqID string
	if err := s.readDB.QueryRow(`SELECT message_id_m, request_id_m FROM entries WHERE uuid_m = 'u-1'`).
		Scan(&msgID, &reqID); err != nil {
		t.Fatal(err)
	}
	if msgID != "msg-u-1" || reqID != "req-1" {
		t.Fatalf("twins = %q %q", msgID, reqID)
	}

	// Idempotent: a second run finds an empty queue.
	res, err = s.MaterialiseEntriesUsage(context.Background())
	if err != nil || !res.Done || res.Rows != 0 {
		t.Fatalf("second backfill: %+v %v", res, err)
	}
}
