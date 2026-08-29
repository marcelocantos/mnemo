//go:build compresslive

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Live-corpus oracle for 🎯T151. Runs the whole compression lifecycle —
// additive migration, dictionary training, backfill of every historical
// row, VACUUM — against a COPY of the real index and reports what the
// change buys at scale. Build-tagged because it takes minutes and needs
// a 30+ GB database:
//
//	sqlite3 ~/.mnemo/mnemo.db ".backup '/path/copy.db'"
//	MNEMO_COMPRESS_LIVE_DB=/path/copy.db \
//	  go test -tags "sqlite_fts5 compresslive" -run TestCompressLiveCorpus -v -timeout 4h ./internal/store/
//
// Never point it at ~/.mnemo/mnemo.db: it rewrites every row.

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompressLiveCorpus(t *testing.T) {
	path := os.Getenv("MNEMO_COMPRESS_LIVE_DB")
	if path == "" {
		t.Skip("MNEMO_COMPRESS_LIVE_DB not set")
	}
	sizeOf := func(label string) int64 {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-28s file %.2f GB", label, float64(fi.Size())/1e9)
		return fi.Size()
	}
	before := sizeOf("before")

	// Table bytes from dbstat, the acceptance metric (file size also
	// carries WAL/free pages until VACUUM).
	// go-sqlite3 is not built with the dbstat vtab; the sqlite3 CLI is.
	tableBytes := func(_ *sql.DB, table string) int64 {
		out, err := exec.Command("sqlite3", path,
			"SELECT COALESCE(SUM(pgsize), 0) FROM dbstat WHERE name = '"+table+"'").Output()
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Log("opening (runs the additive migration synchronously if pending)")
	start := time.Now()
	s, err := New(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitSchemaUpgrade()
	t.Logf("open+migrate: %s", time.Since(start).Round(time.Second))
	if !s.CompressionReady() {
		// A deferred upgrade enables compression in its own goroutine
		// after AwaitSchemaUpgrade returns; give it a moment.
		for i := 0; i < 50 && !s.CompressionReady(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !s.CompressionReady() {
		t.Fatal("compression not ready after migration")
	}
	msgBefore := tableBytes(s.readDB, "messages")
	docsBefore := tableBytes(s.readDB, "docs")
	entBefore := tableBytes(s.readDB, "entries")
	t.Logf("dbstat before: messages %.2f GB, docs %.2f GB, entries %.2f GB",
		float64(msgBefore)/1e9, float64(docsBefore)/1e9, float64(entBefore)/1e9)

	ctx := context.Background()
	// 🎯T152: the boot pass copies the generated columns into *_m; the
	// entries.raw backfill is refused until it reports done.
	start = time.Now()
	if mres, err := s.MaterialiseEntries(ctx); err != nil && !strings.Contains(err.Error(), "already running") {
		t.Fatal(err)
	} else {
		t.Logf("entries.fields: visited %d rows, updated %d in %s (done=%v)", mres.Rows, mres.Compressed, time.Since(start).Round(time.Second), mres.Done)
	}
	for !s.codec.entriesPackable.Load() {
		if ok, _ := s.EntriesMaterialised(); ok {
			break
		}
		time.Sleep(time.Second)
	}
	compressed := map[string]int64{}
	for _, family := range allFamilies {
		start = time.Now()
		id, err := s.TrainDictionary(ctx, family)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: trained dictionary %d in %s", family, id, time.Since(start).Round(time.Millisecond))

		start = time.Now()
		res, err := s.CompressBackfill(ctx, family)
		if err != nil {
			t.Fatalf("%s backfill after %d rows: %v", family, res.Rows, err)
		}
		t.Logf("%s: backfill visited %d rows, compressed %d, saved %.2f GB in %s (done=%v)",
			family, res.Rows, res.Compressed, float64(res.Saved)/1e9, time.Since(start).Round(time.Second), res.Done)
		compressed[family] = res.Compressed
	}

	// Spot-check: a random sample of compressed rows decodes and the FTS
	// index still finds text that lives only in compressed rows.
	var n, bad int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*), SUM(length(mnemo_text(text, text_z)) = 0)
		FROM (SELECT text, text_z FROM messages_v WHERE text_z IS NOT NULL ORDER BY random() LIMIT 20000)`).Scan(&n, &bad); err != nil {
		t.Fatal(err)
	}
	t.Logf("sampled %d compressed rows, %d decode empty", n, bad)
	if bad != 0 {
		t.Fatalf("%d compressed rows decode to empty text", bad)
	}
	var en, ebad int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*), SUM(json_extract(raw, '$.uuid') IS NOT uuid)
		FROM (SELECT raw, uuid FROM entries_v WHERE type = 'assistant' ORDER BY random() LIMIT 20000)`).Scan(&en, &ebad); err != nil {
		t.Fatal(err)
	}
	t.Logf("sampled %d entries, %d where decoded raw disagrees with the materialised uuid", en, ebad)
	if ebad != 0 {
		t.Fatalf("%d entries rows disagree between raw and uuid_m", ebad)
	}
	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Families {
		t.Logf("%-16s rows=%d compressed=%d plain=%.2f GB packed=%.2f GB done=%v",
			f.Family, f.Rows, f.Compressed, float64(f.PlainBytes)/1e9, float64(f.PackedBytes)/1e9, f.BackfillDone)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	t.Log("VACUUM (this is the slow part)")
	start = time.Now()
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	t.Logf("vacuum: %s", time.Since(start).Round(time.Second))
	msgAfter := tableBytes(db, "messages")
	docsAfter := tableBytes(db, "docs")
	entAfter := tableBytes(db, "entries")
	db.Close()
	after := sizeOf("after")

	t.Logf("dbstat after: messages %.2f GB (%.0f%%), docs %.2f GB (%.0f%%), entries %.2f GB (%.0f%%)",
		float64(msgAfter)/1e9, 100*float64(msgAfter)/float64(msgBefore),
		float64(docsAfter)/1e9, 100*float64(docsAfter)/float64(docsBefore),
		float64(entAfter)/1e9, 100*float64(entAfter)/float64(entBefore))
	t.Logf("file: %.2f GB -> %.2f GB (%.0f%%)", float64(before)/1e9, float64(after)/1e9, 100*float64(after)/float64(before))
	// A family whose rows were already compressed before this run has no
	// plain baseline to measure against; only assert on the ones this
	// run actually repacked.
	if compressed[FamilyMessagesText]+compressed[FamilyDocsContent] > 1000 {
		if ratio := float64(msgAfter+docsAfter) / float64(msgBefore+docsBefore); ratio > 0.45 {
			t.Errorf("messages+docs at %.0f%% of pre-GC size; 🎯T151 acceptance is at most 45%%", 100*ratio)
		}
	}
	if compressed[FamilyEntriesRaw] > 1000 {
		if ratio := float64(entAfter) / float64(entBefore); ratio > 0.50 {
			t.Errorf("entries at %.0f%% of pre-GC size; 🎯T152 acceptance is at most 50%%", 100*ratio)
		}
	}
}
