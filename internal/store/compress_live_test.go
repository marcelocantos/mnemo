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
	t.Logf("dbstat before: messages %.2f GB, docs %.2f GB", float64(msgBefore)/1e9, float64(docsBefore)/1e9)

	ctx := context.Background()
	for _, family := range []string{FamilyMessagesText, FamilyDocsContent} {
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
	}

	// Spot-check: a random sample of compressed rows decodes and the FTS
	// index still finds text that lives only in compressed rows.
	var n, bad int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*), SUM(length(mnemo_text(text, text_z)) = 0)
		FROM (SELECT text, text_z FROM messages WHERE text_z IS NOT NULL ORDER BY random() LIMIT 20000)`).Scan(&n, &bad); err != nil {
		t.Fatal(err)
	}
	t.Logf("sampled %d compressed rows, %d decode empty", n, bad)
	if bad != 0 {
		t.Fatalf("%d compressed rows decode to empty text", bad)
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
	db.Close()
	after := sizeOf("after")

	t.Logf("dbstat after: messages %.2f GB (%.0f%%), docs %.2f GB (%.0f%%)",
		float64(msgAfter)/1e9, 100*float64(msgAfter)/float64(msgBefore),
		float64(docsAfter)/1e9, 100*float64(docsAfter)/float64(docsBefore))
	t.Logf("file: %.2f GB -> %.2f GB (%.0f%%)", float64(before)/1e9, float64(after)/1e9, 100*float64(after)/float64(before))
	if ratio := float64(msgAfter+docsAfter) / float64(msgBefore+docsBefore); ratio > 0.45 {
		t.Errorf("messages+docs at %.0f%% of pre-GC size; acceptance is at most 45%%", 100*ratio)
	}
}
