// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelaxQueryStemmedSkipsPrefix(t *testing.T) {
	if got := relaxQueryStemmed(`"the cat"`); got != `"the cat"` {
		t.Fatalf("stemmed phrase = %q, want the phrase without a trailing star", got)
	}
	if got := relaxQuery(`"the cat"`); got != `"the cat"*` {
		t.Fatalf("legacy phrase = %q, want the prefix stopgap", got)
	}
	if got := relaxQueryStemmed("alpha beta"); got != "alpha OR beta" {
		t.Fatalf("stemmed word list = %q", got)
	}
}

func TestStemDualWrite(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	insertPlain(t, s, "sess-dual-aaa", "workbench", "a zqxdualwrite note")
	if n := ftsCount(t, s, "messages_fts", "zqxdualwrite"); n != 1 {
		t.Fatalf("legacy index hits = %d, want 1", n)
	}
	if n := ftsCount(t, s, "messages_fts_stem", "zqxdualwrite"); n != 1 {
		t.Fatalf("stem index hits = %d, want 1", n)
	}
}

func TestStemSplitAgreesWithGroundTruth(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	// Four rows. The marker sits between the second and the third so
	// each side of the split owns two, and the same text occurs on
	// both sides: a plural that both indexes must find, and a prefix
	// over-match that only the legacy side should find.
	ids := []int64{
		insertPlain(t, s, "sess-split-aaa", "workbench", "the catalog of items"),
		insertPlain(t, s, "sess-split-aaa", "workbench", "alpha zqxworkers arrived"),
		insertPlain(t, s, "sess-split-bbb", "workbench", "the catalog of items"),
		insertPlain(t, s, "sess-split-bbb", "workbench", "alpha zqxworkers arrived"),
	}
	marker := ids[2]
	armStemSplit(t, s, marker)

	// Prefix over-match: "the cat"* matches catalog on unicode61.
	// Porter does not stem catalog to cat, and the stem query does
	// not add the star.
	legacyQ := relaxQuery(`"the cat"`)
	stemQ := relaxQueryStemmed(`"the cat"`)
	got := messageIDs(t, s, `"the cat"`, 20)
	want := groundIDs(t, s, legacyQ, stemQ, marker)
	if !sameIDs(got, want) {
		t.Fatalf("phrase split ids = %v, ground truth %v", got, want)
	}
	if !got[ids[0]] || got[ids[2]] {
		t.Fatalf("prefix over-match leaked across the marker: got %v, below %d above %d", got, ids[0], ids[2])
	}

	legacyQ = relaxQuery(`"alpha zqxworker"`)
	stemQ = relaxQueryStemmed(`"alpha zqxworker"`)
	got = messageIDs(t, s, `"alpha zqxworker"`, 20)
	want = groundIDs(t, s, legacyQ, stemQ, marker)
	if !sameIDs(got, want) {
		t.Fatalf("plural split ids = %v, ground truth %v", got, want)
	}
	if !got[ids[1]] || !got[ids[3]] {
		t.Fatalf("plural missing on one side of the marker: got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("plural ids = %v, want exactly the two rows and no duplicates", got)
	}

	// Marker at the high end: every row is below it, answered by the
	// legacy index. Marker at the low end: every row is on the stem side.
	for _, m := range []int64{ids[0], ids[3] + 1} {
		armStemSplit(t, s, m)
		legacyQ = relaxQuery("zqxworkers")
		stemQ = relaxQueryStemmed("zqxworkers")
		got = messageIDs(t, s, "zqxworkers", 20)
		want = groundIDs(t, s, legacyQ, stemQ, m)
		if !sameIDs(got, want) {
			t.Fatalf("marker %d ids = %v, ground truth %v", m, got, want)
		}
	}

	// The row above the marker must survive a limit smaller than the
	// number of stronger matches below it. That fails if LIMIT is
	// applied before the rowid predicate. The stem index holds every
	// row (dual-write); the predicate is what drops the stronger ones.
	const token = "zqxstrong"
	for i := 0; i < 60; i++ {
		insertPlain(t, s, "sess-split-aaa", "workbench", token)
	}
	weak := insertPlain(t, s, "sess-split-bbb", "workbench", token+" "+strings.Repeat("padding word ", 40))
	side, err := s.matchSide("messages_fts_stem", token, ">=", weak, 10, hitFilter{}, false)
	if err != nil {
		t.Fatal(err)
	}
	seenWeak := false
	for _, h := range side {
		if h.rowid == weak {
			seenWeak = true
		}
		if h.rowid < weak {
			t.Fatalf("row %d is below the marker and was returned", h.rowid)
		}
	}
	if !seenWeak {
		t.Fatalf("row above the marker missing at limit 10 (%d hits); rowid predicate ran after LIMIT", len(side))
	}
}

func TestStemBackfillResumes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	defer resetStemHooks()
	var ids []int64
	for i := 0; i < 6; i++ {
		ids = append(ids, insertPlain(t, s, "sess-resume-aaa", "workbench", fmt.Sprintf("zqxresume note %d", i)))
	}
	mustExec(t, s, `INSERT INTO messages_fts_stem(messages_fts_stem) VALUES('delete-all')`)
	var maxID int64
	if err := s.readDB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE fts_stem_progress SET status = 'running', marker_rowid = ?, initial_marker = ?, rows_done = 0 WHERE corpus = 'message'`, maxID+1, maxID+1)

	stemNoYield = true
	stemSkipCalibrate = true
	stemBatchOverride = 2
	stopped := false
	stemAfterBatch = func(corpus string, marker, rowsDone int64) error {
		if corpus != "message" {
			return nil
		}
		stopped = true
		if rowsDone == 0 || marker >= maxID+1 {
			t.Errorf("batch did not advance: marker %d rows %d", marker, rowsDone)
		}
		return errStemPause
	}
	if err := s.stemBackfill(context.Background()); err != errStemPause {
		t.Fatalf("backfill err = %v, want pause", err)
	}
	if !stopped {
		t.Fatal("backfill finished a corpus without pausing")
	}
	var status string
	var marker, rows int64
	if err := s.readDB.QueryRow(`SELECT status, marker_rowid, rows_done FROM fts_stem_progress WHERE corpus = 'message'`).Scan(&status, &marker, &rows); err != nil {
		t.Fatal(err)
	}
	if status != "running" || rows == 0 || marker >= maxID+1 {
		t.Fatalf("progress after interrupt: status %s marker %d rows %d", status, marker, rows)
	}
	if n := ftsCount(t, s, "messages_fts_stem", "zqxresume"); n == 0 || n >= 6 {
		t.Fatalf("stem rows after one batch = %d, want a partial index", n)
	}

	stemAfterBatch = nil
	stemBatchOverride = 2
	if err := s.stemBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`SELECT status FROM fts_stem_progress WHERE corpus = 'message'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("status after resume = %s", status)
	}
	if n := ftsCount(t, s, "messages_fts_stem", "zqxresume"); n != 6 {
		t.Fatalf("stem rows after resume = %d, want 6", n)
	}
	for _, id := range ids {
		_ = id
	}
}

func TestStemCompletionUsesOnlyTheNewIndex(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	id := insertPlain(t, s, "sess-done-aaa", "workbench", "zqxdone shared token")
	var status string
	if err := s.readDB.QueryRow(`SELECT status FROM fts_stem_progress WHERE corpus = 'message'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("fresh store status = %s, want complete", status)
	}
	// Remove it from the legacy index only. Search must still find it.
	mustExec(t, s, `INSERT INTO messages_fts(messages_fts, rowid, text, role, project, session_id) `+
		`SELECT 'delete', id, mnemo_text(text, text_z), role, project, session_id FROM messages WHERE id = ?`, id)
	got, err := s.Search("zqxdone", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMessage(got, id) {
		t.Fatal("completed migration did not answer from the stem index")
	}
	// Remove it from the stem index and put it back only in the legacy
	// one. Search must not find it.
	mustExec(t, s, `INSERT INTO messages_fts_stem(messages_fts_stem, rowid, text, role, project, session_id) `+
		`SELECT 'delete', id, mnemo_text(text, text_z), role, project, session_id `+
		`FROM messages WHERE id = ? AND EXISTS (SELECT 1 FROM messages_fts_stem_docsize ds WHERE ds.id = messages.id)`, id)
	mustExec(t, s, `INSERT INTO messages_fts(rowid, text, role, project, session_id) `+
		`SELECT id, mnemo_text(text, text_z), role, project, session_id FROM messages WHERE id = ?`, id)
	got, err = s.Search("zqxdone", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if hasMessage(got, id) {
		t.Fatal("completed migration still answered from the legacy index")
	}
}

func TestStemMigrationFromPriorSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mnemo.db")
	s0, err := New(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	s0.AwaitStartup()
	if err := s0.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open(writerDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT type, name FROM sqlite_master WHERE name LIKE '%stem%'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers, tables []string
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatal(err)
		}
		if typ == "trigger" {
			triggers = append(triggers, name)
			continue
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) == 0 {
		t.Fatal("prior-schema setup found no stem objects to drop")
	}
	for _, name := range triggers {
		if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + quoteIdent(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range tables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + quoteIdent(name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('sess-prior-aaa', 'workbench', 'user', 'zqxpriorschema note', '2026-01-01T00:00:00Z', 'user', 0, 'text')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AwaitStartup()
	var status string
	if err := s.readDB.QueryRow(`SELECT status FROM fts_stem_progress WHERE corpus = 'message'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("status after upgrade = %s, want complete", status)
	}
	if n := ftsCount(t, s, "messages_fts_stem", "zqxpriorschema"); n != 1 {
		t.Fatalf("backfill indexed %d stem rows, want 1", n)
	}
	got, err := s.Search("zqxpriorschema", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !strings.Contains(got[0].Text, "zqxpriorschema") {
		t.Fatalf("search after upgrade = %+v", got)
	}
}

func TestStemBackfillThroughput(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	defer resetStemHooks()
	const n = 800
	body := strings.Repeat("alpha beta gamma delta ", 60) // ~1.3 KiB
	for i := 0; i < n; i++ {
		insertPlain(t, s, "sess-bench-aaa", "workbench", fmt.Sprintf("zqxbench %d %s", i, body))
	}
	mustExec(t, s, `INSERT INTO messages_fts_stem(messages_fts_stem) VALUES('delete-all')`)
	var maxID int64
	if err := s.readDB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE fts_stem_progress SET status = 'running', marker_rowid = ?, initial_marker = ?, rows_done = 0 WHERE corpus = 'message'`, maxID+1, maxID+1)
	s.checkpointPassive("stem-bench-before")
	before := usedDBBytes(t, s)

	stemNoYield = true
	stemSkipCalibrate = true
	start := time.Now()
	if err := s.stemBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	s.checkpointPassive("stem-bench-after")
	after := usedDBBytes(t, s)
	if nGot := ftsCount(t, s, "messages_fts_stem", "zqxbench"); nGot != n {
		t.Fatalf("indexed %d of %d", nGot, n)
	}
	rate := float64(n) / elapsed.Seconds()
	overhead := after - before
	perRow := float64(overhead) / float64(n)
	const storeRows = 4.95e6
	fullDuty := time.Duration(float64(time.Second) * storeRows / rate)
	halfDuty := fullDuty * 2
	t.Logf("STEM_BENCH rows=%d elapsed=%s rows_per_s=%.1f bytes_per_row=%.0f disk_bytes=%d full_duty_5M=%s half_duty_5M=%s text_bytes=%d",
		n, elapsed.Round(time.Millisecond), rate, perRow, overhead, fullDuty.Round(time.Second), halfDuty.Round(time.Second), len(body))
	if rate <= 0 || overhead <= 0 {
		t.Fatalf("benchmark did not measure a rate (%.1f rows/s) and a disk delta (%d)", rate, overhead)
	}
}

func armStemSplit(t *testing.T, s *Store, marker int64) {
	t.Helper()
	mustExec(t, s, `INSERT INTO messages_fts_stem(messages_fts_stem) VALUES('delete-all')`)
	mustExec(t, s, `INSERT INTO messages_fts_stem(rowid, text, role, project, session_id) `+
		`SELECT id, mnemo_text(text, text_z), role, project, session_id `+
		`FROM messages WHERE id >= ? AND is_noise = 0`, marker)
	var maxID int64
	if err := s.readDB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE fts_stem_progress SET status = 'running', marker_rowid = ?, initial_marker = ? WHERE corpus = 'message'`, marker, maxID+1)
}

func ftsCount(t *testing.T, s *Store, table, term string) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM ` + table + ` WHERE ` + table + ` MATCH ?`
	if err := s.readDB.QueryRow(q, term).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func messageIDs(t *testing.T, s *Store, query string, limit int) map[int64]bool {
	t.Helper()
	hits, err := s.Search(query, limit, "all", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, h := range hits {
		if h.Role == "compaction" || h.Role == "vault" {
			continue
		}
		if out[int64(h.MessageID)] {
			t.Errorf("duplicate message id %d", h.MessageID)
		}
		out[int64(h.MessageID)] = true
	}
	return out
}

func groundIDs(t *testing.T, s *Store, legacyQ, stemQ string, marker int64) map[int64]bool {
	t.Helper()
	out := map[int64]bool{}
	collect := func(q string, args ...any) {
		t.Helper()
		rows, err := s.readDB.Query(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			if out[id] {
				t.Errorf("ground truth duplicate id %d", id)
			}
			out[id] = true
		}
	}
	collect(`SELECT rowid FROM messages_fts_stem WHERE messages_fts_stem MATCH ? AND rowid >= ?`, stemQ, marker)
	collect(`SELECT rowid FROM messages_fts WHERE messages_fts MATCH ? AND rowid < ?`, legacyQ, marker)
	return out
}

func sameIDs(got, want map[int64]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for id := range want {
		if !got[id] {
			return false
		}
	}
	return true
}

func hasMessage(hits []SearchResult, id int64) bool {
	for _, h := range hits {
		if int64(h.MessageID) == id && h.Role != "compaction" && h.Role != "vault" {
			return true
		}
	}
	return false
}

func usedDBBytes(t *testing.T, s *Store) int64 {
	t.Helper()
	var pages, free, size int64
	if err := s.readDB.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	return (pages - free) * size
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func resetStemHooks() {
	stemNoYield = false
	stemSkipCalibrate = false
	stemBatchOverride = 0
	stemAfterBatch = nil
}
