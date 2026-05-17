// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExclusionRegistry_ExactPath(t *testing.T) {
	r := &exclusionRegistry{}
	r.register("/tmp/vault", "vault_path")

	if !r.isExcluded("/tmp/vault") {
		t.Errorf("exact path not matched")
	}
	if r.isExcluded("/tmp/other") {
		t.Errorf("unrelated path matched")
	}
}

func TestExclusionRegistry_DescendantPath(t *testing.T) {
	r := &exclusionRegistry{}
	r.register("/tmp/vault", "vault_path")

	for _, p := range []string{
		"/tmp/vault/sessions",
		"/tmp/vault/sessions/2026/note.md",
		"/tmp/vault/deeply/nested/file.txt",
	} {
		if !r.isExcluded(p) {
			t.Errorf("descendant %q not matched", p)
		}
	}
}

func TestExclusionRegistry_SiblingPath(t *testing.T) {
	r := &exclusionRegistry{}
	r.register("/tmp/vault", "vault_path")

	// /tmp/vault-other is a sibling, not a descendant. The naive
	// prefix-match implementation would match this; filepath.Rel
	// must distinguish.
	if r.isExcluded("/tmp/vault-other") {
		t.Errorf("sibling /tmp/vault-other should not match /tmp/vault")
	}
	if r.isExcluded("/tmp/vault-other/foo") {
		t.Errorf("descendant of sibling should not match")
	}
}

func TestExclusionRegistry_Idempotent(t *testing.T) {
	r := &exclusionRegistry{}
	r.register("/tmp/vault", "vault_path")
	r.register("/tmp/vault", "vault_path")
	r.register("/tmp/vault", "different_reason")

	if got := len(r.entries); got != 1 {
		t.Errorf("expected idempotent registration, got %d entries", got)
	}
}

func TestExclusionRegistry_EmptyPath(t *testing.T) {
	r := &exclusionRegistry{}
	r.register("", "ignored")
	if got := len(r.entries); got != 0 {
		t.Errorf("empty path should be ignored, got %d entries", got)
	}
	if r.isExcluded("") {
		t.Errorf("empty query path should not match")
	}
}

func TestExclusionRegistry_NotExcluded(t *testing.T) {
	r := &exclusionRegistry{}
	r.register("/tmp/vault", "vault_path")

	for _, p := range []string{
		"/tmp",              // ancestor of excluded
		"/var/vault",        // different root
		"/",                 // filesystem root
		"/tmp/another/path", // unrelated subtree
	} {
		if r.isExcluded(p) {
			t.Errorf("path %q should not be excluded", p)
		}
	}
}

func TestExclusionRegistry_RelativePath(t *testing.T) {
	// Registering a relative path canonicalises it via filepath.Abs,
	// so the registry treats relative and absolute forms of the same
	// directory equivalently.
	tmp := t.TempDir()
	vault := filepath.Join(tmp, "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}

	r := &exclusionRegistry{}
	r.register(vault, "vault_path")

	if !r.isExcluded(vault) {
		t.Errorf("absolute path lookup failed")
	}
}

func TestExclusionRegistry_Symlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks on Windows need elevated privileges in CI")
	}

	// Symlink scenario: vault is at /tmp/<test>/real/vault, and there
	// is a symlink /tmp/<test>/alias → /tmp/<test>/real. Registering
	// either form should cause the walker to skip when it encounters
	// the other form. Canonicalisation via EvalSymlinks unifies them.
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	realVault := filepath.Join(realDir, "vault")
	if err := os.MkdirAll(realVault, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	aliasVault := filepath.Join(alias, "vault")

	t.Run("register-via-alias-check-via-real", func(t *testing.T) {
		r := &exclusionRegistry{}
		r.register(aliasVault, "vault_path")
		if !r.isExcluded(realVault) {
			t.Errorf("real path not excluded after registering via symlink alias")
		}
	})

	t.Run("register-via-real-check-via-alias", func(t *testing.T) {
		r := &exclusionRegistry{}
		r.register(realVault, "vault_path")
		if !r.isExcluded(aliasVault) {
			t.Errorf("alias path not excluded after registering via real path")
		}
	})
}

func TestExclusionRegistry_NonexistentPath(t *testing.T) {
	// vault_path is configurable; the directory may not exist at
	// registration time (mnemo creates it later). Canonicalisation
	// must not fail; the registry must still match later lookups.
	tmp := t.TempDir()
	vault := filepath.Join(tmp, "not-yet-created", "vault")

	r := &exclusionRegistry{}
	r.register(vault, "vault_path")
	if !r.isExcluded(vault) {
		t.Errorf("non-existent path not matched at registration time")
	}
	if !r.isExcluded(filepath.Join(vault, "child")) {
		t.Errorf("descendant of non-existent path not matched")
	}
}

func TestExclusionRegistry_LazyRecanonicalise(t *testing.T) {
	// A vault registered before its symlink target exists must still
	// match once the symlink is created. This exercises the lazy
	// re-canonicalisation in isExcluded.
	if runtime.GOOS == "windows" {
		t.Skip("symlinks on Windows need elevated privileges in CI")
	}
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	alias := filepath.Join(tmp, "alias")

	r := &exclusionRegistry{}
	r.register(alias, "vault_path") // register before the symlink exists

	// Now create the alias → real symlink.
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}

	// Lookup via the real path should now match (lazy re-canonicalisation
	// resolves the alias to real at check time).
	if !r.isExcluded(realDir) {
		t.Errorf("lazy re-canonicalisation failed: real path not excluded after symlink created")
	}
}

// TestStore_IsExcluded verifies the Store-level wrappers delegate to
// the underlying registry. This is what walkers actually call.
func TestStore_IsExcluded(t *testing.T) {
	s := &Store{exclusions: &exclusionRegistry{}}
	s.RegisterExcludedPath("/tmp/vault", "vault_path")

	if !s.IsExcluded("/tmp/vault/sessions") {
		t.Errorf("Store.IsExcluded did not delegate correctly")
	}
}

// TestDiscoverRepos_HonoursExclusion verifies the workspace walker
// skips excluded subtrees — covers the vault-in-workspace-root case
// where the vault happens to contain a .git (rare, but possible).
func TestDiscoverRepos_HonoursExclusion(t *testing.T) {
	root := t.TempDir()
	realRepo := filepath.Join(root, "github.com", "org", "real")
	if err := os.MkdirAll(filepath.Join(realRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A vault sitting under the workspace root that happens to
	// contain a .git would otherwise be reported as a repo.
	vault := filepath.Join(root, "vault")
	if err := os.MkdirAll(filepath.Join(vault, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &exclusionRegistry{}
	r.register(vault, "vault_path")

	got := discoverRepos([]string{root}, r.isExcluded)
	if len(got) != 1 {
		t.Fatalf("expected 1 repo, got %d: %v", len(got), got)
	}
	if got[0] != realRepo {
		t.Errorf("expected real repo %q, got %q", realRepo, got[0])
	}
}
