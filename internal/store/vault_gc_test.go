// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestVaultOrphansSetDifference verifies the orphan detection is
// exact set-difference over the manifest, in both directions:
// manifest rows whose file is gone, and on-disk *.md files with no
// manifest row.
func TestVaultOrphansSetDifference(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	vault := t.TempDir()
	now := time.Now().UTC()

	mustWrite := func(rel, body string) {
		t.Helper()
		full := filepath.Join(vault, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Three concrete files. The exporter (we simulate it directly
	// here) records two of them in the manifest; one is a user note
	// with no manifest entry.
	mustWrite("sessions/proj/2026-01-01-a.md", "note a")
	mustWrite("sessions/proj/2026-01-02-b.md", "note b")
	mustWrite("notes/user-content.md", "my own note")

	if err := s.RecordVaultOutput(
		"sessions/proj/2026-01-01-a.md", "session", "sess-a",
		HashVaultContent("note a"), now); err != nil {
		t.Fatalf("RecordVaultOutput a: %v", err)
	}
	if err := s.RecordVaultOutput(
		"sessions/proj/2026-01-02-b.md", "session", "sess-b",
		HashVaultContent("note b"), now); err != nil {
		t.Fatalf("RecordVaultOutput b: %v", err)
	}

	// Plus a manifest row pointing at a file that's NOT on disk
	// (e.g. the user manually removed it).
	if err := s.RecordVaultOutput(
		"sessions/proj/2026-01-03-c.md", "session", "sess-c",
		HashVaultContent("note c"), now); err != nil {
		t.Fatalf("RecordVaultOutput c: %v", err)
	}

	rep, err := s.ScanVaultOrphans(vault)
	if err != nil {
		t.Fatalf("ScanVaultOrphans: %v", err)
	}

	if len(rep.ManifestPathMissing) != 1 || rep.ManifestPathMissing[0].EntityID != "sess-c" {
		t.Errorf("expected manifest-path-missing = [sess-c], got %+v", rep.ManifestPathMissing)
	}
	want := "notes/user-content.md"
	if len(rep.DiskNotInManifest) != 1 || rep.DiskNotInManifest[0] != want {
		t.Errorf("expected disk-not-in-manifest = [%q], got %+v", want, rep.DiskNotInManifest)
	}

	// Removing the orphaned manifest row brings that side to
	// convergence; the disk side is untouched (conservative — never
	// delete user content from this layer).
	if err := s.RemoveVaultManifestRow("sessions/proj/2026-01-03-c.md"); err != nil {
		t.Fatalf("RemoveVaultManifestRow: %v", err)
	}
	rep, err = s.ScanVaultOrphans(vault)
	if err != nil {
		t.Fatalf("ScanVaultOrphans (after cleanup): %v", err)
	}
	if len(rep.ManifestPathMissing) != 0 {
		t.Errorf("expected manifest-path-missing = 0 after cleanup, got %+v", rep.ManifestPathMissing)
	}
	if len(rep.DiskNotInManifest) != 1 {
		t.Errorf("disk side must remain unchanged by manifest cleanup, got %+v", rep.DiskNotInManifest)
	}
}

// TestVaultGCCleanupRemovesOnlyManifestRows verifies the destructive
// half of the GC: cleanup acts on manifest_path_missing rows (DB only)
// and never touches the filesystem, including on-disk orphans.
func TestVaultGCCleanupRemovesOnlyManifestRows(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	vault := t.TempDir()
	now := time.Now().UTC()

	// One on-disk file with a matching manifest entry (no orphan).
	intact := filepath.Join(vault, "sessions/proj/intact.md")
	if err := os.MkdirAll(filepath.Dir(intact), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(intact, []byte("intact"), 0o644); err != nil {
		t.Fatalf("write intact: %v", err)
	}
	if err := s.RecordVaultOutput(
		"sessions/proj/intact.md", "session", "sess-intact",
		HashVaultContent("intact"), now); err != nil {
		t.Fatalf("RecordVaultOutput intact: %v", err)
	}

	// One manifest row whose file is gone.
	if err := s.RecordVaultOutput(
		"sessions/proj/missing.md", "session", "sess-missing",
		HashVaultContent("missing"), now); err != nil {
		t.Fatalf("RecordVaultOutput missing: %v", err)
	}

	// One on-disk user note with no manifest entry.
	userNote := filepath.Join(vault, "user-content.md")
	if err := os.WriteFile(userNote, []byte("my own note"), 0o644); err != nil {
		t.Fatalf("write user: %v", err)
	}

	rep, err := s.ScanVaultOrphans(vault)
	if err != nil {
		t.Fatalf("ScanVaultOrphans: %v", err)
	}
	if len(rep.ManifestPathMissing) != 1 || rep.ManifestPathMissing[0].EntityID != "sess-missing" {
		t.Fatalf("setup: expected one manifest-missing orphan, got %+v", rep.ManifestPathMissing)
	}
	if len(rep.DiskNotInManifest) != 1 || rep.DiskNotInManifest[0] != "user-content.md" {
		t.Fatalf("setup: expected one disk orphan (user-content.md), got %+v", rep.DiskNotInManifest)
	}

	// Simulate the confirm-true cleanup: remove the manifest row.
	for _, m := range rep.ManifestPathMissing {
		if err := s.RemoveVaultManifestRow(m.NotePath); err != nil {
			t.Fatalf("RemoveVaultManifestRow: %v", err)
		}
	}

	// The user note must still exist on disk (never deleted by GC).
	if _, err := os.Stat(userNote); err != nil {
		t.Errorf("user content must survive the GC pass; stat: %v", err)
	}
	// And the intact note's content is unchanged.
	body, err := os.ReadFile(intact)
	if err != nil {
		t.Fatalf("read intact: %v", err)
	}
	if string(body) != "intact" {
		t.Errorf("intact note tampered with: %q", string(body))
	}

	// Manifest side has converged: zero manifest_path_missing.
	rep, err = s.ScanVaultOrphans(vault)
	if err != nil {
		t.Fatalf("ScanVaultOrphans post-cleanup: %v", err)
	}
	if len(rep.ManifestPathMissing) != 0 {
		t.Errorf("manifest side must be converged, got %+v", rep.ManifestPathMissing)
	}
	if len(rep.DiskNotInManifest) != 1 {
		t.Errorf("disk side must be unchanged (user content preserved), got %+v", rep.DiskNotInManifest)
	}
}

// TestVaultOrphansEmptyPath verifies that ScanVaultOrphans is a no-op
// when no vault is configured.
func TestVaultOrphansEmptyPath(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	rep, err := s.ScanVaultOrphans("")
	if err != nil {
		t.Fatalf("ScanVaultOrphans(\"\"): %v", err)
	}
	if len(rep.ManifestPathMissing) != 0 || len(rep.DiskNotInManifest) != 0 {
		t.Errorf("expected empty result, got %+v", rep)
	}
	if gap := s.VaultOrphanBacklog(""); gap != 0 {
		t.Errorf("expected backlog=0 for empty vault, got %d", gap)
	}
}
