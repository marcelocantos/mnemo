// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// newWingTestExporter builds an Exporter wired to a tempdir-resident
// state.json and runs a single Sync. Returns the Exporter, the vault
// root, the state path, and the loaded state after sync.
func newWingTestExporter(t *testing.T, layout string, soak time.Duration) (*Exporter, string, string, *store.State) {
	t.Helper()
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-wing-01", []map[string]any{
		storetest.MetaMsg("user", "hello wing", "2026-05-10T10:00:00Z",
			"/Users/alice/dev/myapp", "main"),
		storetest.Msg("assistant", "wing response", "2026-05-10T10:00:01Z"),
	})
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")
	exp, err := New(s, vaultDir, Options{
		Layout:        layout,
		SoakWarnAfter: soak,
		StatePath:     statePath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st, err := store.LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return exp, vaultDir, statePath, st
}

func TestWingWritesIndexAndReadmeUnderV2(t *testing.T) {
	_, vaultDir, _, _ := newWingTestExporter(t, store.VaultLayoutV2, 0)
	for _, name := range []string{"index.md", "README.md"} {
		p := filepath.Join(vaultDir, "_mnemo", name)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s missing: %v", name, err)
			continue
		}
		if !strings.Contains(string(raw), generatedFence) {
			t.Errorf("%s missing generated fence", name)
		}
	}
}

func TestWingWritesIndexAndReadmeUnderBoth(t *testing.T) {
	_, vaultDir, _, _ := newWingTestExporter(t, store.VaultLayoutBoth, 0)
	for _, name := range []string{"index.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(vaultDir, "_mnemo", name)); err != nil {
			t.Errorf("%s missing under both: %v", name, err)
		}
	}
}

func TestWingSuppressedUnderV1(t *testing.T) {
	_, vaultDir, _, _ := newWingTestExporter(t, store.VaultLayoutV1, 0)
	if _, err := os.Stat(filepath.Join(vaultDir, "_mnemo")); !os.IsNotExist(err) {
		t.Errorf("under v1, _mnemo/ should not be created; stat err=%v", err)
	}
}

func TestWingStampsLayoutFirstSeen(t *testing.T) {
	_, _, _, st := newWingTestExporter(t, store.VaultLayoutBoth, 0)
	if st.LayoutFirstSeen(store.VaultLayoutBoth).IsZero() {
		t.Fatal("first sync under both should have stamped first_seen.both")
	}
	if !st.LayoutFirstSeen(store.VaultLayoutV1).IsZero() ||
		!st.LayoutFirstSeen(store.VaultLayoutV2).IsZero() {
		t.Error("first-seen for non-active layouts should still be zero")
	}
}

func TestWingPreservesBelowFenceAnnotation(t *testing.T) {
	exp, vaultDir, _, _ := newWingTestExporter(t, store.VaultLayoutV2, 0)
	idx := filepath.Join(vaultDir, "_mnemo", "index.md")
	raw, _ := os.ReadFile(idx)
	annotation := "\n## My note\n\nMy personal annotation on the wing index.\n"
	if err := os.WriteFile(idx, append(raw, []byte(annotation)...), 0o644); err != nil {
		t.Fatalf("write annotation: %v", err)
	}
	// Re-sync. Above-fence content rotates; below-fence content stays.
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("re-Sync: %v", err)
	}
	final, _ := os.ReadFile(idx)
	if !strings.Contains(string(final), "My personal annotation") {
		t.Error("below-fence annotation lost across re-sync")
	}
	if strings.Count(string(final), generatedFence) != 1 {
		t.Errorf("expected exactly 1 fence after re-sync, got %d",
			strings.Count(string(final), generatedFence))
	}
}

func TestWingFirstSeenIdempotentAcrossSyncs(t *testing.T) {
	exp, _, statePath, st := newWingTestExporter(t, store.VaultLayoutBoth, 0)
	firstStamp := st.LayoutFirstSeen(store.VaultLayoutBoth)
	// Sleep just enough that a re-stamp would observe a different
	// time. The stamp must NOT change.
	time.Sleep(20 * time.Millisecond)
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	st2, _ := store.LoadState(statePath)
	if got := st2.LayoutFirstSeen(store.VaultLayoutBoth); !got.Equal(firstStamp) {
		t.Errorf("first_seen.both moved across syncs: %v → %v", firstStamp, got)
	}
}
