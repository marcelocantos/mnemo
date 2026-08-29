// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// longText builds a row comfortably over compressMinBytes with enough
// repetition to compress, tagged so rows are distinguishable.
func longText(tag string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s: the quick brown fox jumps over the lazy dog, line %d of the transcript.\n", tag, i)
	}
	return b.String()
}

func TestCompressedMessagesRoundTripThroughEveryReader(t *testing.T) {
	projectDir := t.TempDir()
	long := longText("alpha", 40)
	short := "ok"
	writeJSONL(t, projectDir, "proj", "sess-z1", []map[string]any{
		metaMsg("user", long, "2026-04-01T10:00:00Z", "/Users/dev/work/github.com/acme/webapp", "master"),
		msg("assistant", short, "2026-04-01T10:00:05Z"),
		msg("user", longText("beta", 30), "2026-04-01T10:01:00Z"),
	})
	s := newTestStore(t, projectDir)
	if !s.CompressionReady() {
		t.Fatal("codec not ready on a fresh schema")
	}
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Storage shape: the long rows are packed, the short one is plain.
	var packed, plain int
	if err := s.readDB.QueryRow(`
		SELECT SUM(text_z IS NOT NULL AND text = ''), SUM(text_z IS NULL AND text != '')
		FROM messages WHERE session_id = 'sess-z1'`).Scan(&packed, &plain); err != nil {
		t.Fatal(err)
	}
	if packed != 2 || plain != 1 {
		t.Fatalf("packed=%d plain=%d, want 2 packed / 1 plain", packed, plain)
	}

	// The view and the SQL function decode byte-identically.
	var viaView, viaFunc string
	if err := s.readDB.QueryRow(`SELECT text FROM messages_v WHERE session_id = 'sess-z1' AND role = 'user' ORDER BY id LIMIT 1`).Scan(&viaView); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`SELECT mnemo_text(text, text_z) FROM messages WHERE session_id = 'sess-z1' AND role = 'user' ORDER BY id LIMIT 1`).Scan(&viaFunc); err != nil {
		t.Fatal(err)
	}
	if viaView != long || viaFunc != long {
		t.Fatalf("decoded text differs from original (view ok=%v, func ok=%v)", viaView == long, viaFunc == long)
	}

	// FTS indexed the decoded text (the trigger goes through mnemo_text).
	hits, err := s.Search("lazy dog", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("FTS did not index compressed text")
	}
	// Search excerpts may be truncated; the decoded prefix is what matters.
	if !strings.HasPrefix(hits[0].Text, "alpha: the quick") && !strings.HasPrefix(hits[0].Text, "beta: the quick") {
		t.Fatalf("search hit text not decoded: %q", hits[0].Text[:min(len(hits[0].Text), 60)])
	}

	// ReadSession is the bulk reader agents use.
	msgs, err := s.ReadSession("sess-z1", "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("ReadSession returned %d messages, want 3", len(msgs))
	}
	if msgs[0].Text != long || msgs[1].Text != short {
		t.Fatalf("ReadSession text mismatch: %q / %q", msgs[0].Text[:min(len(msgs[0].Text), 40)], msgs[1].Text)
	}

	// The read pool (read-only driver) has the function too.
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM messages_v WHERE text LIKE 'alpha:%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("messages_v LIKE over decoded text: got %d rows, want 1", n)
	}
}

func TestCompressedDocsRoundTripAndFTSDelete(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "DESIGN.md")
	content := "# Design\n\n" + longText("doc", 50)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if indexed, _ := s.ingestDocFile(path, "acme/webapp", 0); indexed != 1 {
		t.Fatalf("doc not indexed")
	}
	var plain string
	var z []byte
	if err := s.readDB.QueryRow(`SELECT content, content_z FROM docs WHERE file_path = ?`, path).Scan(&plain, &z); err != nil {
		t.Fatal(err)
	}
	if plain != "" || len(z) == 0 || len(z) >= len(content) {
		t.Fatalf("doc not stored compressed: plain=%d z=%d content=%d", len(plain), len(z), len(content))
	}
	docs, err := s.SearchDocs("transcript", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Content != content {
		t.Fatalf("SearchDocs did not return decoded content (%d hits)", len(docs))
	}

	// Rewrite the doc: the docs_au trigger must remove the old tokens via
	// mnemo_text(old.content, old.content_z), or stale hits survive.
	if err := os.WriteFile(path, []byte("# Design\n\nreplaced entirely, nothing in common."), 0o644); err != nil {
		t.Fatal(err)
	}
	if indexed, _ := s.ingestDocFile(path, "acme/webapp", 0); indexed != 1 {
		t.Fatalf("doc rewrite not indexed")
	}
	docs, err = s.SearchDocs("transcript", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatalf("stale FTS tokens survived the rewrite: %d hits", len(docs))
	}
	var fresh int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM docs_fts WHERE docs_fts MATCH 'replaced'`).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if fresh != 1 {
		t.Fatalf("rewritten doc not searchable: %d", fresh)
	}
}

func TestDictionaryTrainingKeepsOldRowsDecodable(t *testing.T) {
	projectDir := t.TempDir()
	var entries []map[string]any
	for i := 0; i < 50; i++ {
		entries = append(entries, msg("assistant", longText(fmt.Sprintf("row%d", i), 5), fmt.Sprintf("2026-04-01T10:%02d:00Z", i)))
	}
	writeJSONL(t, projectDir, "proj", "sess-d1", entries)
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if got := s.codec.ActiveDict(FamilyMessagesText); got != 0 {
		t.Fatalf("fresh store has an active dictionary %d", got)
	}
	ctx := context.Background()

	id1, err := s.TrainDictionary(ctx, FamilyMessagesText)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 || s.codec.ActiveDict(FamilyMessagesText) != id1 {
		t.Fatalf("dictionary %d not active", id1)
	}
	// Rows written under the new dictionary carry its id in the frame.
	writeJSONL(t, projectDir, "proj", "sess-d2", []map[string]any{
		msg("assistant", longText("under-dict-1", 8), "2026-04-02T10:00:00Z"),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var z []byte
	if err := s.readDB.QueryRow(`SELECT text_z FROM messages WHERE session_id = 'sess-d2'`).Scan(&z); err != nil {
		t.Fatal(err)
	}
	if got := frameDictID(z); got != id1 {
		t.Fatalf("frame dictionary id %d, want %d", got, id1)
	}

	// Retrain: a second lineage; the first row still decodes.
	id2, err := s.TrainDictionary(ctx, FamilyMessagesText)
	if err != nil {
		t.Fatal(err)
	}
	if id2 == id1 {
		t.Fatal("retrain reused the dictionary id")
	}
	var back string
	if err := s.readDB.QueryRow(`SELECT text FROM messages_v WHERE session_id = 'sess-d2'`).Scan(&back); err != nil {
		t.Fatal(err)
	}
	if back != longText("under-dict-1", 8) {
		t.Fatal("row written under the first dictionary no longer decodes")
	}

	// A fresh process (new registry state) loads both from the table.
	s2, err := New(s.dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.codec.ActiveDict(FamilyMessagesText) != id2 {
		t.Fatalf("reopened store active dict %d, want %d", s2.codec.ActiveDict(FamilyMessagesText), id2)
	}
	st, err := s2.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Dicts) != 2 || !st.Ready {
		t.Fatalf("status: %+v", st)
	}
}

func TestCompressBackfillIsResumableAndVerified(t *testing.T) {
	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	// Seed legacy-shaped rows directly: plain text, NULL text_z, as a
	// pre-🎯T151 binary wrote them.
	const rows = 5 * backfillBatchRows / 2
	tx, err := s.writeDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(messageInsertLegacySQL)
	if err != nil {
		t.Fatal(err)
	}
	var plainBytes int
	for i := 0; i < rows; i++ {
		text := longText(fmt.Sprintf("legacy%d", i), 3)
		if i%7 == 0 {
			text = "short" // stays plain: under compressMinBytes
		}
		plainBytes += len(text)
		if _, err := stmt.Exec(nil, "sess-legacy", "proj", "assistant", text, "2026-04-01T10:00:00Z", "assistant", 0,
			"text", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// First run is cancelled after the first batch commits.
	ctx, cancel := context.WithCancel(context.Background())
	var firstBatchSeen bool
	go func() {
		// Cancel as soon as the cursor has moved once.
		for {
			var next int64
			s.readDB.QueryRow(`SELECT next_id FROM compression_gc WHERE family = ?`, FamilyMessagesText).Scan(&next)
			if next > 0 {
				firstBatchSeen = true
				cancel()
				return
			}
		}
	}()
	res, err := s.CompressBackfill(ctx, FamilyMessagesText)
	if err == nil && !res.Done {
		t.Fatal("first run neither finished nor was cancelled")
	}
	if !firstBatchSeen && !res.Done {
		t.Fatal("cancelled before any batch committed")
	}

	// Second run resumes from the cursor and finishes.
	res2, err := s.CompressBackfill(context.Background(), FamilyMessagesText)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Done {
		t.Fatal("second run did not finish")
	}
	if res.Compressed+res2.Compressed != int64(rows-(rows+6)/7) {
		t.Fatalf("compressed %d rows across runs, want %d", res.Compressed+res2.Compressed, rows-(rows+6)/7)
	}

	// Every row still reads back, and the long ones are now packed.
	var packed, plainLeft int
	var decodedOK int
	if err := s.readDB.QueryRow(`
		SELECT SUM(text_z IS NOT NULL), SUM(text_z IS NULL),
		       SUM(mnemo_text(text, text_z) LIKE 'legacy%' OR mnemo_text(text, text_z) = 'short')
		FROM messages WHERE session_id = 'sess-legacy'`).Scan(&packed, &plainLeft, &decodedOK); err != nil {
		t.Fatal(err)
	}
	if decodedOK != rows {
		t.Fatalf("%d of %d rows decode to their original text", decodedOK, rows)
	}
	if plainLeft != (rows+6)/7 {
		t.Fatalf("%d rows left plain, want %d short rows", plainLeft, (rows+6)/7)
	}

	// Idempotent: a third run visits nothing.
	res3, err := s.CompressBackfill(context.Background(), FamilyMessagesText)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Rows != 0 || !res3.Done {
		t.Fatalf("third run: %+v", res3)
	}
	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText {
			if !f.BackfillDone || f.PlainBytes >= int64(plainBytes) {
				t.Fatalf("family status: %+v (plain bytes before %d)", f, plainBytes)
			}
			// The persisted total is the sum of per-batch deltas — not the
			// run's cumulative figure re-added every batch.
			if want := res.Saved + res2.Saved; f.BackfillSaved != want {
				t.Fatalf("persisted saved_bytes %d, want %d", f.BackfillSaved, want)
			}
		}
	}
}

func TestPackSkipsShortAndIncompressibleRows(t *testing.T) {
	c := newTextCodec()
	c.ready.Store(true)
	if p, z := c.pack(FamilyMessagesText, "tiny"); p != "tiny" || z != nil {
		t.Fatal("short row should stay plain")
	}
	// High-entropy bytes do not compress: keep plain rather than grow.
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "%08x", uint32(i*2654435761))
	}
	noise := b.String()
	p, z := c.pack(FamilyMessagesText, noise)
	if z != nil && len(z) >= len(noise) {
		t.Fatal("stored a frame larger than the plaintext")
	}
	if z == nil && p != noise {
		t.Fatal("plain fallback altered the text")
	}
	long := longText("x", 20)
	p, z = c.pack(FamilyMessagesText, long)
	if p != "" || len(z) == 0 || len(z) >= len(long) {
		t.Fatalf("long row not packed: plain=%d z=%d", len(p), len(z))
	}
	back, err := dictRegistry.decode(z)
	if err != nil || string(back) != long {
		t.Fatalf("round trip: %v", err)
	}
	c.ready.Store(false)
	if p, z := c.pack(FamilyMessagesText, long); p != long || z != nil {
		t.Fatal("codec not ready must write plain")
	}
}

func TestDocsWriterUsesLegacyShapeUntilCodecReady(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	path := filepath.Join(t.TempDir(), "NOTES.md")
	content := "# Notes\n\n" + longText("legacy", 30)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-migration window (🎯T114.1): the codec is not
	// ready, so the writer must not reference content_z at all.
	s.codec.ready.Store(false)
	if indexed, _ := s.ingestDocFile(path, "acme/webapp", 0); indexed != 1 {
		t.Fatal("doc not indexed with the codec not ready")
	}
	var plain string
	var z []byte
	if err := s.readDB.QueryRow(`SELECT content, content_z FROM docs WHERE file_path = ?`, path).Scan(&plain, &z); err != nil {
		t.Fatal(err)
	}
	if plain != content || z != nil {
		t.Fatalf("legacy shape expected: plain=%d z=%d", len(plain), len(z))
	}
}

// entryFixture builds an assistant entry with usage, the shape the hot
// entries fields are extracted from.
func entryFixture(uuid, model, text, ts string, in, out int) map[string]any {
	e := msg("assistant", text, ts)
	e["uuid"] = uuid
	e["agentId"] = "agent-1"
	e["version"] = "2.0.0"
	e["slug"] = "slug-x"
	e["isSidechain"] = true
	m := e["message"].(map[string]any)
	m["id"] = "msg-" + uuid
	m["model"] = model
	m["stop_reason"] = "end_turn"
	m["usage"] = map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": 7, "cache_creation_input_tokens": 3}
	return e
}

func TestEntriesRawCompressedAndFieldsMaterialised(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "proj", "sess-e1", []map[string]any{
		entryFixture("u-1", "claude-fable-5", longText("e1", 20), "2026-04-01T10:00:00Z", 100, 20),
		entryFixture("u-2", "claude-fable-5", longText("e2", 20), "2026-04-01T10:01:00Z", 200, 40),
	})
	s := newTestStore(t, projectDir)
	// A fresh store materialises at boot (nothing to do) — wait for it so
	// the writer packs raw from the first ingest.
	for i := 0; i < 100 && !s.codec.entriesPackable.Load(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !s.codec.entriesPackable.Load() {
		t.Fatal("entries not packable after boot materialisation")
	}
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Storage shape: raw NULL, raw_z set, generated columns NULL, *_m set.
	var nullRaw, packed, genNull, mSet int
	if err := s.readDB.QueryRow(`
		SELECT SUM(raw IS NULL), SUM(raw_z IS NOT NULL), SUM(model IS NULL), SUM(model_m IS NOT NULL)
		FROM entries WHERE session_id = 'sess-e1'`).Scan(&nullRaw, &packed, &genNull, &mSet); err != nil {
		t.Fatal(err)
	}
	if nullRaw != 2 || packed != 2 || genNull != 2 || mSet != 2 {
		t.Fatalf("shape: raw NULL=%d packed=%d gen NULL=%d m set=%d", nullRaw, packed, genNull, mSet)
	}

	// The view serves the fields and the decoded JSON.
	var uuid, model string
	var in, out, cr, cc, side int
	var rawUUID string
	if err := s.readDB.QueryRow(`
		SELECT uuid, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, is_sidechain,
		       json_extract(raw, '$.uuid')
		FROM entries_v WHERE session_id = 'sess-e1' ORDER BY id LIMIT 1`).Scan(&uuid, &model, &in, &out, &cr, &cc, &side, &rawUUID); err != nil {
		t.Fatal(err)
	}
	if uuid != "u-1" || model != "claude-fable-5" || in != 100 || out != 20 || cr != 7 || cc != 3 || side != 1 || rawUUID != "u-1" {
		t.Fatalf("entries_v: uuid=%s model=%s in=%d out=%d cr=%d cc=%d side=%d rawUUID=%s", uuid, model, in, out, cr, cc, side, rawUUID)
	}

	// Usage analytics ride on entries_v's materialised columns.
	usage, err := s.Usage(UsageParams{GroupBy: "model", Since: "2026-01-01T00:00:00Z", Until: "2026-12-31T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.Total.InputTokens != 300 {
		t.Fatalf("usage input tokens %d, want 300 (%+v)", usage.Total.InputTokens, usage.Rows)
	}

	// INSERT OR IGNORE still deduplicates on (session_id, uuid) after
	// compression: re-ingesting the same file adds no rows.
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM entries WHERE session_id = 'sess-e1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("re-ingest produced %d rows, want 2", n)
	}
}

func TestEntriesLegacyRowsMaterialisedThenCompressed(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	// Seed pre-🎯T152 rows: raw JSONB, no *_m, as an older binary wrote them.
	tx, err := s.writeDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(entryInsertLegacySQL)
	if err != nil {
		t.Fatal(err)
	}
	const rows = 2*backfillBatchRows + 7
	for i := 0; i < rows; i++ {
		e := entryFixture(fmt.Sprintf("u-%d", i), "claude-opus-5", longText("legacy", 3), "2026-04-01T10:00:00Z", i, 1)
		line, _ := json.Marshal(e)
		if _, err := stmt.Exec("sess-legacy", "proj", "assistant", "2026-04-01T10:00:00Z", string(line)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Reset the boot pass so this store's rows are visible to it.
	if _, err := s.writeDB.Exec(`DELETE FROM compression_gc WHERE family = ?`, entriesFieldsFamily); err != nil {
		t.Fatal(err)
	}
	s.codec.entriesPackable.Store(false)

	// Compression is refused until the fields are materialised.
	if _, err := s.CompressBackfill(context.Background(), FamilyEntriesRaw); err == nil {
		t.Fatal("entries.raw backfill ran before materialisation")
	}
	res, err := s.MaterialiseEntries(context.Background())
	if err != nil || !res.Done {
		t.Fatalf("materialise: %+v %v", res, err)
	}
	var mSet int
	if err := s.readDB.QueryRow(`SELECT SUM(uuid_m IS NOT NULL AND input_tokens_m IS NOT NULL) FROM entries WHERE session_id = 'sess-legacy'`).Scan(&mSet); err != nil {
		t.Fatal(err)
	}
	if mSet != rows {
		t.Fatalf("%d of %d rows materialised", mSet, rows)
	}

	res, err = s.CompressBackfill(context.Background(), FamilyEntriesRaw)
	if err != nil || !res.Done || res.Compressed != rows {
		t.Fatalf("compress: %+v %v", res, err)
	}
	var sumIn int64
	var nullRaw, decodedOK int
	if err := s.readDB.QueryRow(`
		SELECT SUM(input_tokens), SUM(raw IS NULL), SUM(json_extract(raw, '$.message.model') = 'claude-opus-5')
		FROM entries_v WHERE session_id = 'sess-legacy'`).Scan(&sumIn, &nullRaw, &decodedOK); err != nil {
		t.Fatal(err)
	}
	if want := int64(rows * (rows - 1) / 2); sumIn != want || decodedOK != rows {
		t.Fatalf("after compression: sum(input)=%d want %d, decoded=%d/%d", sumIn, want, decodedOK, rows)
	}
	// The base table's raw is NULL for every row, so the view — not the
	// generated columns — is what made the previous query correct.
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM entries WHERE session_id = 'sess-legacy' AND raw IS NULL`).Scan(&nullRaw); err != nil {
		t.Fatal(err)
	}
	if nullRaw != rows {
		t.Fatalf("%d rows still hold plain raw", rows-nullRaw)
	}
}
