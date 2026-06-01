// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// newSoakTestExporter builds an Exporter under vault_layout="both"
// with a short soak window and an injectable state path. Returns the
// exporter and the state path so the caller can pre-seed first_seen
// timestamps to simulate elapsed soak.
func newSoakTestExporter(t *testing.T, soak time.Duration) (*Exporter, string) {
	t.Helper()
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-soak-01", []map[string]any{
		storetest.MetaMsg("user", "hi", "2026-05-10T10:00:00Z",
			"/Users/alice/dev/myapp", "main"),
	})
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")
	exp, err := New(s, vaultDir, Options{
		Layout:        store.VaultLayoutBoth,
		SoakWarnAfter: soak,
		StatePath:     statePath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exp, statePath
}

func TestSoakWarningWithinWindowDoesNotEmit(t *testing.T) {
	exp, statePath := newSoakTestExporter(t, 1*time.Hour) // 1h soak
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st, _ := store.LoadState(statePath)
	if !st.LastSoakWarnAt.IsZero() {
		t.Error("within-soak first sync should not emit warning; LastSoakWarnAt was stamped")
	}
}

func TestSoakWarningPastWindowEmits(t *testing.T) {
	exp, statePath := newSoakTestExporter(t, 1*time.Hour) // 1h soak
	// Pre-seed first_seen so the next sync sees us >1h into "both".
	st, _ := store.LoadState(statePath)
	st.StampLayoutFirstSeen(store.VaultLayoutBoth, time.Now().Add(-2*time.Hour))
	if err := st.Write(statePath); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st2, _ := store.LoadState(statePath)
	if st2.LastSoakWarnAt.IsZero() {
		t.Error("past-soak sync should have emitted warning and stamped LastSoakWarnAt")
	}
}

func TestSoakWarningWeeklyCadenceSuppressesRepeat(t *testing.T) {
	exp, statePath := newSoakTestExporter(t, 1*time.Hour)
	// Pre-seed past-soak first_seen AND a recent LastSoakWarnAt
	// (just hours ago). The next sync must NOT re-emit.
	st, _ := store.LoadState(statePath)
	st.StampLayoutFirstSeen(store.VaultLayoutBoth, time.Now().Add(-100*time.Hour))
	recent := time.Now().Add(-2 * time.Hour)
	st.LastSoakWarnAt = recent
	if err := st.Write(statePath); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st2, _ := store.LoadState(statePath)
	// LastSoakWarnAt should be unchanged (within the 7-day cadence).
	if !st2.LastSoakWarnAt.Equal(recent.UTC()) && !st2.LastSoakWarnAt.Equal(recent) {
		t.Errorf("weekly cadence violated: recent warn %v overwritten with %v",
			recent, st2.LastSoakWarnAt)
	}
}

func TestSoakWarningFiresAgainAfterWeek(t *testing.T) {
	exp, statePath := newSoakTestExporter(t, 1*time.Hour)
	// Past-soak first_seen + a stale LastSoakWarnAt (>7 days ago)
	// → another warning is owed.
	st, _ := store.LoadState(statePath)
	st.StampLayoutFirstSeen(store.VaultLayoutBoth, time.Now().Add(-100*time.Hour))
	stale := time.Now().Add(-8 * 24 * time.Hour)
	st.LastSoakWarnAt = stale
	if err := st.Write(statePath); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st2, _ := store.LoadState(statePath)
	if !st2.LastSoakWarnAt.After(stale.UTC()) {
		t.Errorf("weekly cadence should have fired: stale %v → got %v",
			stale, st2.LastSoakWarnAt)
	}
}
