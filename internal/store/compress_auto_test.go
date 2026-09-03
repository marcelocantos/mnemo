// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"testing"
	"time"
)

func TestAutoBackfillPacksPlainRowsWithoutAnOpsCall(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 80)

	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	var before int64
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText {
			before = f.Outstanding
		}
	}
	if before == 0 {
		t.Fatal("seeded plain rows should be outstanding before the worker runs")
	}

	s.StartCompressBackfill()
	waitOutstanding(t, s, FamilyMessagesText, 0)

	snap := s.CompressWorkerStatus()
	if snap.Phase != CompressPhaseComplete && snap.Phase != CompressPhaseIdle {
		t.Fatalf("phase=%s reason=%s, want complete after packing", snap.Phase, snap.Reason)
	}
}

func TestAutoBackfillRestartsWhenPlainRowsReappear(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 40)
	if _, err := s.CompressBackfill(t.Context(), FamilyMessagesText); err != nil {
		t.Fatal(err)
	}
	waitOutstanding(t, s, FamilyMessagesText, 0)
	if err := s.saveBackfillCursor(FamilyMessagesText, 1<<20, 0, true); err != nil {
		t.Fatal(err)
	}

	seedLegacyMessages(t, s, 25)
	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	var outstanding int64
	var done bool
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText {
			outstanding = f.Outstanding
			done = f.BackfillDone
		}
	}
	if outstanding == 0 {
		t.Fatal("re-seeded plain rows should be outstanding")
	}
	if !done {
		t.Fatal("cursor was marked done; the worker must reopen it")
	}

	s.compressBackfillCycle(t.Context())
	waitOutstanding(t, s, FamilyMessagesText, 0)
}

func TestAutoBackfillDisabledStaysOff(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.disableAutoBackfillForTest()
	seedLegacyMessages(t, s, 20)
	s.StartCompressBackfill()
	s.compressBackfillCycle(t.Context())

	st, err := s.CompressionStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Families {
		if f.Family == FamilyMessagesText && f.Outstanding == 0 {
			t.Fatal("disabled worker must not pack leftover rows")
		}
	}
	if snap := s.CompressWorkerStatus(); snap.Phase != CompressPhaseDisabled {
		t.Fatalf("phase=%s, want disabled", snap.Phase)
	}
}

func TestCompressionConfigAutoBackfillDefaultsOn(t *testing.T) {
	if !(CompressionConfig{}).AutoBackfillEnabled() {
		t.Fatal("absent compression section must enable auto-backfill")
	}
	off := false
	if (CompressionConfig{AutoBackfill: &off}).AutoBackfillEnabled() {
		t.Fatal("explicit false must disable auto-backfill")
	}
}

func TestErrBackfillRunningIsSentinel(t *testing.T) {
	if !errors.Is(errors.Join(ErrBackfillRunning), ErrBackfillRunning) {
		t.Fatal("sentinel must survive wrapping")
	}
}

func seedLegacyMessages(t *testing.T, s *Store, n int) {
	t.Helper()
	tx, err := s.writeDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(messageInsertLegacySQL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		text := longText("auto", 3)
		if _, err := stmt.Exec(nil, "sess-auto", "proj", "assistant", text, "2026-04-01T10:00:00Z", "assistant", 0,
			"text", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func waitOutstanding(t *testing.T, s *Store, family string, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.CompressionStatus()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range st.Families {
			if f.Family == family && f.Outstanding == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("outstanding for %s did not reach %d", family, want)
}
