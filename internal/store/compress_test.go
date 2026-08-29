// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			if !f.BackfillDone || f.BackfillSaved <= 0 || f.PlainBytes >= int64(plainBytes) {
				t.Fatalf("family status: %+v (plain bytes before %d)", f, plainBytes)
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
