// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// isolateConfig points MNEMO_HOME at a scratch directory so LoadConfig
// reads an absent config rather than the developer's own.
//
// autoBackfillEnabled() consults the config on every cycle, by design —
// it is how the operator pauses the packer without a restart. The
// consequence for tests is that a machine whose ~/.mnemo/config.json
// sets compression.auto_backfill=false makes every test in this file
// fail, with "outstanding did not reach 0" forty-five seconds later and
// nothing to suggest the cause is a file outside the repo. That is not
// hypothetical: it happened on 2026-09-21, while the owner had the
// packer paused to stop it burning a third of a core.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(MnemoHomeEnv, t.TempDir())
}

func TestAutoBackfillPacksPlainRowsWithoutAnOpsCall(t *testing.T) {
	isolateConfig(t)
	s := newTestStore(t, t.TempDir())
	seedLegacyMessages(t, s, 80)

	// Legacy-shaped inserts leave plain_len NULL, and an unmeasured row is
	// deliberately not actionable leftover (🎯T173) — otherwise a finished
	// family reopens every cycle. Measure first, exactly as the worker does
	// at the top of each cycle.
	measureAllFamilies(t, s)

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

	// Outstanding reaching zero is not the end of the cycle: the worker
	// still has to finish its paced pass and publish a phase (🎯T168 made
	// that window wide enough to lose the race reliably). Poll for the
	// settled phase rather than sampling it once.
	snap := s.CompressWorkerStatus()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if snap = s.CompressWorkerStatus(); snap.Phase == CompressPhaseComplete || snap.Phase == CompressPhaseIdle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("phase=%s reason=%s, want complete after packing", snap.Phase, snap.Reason)
}

func TestAutoBackfillRestartsWhenPlainRowsReappear(t *testing.T) {
	isolateConfig(t)
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
	measureAllFamilies(t, s) // see 🎯T173: unmeasured rows are not leftover
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

	// Assert on packing directly rather than via Outstanding: a disabled
	// worker also skips the measurement pass, so Outstanding is 0 for a
	// reason unrelated to what this test is about (🎯T173).
	var packed int64
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE text_z IS NOT NULL`).Scan(&packed); err != nil {
		t.Fatal(err)
	}
	if packed != 0 {
		t.Fatalf("disabled worker packed %d rows", packed)
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
	// Generous because the auto worker paces itself against a duty cycle
	// now (🎯T168): a pass deliberately costs about twice its busy time,
	// and this has to hold on a loaded machine running the full suite.
	deadline := time.Now().Add(45 * time.Second)
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

// measureAllFamilies runs the bounded length-measurement pass for every
// family, which is what compressBackfillCycle does before probing
// leftover (🎯T173). Tests that seed through a legacy-shaped insert need
// it, because those rows carry no plain_len and an unmeasured row is not
// actionable leftover.
func measureAllFamilies(t *testing.T, s *Store) {
	t.Helper()
	for _, family := range allFamilies {
		fs, err := familyOf(family)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.fillLengths(context.Background(), fs); err != nil {
			t.Fatal(err)
		}
	}
}
