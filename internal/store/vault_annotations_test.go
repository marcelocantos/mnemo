// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestStoreForVault returns a Store backed by a temp DB suitable
// for exercising IngestVaultAnnotations without an upstream
// project directory. The transcripts subsystem is unused here.
func newTestStoreForVault(t *testing.T) *Store {
	t.Helper()
	db := filepath.Join(t.TempDir(), "vault-test.db")
	s, err := New(db, t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeMD(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func indexedVaultPaths(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT file_path FROM docs WHERE kind = 'vault' ORDER BY file_path")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestScopeMnemoOnlyExcludesOutside writes one file inside _mnemo/ and
// another at the vault root. Under "_mnemo_only", only the wing file
// is indexed and the root-level file stays invisible to mnemo_search.
func TestScopeMnemoOnlyExcludesOutside(t *testing.T) {
	vault := t.TempDir()
	inside := filepath.Join(vault, "_mnemo", "themes", "auth.md")
	outside := filepath.Join(vault, "areas", "secret.md")
	writeMD(t, inside, "# auth\n\nwing content\n")
	writeMD(t, outside, "# secret\n\nshould not be indexed\n")

	s := newTestStoreForVault(t)
	if err := s.IngestVaultAnnotations(vault, VaultIngestOptions{Scope: VaultScopeMnemoOnly}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := indexedVaultPaths(t, s)
	if !contains(got, inside) {
		t.Errorf("expected %s indexed, got %v", inside, got)
	}
	if contains(got, outside) {
		t.Errorf("expected %s NOT indexed under _mnemo_only, got %v", outside, got)
	}
}

// TestScopeFullIndexesEverything mirrors the v1 behaviour: a file at
// any path under the vault root (minus hidden dirs) is indexed.
func TestScopeFullIndexesEverything(t *testing.T) {
	vault := t.TempDir()
	a := filepath.Join(vault, "_mnemo", "themes", "auth.md")
	b := filepath.Join(vault, "areas", "knowledge", "auth-deep-dive.md")
	c := filepath.Join(vault, "projects", "x", "notes.md")
	writeMD(t, a, "# auth wing\n")
	writeMD(t, b, "# auth deep dive\n")
	writeMD(t, c, "# project notes\n")

	s := newTestStoreForVault(t)
	if err := s.IngestVaultAnnotations(vault, VaultIngestOptions{Scope: VaultScopeFull}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := indexedVaultPaths(t, s)
	for _, want := range []string{a, b, c} {
		if !contains(got, want) {
			t.Errorf("expected %s indexed under full, got %v", want, got)
		}
	}
}

// TestScopeIncludesHonoursList ensures includes mode walks the wing
// plus exactly the configured include paths, and nothing else.
func TestScopeIncludesHonoursList(t *testing.T) {
	vault := t.TempDir()
	wing := filepath.Join(vault, "_mnemo", "themes", "auth.md")
	included := filepath.Join(vault, "areas", "knowledge", "auth-deep-dive.md")
	excluded := filepath.Join(vault, "projects", "x", "notes.md")
	writeMD(t, wing, "# wing\n")
	writeMD(t, included, "# included\n")
	writeMD(t, excluded, "# excluded\n")

	s := newTestStoreForVault(t)
	opts := VaultIngestOptions{
		Scope:    VaultScopeIncludes,
		Includes: []string{filepath.Join(vault, "areas", "knowledge")},
	}
	if err := s.IngestVaultAnnotations(vault, opts); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := indexedVaultPaths(t, s)
	if !contains(got, wing) || !contains(got, included) {
		t.Errorf("wing + included must be indexed; got %v", got)
	}
	if contains(got, excluded) {
		t.Errorf("excluded path must not be indexed under includes; got %v", got)
	}
}

// TestMnemoIgnoreApplies verifies .mnemoignore patterns filter even
// inside the wing under _mnemo_only.
func TestMnemoIgnoreApplies(t *testing.T) {
	vault := t.TempDir()
	keep := filepath.Join(vault, "_mnemo", "themes", "keep.md")
	drop := filepath.Join(vault, "_mnemo", "themes", "draft-foo.md")
	writeMD(t, keep, "# keep\n")
	writeMD(t, drop, "# draft\n")

	ignore := ParseVaultIgnore("_mnemo/themes/draft-*.md\n")
	s := newTestStoreForVault(t)
	opts := VaultIngestOptions{Scope: VaultScopeMnemoOnly, Ignore: ignore}
	if err := s.IngestVaultAnnotations(vault, opts); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := indexedVaultPaths(t, s)
	if !contains(got, keep) {
		t.Errorf("keep must be indexed; got %v", got)
	}
	if contains(got, drop) {
		t.Errorf("draft-foo.md must be excluded; got %v", got)
	}
}

// TestScopeNarrowingPrunesStale checks that an entry indexed under
// "full" disappears when a subsequent ingest narrows to "_mnemo_only".
func TestScopeNarrowingPrunesStale(t *testing.T) {
	vault := t.TempDir()
	outside := filepath.Join(vault, "areas", "secret.md")
	wing := filepath.Join(vault, "_mnemo", "themes", "auth.md")
	writeMD(t, outside, "# secret\n")
	writeMD(t, wing, "# wing\n")

	s := newTestStoreForVault(t)
	if err := s.IngestVaultAnnotations(vault, VaultIngestOptions{Scope: VaultScopeFull}); err != nil {
		t.Fatalf("ingest full: %v", err)
	}
	if !contains(indexedVaultPaths(t, s), outside) {
		t.Fatalf("expected %s indexed under full first pass", outside)
	}
	if err := s.IngestVaultAnnotations(vault, VaultIngestOptions{Scope: VaultScopeMnemoOnly}); err != nil {
		t.Fatalf("ingest narrowed: %v", err)
	}
	got := indexedVaultPaths(t, s)
	if contains(got, outside) {
		t.Errorf("expected outside row pruned after narrowing; got %v", got)
	}
	if !contains(got, wing) {
		t.Errorf("expected wing row retained; got %v", got)
	}
}

// TestEffectiveVaultIndexingScopeDefaults exercises the new-vault vs
// v1-vault detection logic that backs the migration safety valve.
func TestEffectiveVaultIndexingScopeDefaults(t *testing.T) {
	freshVault := t.TempDir()
	v1Vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(v1Vault, "sessions"), 0o755); err != nil {
		t.Fatalf("mk v1: %v", err)
	}
	v2Vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(v2Vault, "_mnemo"), 0o755); err != nil {
		t.Fatalf("mk v2: %v", err)
	}

	cfg := Config{}
	if got := cfg.EffectiveVaultIndexingScope(freshVault); got != VaultScopeMnemoOnly {
		t.Errorf("fresh vault default = %q, want %q", got, VaultScopeMnemoOnly)
	}
	if got := cfg.EffectiveVaultIndexingScope(v1Vault); got != VaultScopeFull {
		t.Errorf("v1-populated vault default = %q, want %q", got, VaultScopeFull)
	}
	if got := cfg.EffectiveVaultIndexingScope(v2Vault); got != VaultScopeMnemoOnly {
		t.Errorf("v2 vault (with _mnemo/) default = %q, want %q", got, VaultScopeMnemoOnly)
	}

	cfg.VaultIndexingScope = VaultScopeFull
	if got := cfg.EffectiveVaultIndexingScope(freshVault); got != VaultScopeFull {
		t.Errorf("explicit override must win, got %q", got)
	}
}

// TestResolvedVaultIndexingIncludesEscapeProtection ensures bad config
// (".."-escapes, absolute paths outside the vault) narrows scope rather
// than widening it.
func TestResolvedVaultIndexingIncludesEscapeProtection(t *testing.T) {
	vault := t.TempDir()
	cfg := Config{
		VaultIndexingIncludes: []string{
			"areas/knowledge",
			"../../escape",
			"/etc",
			"",
		},
	}
	got := cfg.ResolvedVaultIndexingIncludes(vault)
	if len(got) != 1 {
		t.Fatalf("expected 1 safe include, got %v", got)
	}
	wantPrefix := filepath.Join(vault, "areas")
	if filepath.Dir(got[0]) != wantPrefix {
		t.Errorf("expected include rooted under %s, got %s", wantPrefix, got[0])
	}
}

func TestValidateVaultIndexingScope(t *testing.T) {
	for _, ok := range []string{"", VaultScopeMnemoOnly, VaultScopeFull, VaultScopeIncludes} {
		c := Config{VaultIndexingScope: ok}
		if err := c.validateVaultIndexingScope(); err != nil {
			t.Errorf("%q must validate, got %v", ok, err)
		}
	}
	c := Config{VaultIndexingScope: "garbage"}
	if err := c.validateVaultIndexingScope(); err == nil {
		t.Errorf("garbage must fail validation")
	}
}
