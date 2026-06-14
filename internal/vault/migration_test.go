// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// seedV1Vault populates the vault with the same v1 marker dirs the
// resolver checks for. The presence of these dirs is what triggers
// MIGRATION.md on first sync under "both".
func seedV1Vault(t *testing.T, dir string) {
	t.Helper()
	for _, d := range []string{"sessions", "decisions", "memories"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatalf("seed %s: %v", d, err)
		}
	}
}

func newMigrationTestExporter(t *testing.T) (*Exporter, string, string) {
	t.Helper()
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-mig-01", []map[string]any{
		storetest.MetaMsg("user", "hello", "2026-05-10T10:00:00Z",
			"/Users/alice/dev/myapp", "main"),
	})
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()
	seedV1Vault(t, vaultDir)
	statePath := filepath.Join(t.TempDir(), "state.json")
	exp, err := New(s, vaultDir, Options{
		Layout:    store.VaultLayoutBoth,
		StatePath: statePath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exp, vaultDir, statePath
}

func TestMigrationDocWrittenOnceUnderBoth(t *testing.T) {
	exp, vaultDir, _ := newMigrationTestExporter(t)
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	mig := filepath.Join(vaultDir, "_mnemo", "MIGRATION.md")
	raw, err := os.ReadFile(mig)
	if err != nil {
		t.Fatalf("MIGRATION.md missing after first sync: %v", err)
	}
	// Vault path must be interpolated so the user sees their actual path.
	if !strings.Contains(string(raw), vaultDir) {
		t.Errorf("MIGRATION.md should reference vault path %q, got:\n%s", vaultDir, raw)
	}
	// MIGRATION.md must NOT carry the generated fence — it is fully
	// user-owned from the moment it lands on disk.
	if strings.Contains(string(raw), generatedFence) {
		t.Error("MIGRATION.md should not contain the generated fence")
	}
}

func TestMigrationDocNotRecreatedAfterDeletion(t *testing.T) {
	exp, vaultDir, _ := newMigrationTestExporter(t)
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	mig := filepath.Join(vaultDir, "_mnemo", "MIGRATION.md")
	// User deletes the doc — mnemo treats this as "read; move on".
	if err := os.Remove(mig); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if _, err := os.Stat(mig); !os.IsNotExist(err) {
		t.Errorf("MIGRATION.md was recreated after user deletion; stat err=%v", err)
	}
}

func TestRegenerateMigrationDocBringsItBack(t *testing.T) {
	exp, vaultDir, _ := newMigrationTestExporter(t)
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	mig := filepath.Join(vaultDir, "_mnemo", "MIGRATION.md")
	if err := os.Remove(mig); err != nil {
		t.Fatalf("delete: %v", err)
	}
	content, err := exp.RegenerateMigrationDoc()
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if content == "" {
		t.Error("Regenerate should return non-empty snapshot")
	}
	if _, err := os.Stat(mig); err != nil {
		t.Errorf("MIGRATION.md missing after regen: %v", err)
	}
}

func TestMigrationDocSnapshotDoesNotWriteFile(t *testing.T) {
	exp, vaultDir, _ := newMigrationTestExporter(t)
	// No sync yet → no _mnemo/ dir → snapshot must still return content
	// without creating any files.
	snap := exp.MigrationDocSnapshot()
	if snap == "" {
		t.Error("snapshot must return non-empty content")
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "_mnemo")); !os.IsNotExist(err) {
		t.Errorf("snapshot must not create _mnemo/; stat err=%v", err)
	}
}

func TestMigrationDocNotWrittenUnderV2(t *testing.T) {
	// Under v2 layout, MIGRATION.md must NOT auto-write — it's an
	// explicit v1→v2 transition signal, only relevant under "both".
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-v2-01", []map[string]any{
		storetest.MetaMsg("user", "hello", "2026-05-10T10:00:00Z",
			"/Users/alice/dev/myapp", "main"),
	})
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()
	seedV1Vault(t, vaultDir) // v1 dirs present but layout explicitly v2
	exp, err := New(s, vaultDir, Options{
		Layout:    store.VaultLayoutV2,
		StatePath: filepath.Join(t.TempDir(), "state.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	mig := filepath.Join(vaultDir, "_mnemo", "MIGRATION.md")
	if _, err := os.Stat(mig); !os.IsNotExist(err) {
		t.Errorf("MIGRATION.md should not be auto-written under v2; stat err=%v", err)
	}
}
