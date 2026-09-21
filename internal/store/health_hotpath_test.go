// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tokenColRe matches the billable columns whose generated forms are NULL
// on packed rows. A query selecting these from entries_v defeats the
// covering indexes the health-latency work exists to reach.
var tokenColRe = regexp.MustCompile(`\b(input_tokens|output_tokens|cache_read_tokens|cache_creation_tokens|cache_write_5m|cache_write_1h)\b`)

// sqlLiterals returns the string literals of a Go file, so a ratchet can
// judge one query at a time instead of the whole file at once.
func sqlLiterals(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(bl.Value); err == nil {
			out = append(out, v)
		}
		return true
	})
	return out
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestLeftoverMembershipIndexesExist is the schema half of the
// compress.backfill fix: leftover is z IS NULL, and that predicate
// has a partial index so status/packer membership does not scan blobs.
func TestLeftoverMembershipIndexesExist(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for _, name := range []string{
		"idx_messages_text_z_null",
		"idx_docs_content_z_null",
		"idx_entries_raw_z_null",
	} {
		var n int
		if err := s.readDB.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name,
		).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s missing: n=%d err=%v", name, n, err)
		}
	}
}

// TestCompressionStatusUsesStoredLengths is the functional half: after
// leftover lengths are filled, status outstanding is SUM(plain_len),
// and a finished family (only short leftover) reports outstanding 0
// without needing length(blob) on packed rows.
func TestCompressionStatusUsesStoredLengths(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 12)
	// Short rows stay leftover forever; they must not count as outstanding
	// once plain_len is stored.
	mustExec(t, s, `INSERT INTO messages
		(session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('sess-auto', 'proj', 'assistant', 'tiny', '2026-04-01T10:00:00Z', 'assistant', 0, 'text')`)
	// These inserts leave plain_len NULL. Measuring is the background
	// worker's job now, never the status call's (🎯T173), so do here what
	// the worker does at the top of each cycle.
	measureAllFamilies(t, s)

	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	var fam FamilyStatus
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText {
			fam = f
		}
	}
	if fam.LeftoverRows != 12 {
		t.Fatalf("leftover rows=%d, want 12 long seeded rows (short excluded)", fam.LeftoverRows)
	}
	if fam.Outstanding == 0 {
		t.Fatal("seeded long rows should have outstanding plain_len > 0")
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
			if f.Outstanding != 0 || f.LeftoverRows != 0 {
				t.Fatalf("after pack: outstanding=%d leftover_rows=%d, want 0/0", f.Outstanding, f.LeftoverRows)
			}
			if f.PlainBytes == 0 {
				t.Fatal("short leftover should still contribute to PlainBytes")
			}
		}
	}
}

// TestLeftoverCountUsesPartialIndex asserts the membership probe is an
// index scan of idx_*_z_null, not a table walk that would touch overflow.
func TestLeftoverCountUsesPartialIndex(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	rows, err := s.readDB.Query(`EXPLAIN QUERY PLAN
		SELECT COUNT(*), COALESCE(SUM(plain_len), 0)
		FROM messages
		WHERE text_z IS NULL AND (plain_len IS NULL OR plain_len >= 64)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err == nil {
			plan.WriteString(detail + "\n")
		}
	}
	got := plan.String()
	// Either partial index is acceptable and both are index-only:
	// idx_messages_text_z_null carries id, idx_messages_text_plain_len
	// carries the summed column (🎯T181), and the planner picks between
	// them. What must never appear is a table scan — on an unpacked row
	// that means reading the payload to reach an integer beside it.
	if !strings.Contains(got, "idx_messages_text_z_null") &&
		!strings.Contains(got, "idx_messages_text_plain_len") {
		t.Fatalf("leftover probe used neither partial index:\n%s", got)
	}
	if strings.Contains(got, "SCAN messages\n") {
		t.Fatalf("leftover probe scans messages:\n%s", got)
	}
}

// TestHotTokenSQLAvoidsEntriesV is the query-shape ratchet for Usage,
// AgentTrees, context/dbstats, and the compactor addenda sum: those
// SELECTs must read entries + *_m so idx_entries_*_m is usable.
func TestHotTokenSQLAvoidsEntriesV(t *testing.T) {
	root := filepath.Join("..", "..")
	files := []string{
		filepath.Join("internal", "store", "store.go"),
		filepath.Join("internal", "store", "agenttree.go"),
		filepath.Join("internal", "store", "compactions.go"),
		filepath.Join("internal", "api", "api.go"),
		filepath.Join("internal", "store", "compress.go"),
		filepath.Join("internal", "store", "compress_auto.go"),
	}
	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		src := string(body)
		switch rel {
		case filepath.Join("internal", "store", "store.go"):
			// Empty bodies assert nothing. This arm existed and was dead.
			if !strings.Contains(src, "FROM entries e") {
				t.Errorf("%s: Usage/usageByBlock/activeHours must read the base table "+
					"so idx_entries_*_m is usable", rel)
			}
			// Precise, not blanket: store.go legitimately reads entries_v
			// where it needs decoded raw (the per-session image scan). What
			// must never happen is a TOKEN AGGREGATE on entries_v, whose
			// COALESCE/mnemo_raw hides idx_entries_*_m. Check per SQL
			// literal rather than per file.
			for _, lit := range sqlLiterals(t, filepath.Join(root, rel)) {
				if !strings.Contains(lit, "entries_v") {
					continue
				}
				if tokenColRe.MatchString(lit) {
					t.Errorf("%s: a token aggregate is back on entries_v, which hides "+
						"idx_entries_*_m:\n%s", rel, firstLines(lit, 4))
				}
			}
			if strings.Contains(src, "json_extract(e.raw,") || strings.Contains(src, "e.raw->>") {
				t.Errorf("%s still extracts billable fields from e.raw", rel)
			}
			if !strings.Contains(src, "e.message_id_m") || !strings.Contains(src, "e.cache_write_5m_m") {
				t.Errorf("%s missing *_m billable columns", rel)
			}
		case filepath.Join("internal", "store", "agenttree.go"):
			if strings.Contains(src, "FROM entries_v e") {
				t.Errorf("%s still reads entries_v on the token/spawn path", rel)
			}
			if strings.Contains(src, "e.raw->>") {
				t.Errorf("%s still decodes raw for AgentTrees", rel)
			}
		case filepath.Join("internal", "store", "compactions.go"):
			if !strings.Contains(src, "INDEXED BY idx_entries_addenda_m") {
				t.Errorf("%s addenda sum does not pin idx_entries_addenda_m", rel)
			}
			if strings.Contains(src, "SUM(e.output_tokens + e.cache_creation_tokens)") {
				t.Errorf("%s addenda sum still uses entries_v column names", rel)
			}
		case filepath.Join("internal", "api", "api.go"):
			if strings.Contains(src, "FROM entries_v") {
				t.Errorf("%s context/dbstats still aggregate through entries_v", rel)
			}
		case filepath.Join("internal", "store", "compress.go"), filepath.Join("internal", "store", "compress_auto.go"):
			for i, line := range strings.Split(src, "\n") {
				trim := strings.TrimSpace(line)
				if strings.HasPrefix(trim, "//") {
					continue
				}
				if strings.Contains(line, "SUM(length(") {
					t.Errorf("%s:%d live SUM(length()) — that is the overflow-page scan", rel, i+1)
				}
			}
		}
	}
}

// TestMaterialiseUsageFieldsFillsHistoricalRows covers the upgrade path:
// a pre-column INSERT (legacy shape) gets message_id / cache-tier twins
// so Usage can stay off mnemo_raw.
func TestMaterialiseUsageFieldsFillsHistoricalRows(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	mustExec(t, s, `DELETE FROM compression_gc WHERE family = ?`, entriesUsageFieldsFamily)
	raw := `{"uuid":"u1","requestId":"req-9","message":{"id":"msg_hist","model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":1}}}}`
	mustExec(t, s, `INSERT INTO entries (session_id, project, type, timestamp, raw)
		VALUES ('sess-hist', 'p', 'assistant', '2026-04-01T10:00:00Z', jsonb(?))`, raw)
	mustExec(t, s, `UPDATE entries SET message_id_m = NULL, request_id_m = NULL,
		cache_write_5m_m = NULL, cache_write_1h_m = NULL WHERE session_id = 'sess-hist'`)

	res, err := s.MaterialiseUsageFields(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatalf("usage-fields pass not done: %+v", res)
	}
	var mid, rid string
	var cw5, cw1 int64
	if err := s.readDB.QueryRow(`
		SELECT message_id_m, request_id_m, cache_write_5m_m, cache_write_1h_m
		FROM entries WHERE session_id = 'sess-hist'`).Scan(&mid, &rid, &cw5, &cw1); err != nil {
		t.Fatal(err)
	}
	if mid != "msg_hist" || rid != "req-9" || cw5 != 3 || cw1 != 1 {
		t.Fatalf("usage fields: id=%s req=%s 5m=%d 1h=%d", mid, rid, cw5, cw1)
	}
}

// TestUsageReadsMessageIDFromMaterialisedColumn is the end-to-end check
// that a writer-ingested assistant row is keyed (not quarantined) after
// the entries_v → entries.*_m rewrite.
func TestUsageReadsMessageIDFromMaterialisedColumn(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	writeJSONL(t, dir, "p", "sess-usage-m", []map[string]any{
		assistantWithUsage(now(), "claude-sonnet-4-6", 1000, 100, 50, 25),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var mid string
	if err := s.readDB.QueryRow(`SELECT message_id_m FROM entries WHERE type = 'assistant' LIMIT 1`).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if mid == "" {
		t.Fatal("ingest did not fill message_id_m")
	}
	got, err := s.Usage(UsageParams{Days: 30, GroupBy: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total.Messages == 0 {
		t.Fatalf("Usage quarantined the keyed row; uncounted=%+v", got.Uncounted)
	}
}

// TestFamilyStatusAggregatesAreIndexOnly is the ratchet for 🎯T181.
//
// compress_status reports four small integers per family, and all four
// used to be answered by walking the table. That is not a mild
// inefficiency on this schema: the rows being stepped over carry the
// compressed payload, so SQLite drags every blob's overflow pages
// through the page cache to reach an INTEGER beside them. On the
// owner's 21 GiB database the packed-side sum alone took 19.9s for
// entries and 9.3s for messages, and because compress.backfill runs on
// the Fast tier every three minutes the daemon sat at roughly a third
// of a core re-answering a question whose answer had not changed. The
// 20s per-check timeout concealed it instead of stopping it: the check
// reported "did not answer" while the query ran on to completion.
//
// "SCAN <table>" in any of these plans means the regression is back.
// Note the last clause filters on the blob column rather than on z_len:
// the two select the same rows, but only that spelling matches the
// partial index.
func TestFamilyStatusAggregatesAreIndexOnly(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for _, tc := range []struct {
		name, query, wantIndex string
	}{
		{
			"packed rows", "SELECT COUNT(*) FROM messages WHERE text_z IS NOT NULL",
			"idx_messages_text_z_len",
		},
		{
			"packed bytes", "SELECT COALESCE(SUM(z_len), 0) FROM messages WHERE text_z IS NOT NULL",
			"idx_messages_text_z_len",
		},
		{
			"plain bytes", "SELECT COALESCE(SUM(plain_len), 0) FROM messages WHERE text_z IS NULL",
			"idx_messages_text_plain_len",
		},
		{
			"entries packed bytes", "SELECT COALESCE(SUM(z_len), 0) FROM entries WHERE raw_z IS NOT NULL",
			"idx_entries_raw_z_len",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.readDB.Query("EXPLAIN QUERY PLAN " + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan strings.Builder
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err == nil {
					plan.WriteString(detail + "\n")
				}
			}
			got := plan.String()
			if !strings.Contains(got, tc.wantIndex) {
				t.Errorf("%s does not use %s — it walks the table and pulls "+
					"every payload's overflow pages with it:\n%s", tc.query, tc.wantIndex, got)
			}
			if strings.Contains(got, "SCAN messages\n") || strings.Contains(got, "SCAN entries\n") {
				t.Errorf("%s scans the table:\n%s", tc.query, got)
			}
		})
	}
}
