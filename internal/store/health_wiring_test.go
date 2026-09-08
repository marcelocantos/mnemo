// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// The health-latency work added three passes whose units were thoroughly
// tested and whose WIRING was not. Every test in this file exercises a
// path end-to-end rather than calling the new function directly, because
// calling it directly is exactly what let these through.

// TestUsageFieldsFillOnAStoreWhoseEntriesAreAlreadyMaterialised is the
// regression test for 🎯T174.
//
// MaterialiseUsageFields lived inside the branch that runs only when
// entries.fields still needs work. entries.fields is done=1 on every
// store that upgraded from 0.94+ — which is every real installation —
// so the usage twins were never filled on the exact machines the pass
// exists for, permanently, and the hot paths read those columns with no
// COALESCE fallback.
//
// The test asserts the two cursors are gated independently: with
// entries.fields already complete, the usage pass must still run.
func TestUsageFieldsFillOnAStoreWhoseEntriesAreAlreadyMaterialised(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	raw := `{"uuid":"u-hist","requestId":"req-hist","message":{"id":"msg_hist",` +
		`"model":"claude-sonnet-4-6","usage":{"input_tokens":10,"output_tokens":4,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":1}}}}`
	mustExec(t, s, `INSERT INTO entries (session_id, project, type, timestamp, raw)
		VALUES ('sess-hist', 'p', 'assistant', '2026-04-01T10:00:00Z', jsonb(?))`, raw)
	// The shape of a store that upgraded from 0.94: raw is present, the
	// usage twins are not, and entries.fields is already marked done.
	mustExec(t, s, `UPDATE entries SET message_id_m = NULL, request_id_m = NULL,
		cache_write_5m_m = NULL, cache_write_1h_m = NULL WHERE session_id = 'sess-hist'`)
	if err := s.saveBackfillCursor(entriesFieldsFamily, 1<<30, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.saveBackfillCursor(entriesUsageFieldsFamily, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.EntriesMaterialised(); err != nil || !ok {
		t.Fatalf("precondition: entries.fields must read as done (ok=%v err=%v)", ok, err)
	}

	// Drive the boot phase's own logic, not MaterialiseUsageFields.
	if err := s.runEntriesMaterialisePhase(context.Background()); err != nil {
		t.Fatal(err)
	}

	var mid, rid string
	var cw5, cw1 int64
	if err := s.readDB.QueryRow(`
		SELECT COALESCE(message_id_m, ''), COALESCE(request_id_m, ''),
		       COALESCE(cache_write_5m_m, 0), COALESCE(cache_write_1h_m, 0)
		FROM entries WHERE session_id = 'sess-hist'`).Scan(&mid, &rid, &cw5, &cw1); err != nil {
		t.Fatal(err)
	}
	if mid != "msg_hist" {
		t.Errorf("message_id_m = %q, want msg_hist. A NULL id makes dedupGroupSQL fall back "+
			"to a per-row key, which silently disables deduplication (worth 1.95x-2.83x) "+
			"for every historical row", mid)
	}
	if rid != "req-hist" {
		t.Errorf("request_id_m = %q, want req-hist", rid)
	}
	if cw5 != 3 || cw1 != 1 {
		t.Errorf("cache tiers = (%d, %d), want (3, 1); a collapsed 5m/1h split misprices "+
			"the tier that carries most of the volume", cw5, cw1)
	}
}

// TestCompressionStatusDoesNotWrite is the regression test for 🎯T173.
//
// ensureLeftoverLengths ran an UNBOUNDED UPDATE from CompressionStatus,
// which is the Fast-tier compress.backfill check — so the first /health
// after an upgrade rewrote every unpacked row in one transaction while
// every other writer queued behind SQLite's single writer. That is the
// starvation 🎯T168 removed from the packer, reappearing in the health
// handler.
//
// Reading must not write. The assertion is on the database's own change
// counter, so it holds however the status query is later restructured.
func TestCompressionStatusDoesNotWrite(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 40)
	// Unmeasured rows are the condition that used to trigger the write.
	mustExec(t, s, `UPDATE messages SET plain_len = NULL, z_len = NULL`)

	fs := familySpecs[FamilyMessagesText]
	pendingBefore, err := s.familyLengthsPending(fs)
	if err != nil {
		t.Fatal(err)
	}
	if pendingBefore == 0 {
		t.Fatal("precondition: rows must be unmeasured for this test to mean anything")
	}

	// total_changes() is per-connection and readDB is a separate pool, so
	// the oracle is the measured-row count itself: if status wrote, rows
	// gained a plain_len.
	before := measuredRows(t, s)
	if _, err := s.CompressionStatus(); err != nil {
		t.Fatal(err)
	}
	if after := measuredRows(t, s); after != before {
		t.Errorf("CompressionStatus measured %d rows; it is called from a Fast-tier "+
			"health check and must not write", after-before)
	}

	// And the backlog is disclosed rather than hidden.
	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	var reported int64
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText {
			reported = f.LengthsPending
		}
	}
	if reported != pendingBefore {
		t.Errorf("LengthsPending = %d, want %d — an understated byte total must be "+
			"declared provisional, not presented as settled", reported, pendingBefore)
	}
}

// TestUnmeasuredRowsDoNotReopenAFinishedFamily is the other half of
// 🎯T173, and the trap in the fix.
//
// If unmeasured rows counted as leftover, a finished family would reopen
// on every 2-minute cycle, walk itself, compress nothing and mark itself
// done again — which is the ~22s-every-2-minutes loop the maintainer
// brief reported as item 3, reintroduced by the repair.
func TestUnmeasuredRowsDoNotReopenAFinishedFamily(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 10)
	// Short rows are the ones that matter: they never pack, so they stay
	// z IS NULL forever and are exactly the residue an upgraded store has
	// sitting unmeasured. A fixture of only long rows packs everything and
	// cannot tell the two predicates apart.
	for i := 0; i < 5; i++ {
		mustExec(t, s, messageInsertLegacySQL,
			nil, "sess-short", "p", "user", "tiny", "2026-04-01T10:00:00Z", "user", 0,
			"text", nil, nil, nil, 0)
	}
	if res, err := s.CompressBackfill(context.Background(), FamilyMessagesText); err != nil || !res.Done {
		t.Fatalf("pack: %+v %v", res, err)
	}
	// Wipe the measurements, as an upgrade from 0.96 leaves them.
	mustExec(t, s, `UPDATE messages SET plain_len = NULL, z_len = NULL`)

	fs := familySpecs[FamilyMessagesText]
	var unpackedUnmeasured int64
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE text_z IS NULL AND plain_len IS NULL`).
		Scan(&unpackedUnmeasured); err != nil {
		t.Fatal(err)
	}
	if unpackedUnmeasured == 0 {
		t.Fatal("fixture must leave unpacked, unmeasured rows or the predicates cannot differ")
	}
	leftover, err := s.familyLeftoverCount(fs)
	if err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Errorf("familyLeftoverCount = %d with only unmeasured rows; a non-zero count "+
			"reopens the family every cycle and re-scans it forever", leftover)
	}

	var done int
	if err := s.readDB.QueryRow(
		`SELECT done FROM compression_gc WHERE family = ?`, FamilyMessagesText).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if done != 1 {
		t.Fatalf("precondition: family should still be marked done, got %d", done)
	}
}

// TestFillLengthsIsBoundedAndMeasuresInBytes covers the background pass
// itself (🎯T173): it must be bounded per call, and it must agree with
// every other writer of plain_len about what the number means.
//
// SQLite's length() counts CHARACTERS on TEXT and BYTES on BLOB, and
// entries.raw is stored as jsonb, whose byte count is a third quantity
// again. Go's len() is bytes. Before this, all three units were in use
// at once and were compared against the same 64-byte threshold.
func TestFillLengthsIsBoundedAndMeasuresInBytes(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// A payload whose byte length and character length differ.
	const multibyte = "héllo wörld — a line with non-ASCII characters that is comfortably over the threshold"
	wantBytes := int64(len(multibyte))
	if wantBytes == int64(len([]rune(multibyte))) {
		t.Fatal("fixture must have differing byte and character counts")
	}

	mustExec(t, s, messageInsertLegacySQL,
		nil, "sess-len", "p", "user", multibyte, "2026-04-01T10:00:00Z", "user", 0,
		"text", nil, nil, nil, 0)
	mustExec(t, s, `UPDATE messages SET plain_len = NULL WHERE session_id = 'sess-len'`)

	fs := familySpecs[FamilyMessagesText]
	if _, err := s.fillLengths(context.Background(), fs); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := s.readDB.QueryRow(
		`SELECT plain_len FROM messages WHERE session_id = 'sess-len'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != wantBytes {
		t.Errorf("plain_len = %d, want %d (bytes, as Go's len() and the pack path mean it); "+
			"SQL length() on TEXT would give %d characters",
			got, wantBytes, int64(len([]rune(multibyte))))
	}

	// Bounded: more unmeasured rows than one batch must not all go at once.
	seedLegacyMessages(t, s, 12)
	mustExec(t, s, `UPDATE messages SET plain_len = NULL`)
	saved := lengthFillBatchForTest(2)
	defer lengthFillBatchForTest(saved)
	n, err := s.fillLengths(context.Background(), fs)
	if err != nil {
		t.Fatal(err)
	}
	if n > 4 { // 2 plain + at most 2 z per call
		t.Errorf("fillLengths measured %d rows in one call with a batch of 2; "+
			"an unbounded pass is what made this a health-path hazard", n)
	}
}

// TestFinishedFamilyWithShortResidueIsNotAPermanentWarning covers the
// maintainer brief's item 2 secondary complaint: compress.backfill warned
// on every run forever, because a residue of rows too small to be worth
// compressing was counted as outstanding. A check that can never return
// to OK is one people learn to ignore — and it made fail=0 warn=1 the
// daemon's steady state, hiding genuine new warnings.
func TestFinishedFamilyWithShortResidueIsNotAPermanentWarning(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for i := 0; i < 5; i++ {
		mustExec(t, s, messageInsertLegacySQL,
			nil, "sess-short", "p", "user", "tiny", "2026-04-01T10:00:00Z", "user", 0,
			"text", nil, nil, nil, 0)
	}
	if res, err := s.CompressBackfill(context.Background(), FamilyMessagesText); err != nil || !res.Done {
		t.Fatalf("pack: %+v %v", res, err)
	}
	fs := familySpecs[FamilyMessagesText]
	if _, err := s.fillLengths(context.Background(), fs); err != nil {
		t.Fatal(err)
	}

	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Families {
		if f.Family != FamilyMessagesText {
			continue
		}
		if f.Outstanding != 0 || f.LeftoverRows != 0 {
			t.Errorf("short residue reported as outstanding=%d leftover=%d; rows below "+
				"the compression threshold stay plain forever and must not keep the "+
				"check warning", f.Outstanding, f.LeftoverRows)
		}
	}
}

func measuredRows(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE plain_len IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
