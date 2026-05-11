// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
	"github.com/marcelocantos/mnemo/internal/vault"
)

// TestReloadReportClassifiesFields validates the diff logic without
// requiring any per-user Store to be initialised. A Registry with no
// stores still produces a meaningful Changed/Adopted/RequiresRestart
// report — the live-adoption pass simply has no entries to iterate.
func TestReloadReportClassifiesFields(t *testing.T) {
	old := store.Config{
		WorkspaceRoots:   []string{"/old/ws"},
		ExtraProjectDirs: []string{"/old/proj"},
		SynthesisRoots:   []string{"/old/syn"},
		VaultPath:        "/old/vault",
	}
	r := NewRegistry(context.Background(), old, "")
	defer r.Close()

	newCfg := store.Config{
		WorkspaceRoots:   []string{"/new/ws"},
		ExtraProjectDirs: []string{"/old/proj"}, // unchanged
		SynthesisRoots:   []string{"/new/syn"},
		VaultPath:        "/new/vault",
	}
	report := r.Reload(newCfg)

	sort.Strings(report.Changed)
	sort.Strings(report.Adopted)
	wantChanged := []string{"synthesis_roots", "vault_path", "workspace_roots"}
	if !reflect.DeepEqual(report.Changed, wantChanged) {
		t.Errorf("Changed: got %v want %v", report.Changed, wantChanged)
	}
	wantAdopted := wantChanged // all three classify as live-adoptable
	if !reflect.DeepEqual(report.Adopted, wantAdopted) {
		t.Errorf("Adopted: got %v want %v", report.Adopted, wantAdopted)
	}
	if len(report.RequiresRestart) != 0 {
		t.Errorf("RequiresRestart: got %v want []", report.RequiresRestart)
	}
	if got := r.CurrentConfig().VaultPath; got != "/new/vault" {
		t.Errorf("CurrentConfig().VaultPath: got %q want /new/vault", got)
	}
}

func TestReloadFlagsLinkedInstancesAsRestart(t *testing.T) {
	old := store.Config{LinkedInstances: nil}
	r := NewRegistry(context.Background(), old, "")
	defer r.Close()

	newCfg := store.Config{
		LinkedInstances: []store.LinkedInstance{
			{Name: "alice", URL: "https://x/", PeerCert: "alice"},
		},
	}
	report := r.Reload(newCfg)
	if len(report.RequiresRestart) != 1 || report.RequiresRestart[0] != "linked_instances" {
		t.Errorf("RequiresRestart: got %v want [linked_instances]", report.RequiresRestart)
	}
	for _, a := range report.Adopted {
		if a == "linked_instances" {
			t.Errorf("linked_instances should not be in Adopted: %v", report.Adopted)
		}
	}
}

func TestReloadNoOpWhenConfigUnchanged(t *testing.T) {
	cfg := store.Config{VaultPath: "/v", WorkspaceRoots: []string{"/w"}}
	r := NewRegistry(context.Background(), cfg, "")
	defer r.Close()

	report := r.Reload(cfg)
	if len(report.Changed) != 0 {
		t.Errorf("Changed should be empty, got %v", report.Changed)
	}
}

// TestSwapVaultEndToEnd builds a Registry around a real Store +
// Exporter, runs swapVault, and asserts the new exporter is wired and
// the old workers are gone. Regression for the prior deadlock in
// startVaultWorkers re-acquiring r.mu while ForUser still owned it,
// and for the concurrent-swap race that left orphaned workers.
func TestSwapVaultEndToEnd(t *testing.T) {
	projectDir := t.TempDir()
	s := storetest.NewStore(t, projectDir)

	oldDir := filepath.Join(t.TempDir(), "old-vault")
	newDir := filepath.Join(t.TempDir(), "new-vault")
	oldExp, err := vault.New(s, oldDir)
	if err != nil {
		t.Fatalf("vault.New old: %v", err)
	}

	r := NewRegistry(context.Background(), store.Config{VaultPath: oldDir}, "")
	defer r.Close()

	e := &userEntry{store: s, vault: oldExp, homeDir: t.TempDir()}

	// Simulate the ForUser path: caller acquires r.mu, calls
	// startVaultWorkers under the lock, releases. Pre-fix this
	// deadlocked because startVaultWorkers re-acquired r.mu.
	r.mu.Lock()
	r.startVaultWorkers("u", e)
	r.mu.Unlock()
	if e.vaultCancel == nil {
		t.Fatal("vaultCancel not set after startVaultWorkers")
	}

	// Swap to the new path. swapVault should: stop old workers,
	// build a new exporter at newDir, and start fresh workers.
	r.swapVault("u", e, newDir)
	r.mu.Lock()
	gotPath := ""
	if e.vault != nil {
		gotPath = e.vault.Path()
	}
	hasCancel := e.vaultCancel != nil
	r.mu.Unlock()

	if gotPath != newDir {
		t.Errorf("vault path after swap: got %q want %q", gotPath, newDir)
	}
	if !hasCancel {
		t.Errorf("vaultCancel should be set after swap to non-empty path")
	}

	// Swap to disabled (empty path). Workers should drain and exporter
	// clear.
	r.swapVault("u", e, "")
	r.mu.Lock()
	if e.vault != nil {
		t.Errorf("vault should be nil after swap to empty path, got %v", e.vault)
	}
	if e.vaultCancel != nil {
		t.Errorf("vaultCancel should be nil after disable")
	}
	r.mu.Unlock()
}

// TestReloadSerialisesConcurrentSwaps fires multiple Reload calls in
// parallel and asserts the final state matches the last write under
// reloadMu. Without reloadMu serialisation, two concurrent swapVault
// flows could leave the registry with orphaned cancel funcs or
// duplicate worker sets per user.
func TestReloadSerialisesConcurrentSwaps(t *testing.T) {
	projectDir := t.TempDir()
	s := storetest.NewStore(t, projectDir)

	r := NewRegistry(context.Background(), store.Config{}, "")
	defer r.Close()

	r.mu.Lock()
	r.stores["u"] = &userEntry{store: s, homeDir: t.TempDir()}
	r.mu.Unlock()

	dirs := []string{
		filepath.Join(t.TempDir(), "a"),
		filepath.Join(t.TempDir(), "b"),
		filepath.Join(t.TempDir(), "c"),
	}

	var wg sync.WaitGroup
	for _, d := range dirs {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			r.Reload(store.Config{VaultPath: path})
		}(d)
	}
	wg.Wait()

	r.mu.Lock()
	finalPath := ""
	if r.stores["u"].vault != nil {
		finalPath = r.stores["u"].vault.Path()
	}
	r.mu.Unlock()

	// Final path must be one of the dirs (whichever Reload won the
	// reloadMu race last), not a partial/empty state.
	matched := false
	for _, d := range dirs {
		if finalPath == d {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("final vault path %q must be one of the concurrent reload inputs %v", finalPath, dirs)
	}
}
