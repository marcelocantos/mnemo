// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Tests for the /health and dashboard latency root-cause fixes:
// leftover-membership indexes, COUNT-not-SUM(length) status, and
// Usage/token aggregates on entries + *_m instead of entries_v.

func TestPlainLeftoverIndexesExist(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for _, name := range []string{"idx_messages_plain", "idx_docs_plain", "idx_entries_plain"} {
		var got string
		if err := s.readDB.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name,
		).Scan(&got); err != nil {
			t.Fatalf("missing leftover-membership index %s: %v", name, err)
		}
	}
}

func TestLeftoverCountUsesPlainIndex(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	rows, err := s.readDB.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM messages WHERE text_z IS NULL AND id >= 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	got := plan.String()
	if !strings.Contains(got, "idx_messages_plain") {
		t.Fatalf("leftover COUNT does not use idx_messages_plain.\nPlan:\n%s", got)
	}
}

func TestCompressionStatusIsMembershipNotBlobSum(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 12)
	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	var msg FamilyStatus
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText {
			msg = f
		}
	}
	if msg.Rows != 12 {
		t.Fatalf("rows=%d, want 12", msg.Rows)
	}
	if msg.Outstanding != 12 {
		t.Fatalf("outstanding leftover=%d, want 12 (membership, not bytes)", msg.Outstanding)
	}
	if msg.PlainBytes != 0 || msg.PackedBytes != 0 {
		t.Fatalf("byte totals must not be live-scanned: plain=%d packed=%d", msg.PlainBytes, msg.PackedBytes)
	}

	if _, err := s.CompressBackfill(context.Background(), FamilyMessagesText); err != nil {
		t.Fatal(err)
	}
	st, err = s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText {
			if !f.BackfillDone {
				t.Fatal("expected backfill done")
			}
			if f.Outstanding != 0 {
				t.Fatalf("outstanding after done=%d, want 0 (since-cursor, not historical shorts)", f.Outstanding)
			}
			if f.Compressed != 12 {
				t.Fatalf("compressed=%d, want 12", f.Compressed)
			}
		}
	}
}

func TestAutoBackfillShortCircuitsWhenDone(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 8)
	if _, err := s.CompressBackfill(context.Background(), FamilyMessagesText); err != nil {
		t.Fatal(err)
	}
	if err := s.saveBackfillCursor(FamilyMessagesText, 1<<20, 0, true); err != nil {
		t.Fatal(err)
	}

	s.compressBackfillCycle(t.Context())

	var done int
	if err := s.readDB.QueryRow(`SELECT done FROM compression_gc WHERE family = ?`, FamilyMessagesText).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 1 {
		t.Fatal("done family with no new compressible rows must not be reopened")
	}
}

func TestUsageReadsEntriesMaterialisedColumns(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	raw := `{"uuid":"u-usage","message":{"id":"msg-1","model":"claude-fable-5","usage":{"input_tokens":40,"output_tokens":6,"cache_read_input_tokens":2,"cache_creation_input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":1}}}}`
	if _, err := s.writeDB.Exec(
		`INSERT INTO entries (session_id, project, type, timestamp, raw) VALUES (?,?,?,?,jsonb(?))`,
		"sess-usage", "proj", "assistant", "2026-04-01T10:00:00Z", raw); err != nil {
		t.Fatal(err)
	}

	var mid, model string
	var in, cw5m int64
	if err := s.readDB.QueryRow(`
		SELECT message_id_m, model_m, input_tokens_m, cache_write_5m_m
		FROM entries WHERE session_id = 'sess-usage'`).Scan(&mid, &model, &in, &cw5m); err != nil {
		t.Fatal(err)
	}
	if mid != "msg-1" || model != "claude-fable-5" || in != 40 || cw5m != 1 {
		t.Fatalf("usage columns: id=%s model=%s in=%d cw5m=%d", mid, model, in, cw5m)
	}

	usage, err := s.Usage(UsageParams{GroupBy: "model", Since: "2026-01-01T00:00:00Z", Until: "2026-12-31T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.Total.InputTokens != 40 {
		t.Fatalf("usage input=%d, want 40 (keyed via message_id_m)", usage.Total.InputTokens)
	}
}

func TestMaterialiseUsageFieldsFillsPackedRows(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	e := entryFixture("u-pack", "claude-opus-5", longText("packed", 8), "2026-04-01T10:00:00Z", 11, 3)
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(entryInsertSQL, "sess-pack", "proj", "assistant", "2026-04-01T10:00:00Z", string(line), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompressBackfill(context.Background(), FamilyEntriesRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`
		UPDATE entries SET
			message_id_m = NULL, request_id_m = NULL, cache_write_5m_m = NULL,
			src_uuid_m = NULL, attribution_skill_m = NULL, attribution_agent_m = NULL
		WHERE session_id = 'sess-pack'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`DELETE FROM compression_gc WHERE family = ?`, entriesUsageFieldsFamily); err != nil {
		t.Fatal(err)
	}

	res, err := s.MaterialiseUsageFields(context.Background())
	if err != nil || !res.Done {
		t.Fatalf("materialise usage fields: %+v %v", res, err)
	}
	var mid string
	if err := s.readDB.QueryRow(`SELECT message_id_m FROM entries WHERE session_id = 'sess-pack'`).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if mid != "msg-u-pack" {
		t.Fatalf("message_id_m=%q after packed-row backfill", mid)
	}
}

func TestUsageAndAddendaSQLAvoidEntriesV(t *testing.T) {
	// Source ratchet: the hot SELECT lists must not go through entries_v,
	// whose COALESCE/mnemo_raw make idx_entries_*_m unusable.
	offenders := []string{
		`FROM entries_v e
			%s
			WHERE %s
			GROUP BY %s
		)
		SELECT
			%s AS period`,
		`FROM entries_v
			WHERE input_tokens IS NOT NULL`,
		`FROM entries_v e
		        WHERE e.session_id = s.session_id
		          AND e.type = 'assistant'
		          AND e.id > COALESCE((`,
	}
	// If someone reverts the rewrites, these fragments return.
	src := usageHotPathSource(t)
	for _, frag := range offenders {
		if strings.Contains(src, frag) {
			t.Errorf("hot token path still uses entries_v:\n%s", frag)
		}
	}
	for _, want := range []string{
		"FROM entries e",
		"e.model_m",
		"e.input_tokens_m",
		"e.message_id_m",
		"INDEXED BY idx_entries_addenda_m",
		"INDEXED BY idx_entries_assistant_usage_m",
		"input_tokens_m",
		"FROM entries",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("hot token path missing %q", want)
		}
	}
}

func usageHotPathSource(t *testing.T) string {
	t.Helper()
	// Concatenate the files that own the rewritten queries so a revert
	// fails here rather than only on a 7.8 GiB laptop.
	var b strings.Builder
	for _, name := range []string{"store.go", "compactions.go", "agenttree.go", "../api/api.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}
