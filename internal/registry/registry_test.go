// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

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

// TestReloadVaultFailureReportedAsWarning runs Reload with a
// vault_path that points at a regular file. vault.New's MkdirAll
// fails, and Reload must NOT classify vault_path as Adopted — instead
// the failure surfaces in Warnings so the MCP caller sees that
// although the on-disk config now reflects the new value, no vault is
// actually active.
func TestReloadVaultFailureReportedAsWarning(t *testing.T) {
	projectDir := t.TempDir()
	s := storetest.NewStore(t, projectDir)

	// Create a regular file at the target path. MkdirAll on an
	// existing regular-file path fails with ENOTDIR on Unix /
	// equivalent on Windows.
	badPath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badPath, []byte("blocking"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := NewRegistry(context.Background(), store.Config{}, "")
	defer r.Close()
	r.mu.Lock()
	r.stores["u"] = &userEntry{store: s, homeDir: t.TempDir()}
	r.mu.Unlock()

	report := r.Reload(store.Config{VaultPath: badPath})

	if len(report.Warnings) == 0 {
		t.Fatalf("expected a warning for failed vault.New, got none. report=%+v", report)
	}
	for _, a := range report.Adopted {
		if a == "vault_path" {
			t.Errorf("vault_path should NOT be Adopted when swap failed; report=%+v", report)
		}
	}
	if len(report.Changed) == 0 || report.Changed[0] != "vault_path" {
		t.Errorf("vault_path should still be Changed even on adoption failure; report=%+v", report)
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

// TestStartVaultWorkersReturnsVctx exercises the contract that
// startVaultWorkers returns the vault sub-context (so swapVault can
// pipe its post-reload sync through vctx instead of r.baseCtx).
// Pre-fix the post-reload sync used r.baseCtx; a cascaded reload
// (A→B→C) would force C to block on B-sync's natural completion even
// though B's vault had been decommissioned. Cancelling the returned
// context here is equivalent to swapVault's next-swap oldCancel(): if
// the contract holds, Done fires immediately.
func TestStartVaultWorkersReturnsVctx(t *testing.T) {
	projectDir := t.TempDir()
	s := storetest.NewStore(t, projectDir)

	dir := filepath.Join(t.TempDir(), "v")
	exp, err := vault.New(s, dir)
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	r := NewRegistry(context.Background(), store.Config{VaultPath: dir}, "")
	defer r.Close()

	e := &userEntry{store: s, vault: exp, homeDir: t.TempDir()}
	r.mu.Lock()
	vctx := r.startVaultWorkers("u", e)
	r.mu.Unlock()
	if vctx == nil {
		t.Fatal("startVaultWorkers must return a non-nil context when vault is set")
	}

	// Drive vctx through e.vaultCancel: this is the exact path
	// swapVault uses (oldCancel()), so it proves vctx is the same
	// context that the next swap will cancel.
	e.vaultCancel()
	select {
	case <-vctx.Done():
	default:
		t.Errorf("vctx returned from startVaultWorkers must be cancelled when e.vaultCancel runs")
	}
	e.vaultWorkers.Wait()

	// Disabled-vault path: returned context is nil and no workers
	// run.
	empty := &userEntry{store: s, homeDir: t.TempDir()}
	r.mu.Lock()
	got := r.startVaultWorkers("u2", empty)
	r.mu.Unlock()
	if got != nil {
		t.Errorf("startVaultWorkers should return nil when vault is unset")
	}
}

// TestCloseAcquiresReloadMu pins down the Close/Reload ordering: Close
// must not return while a swapVault is mid-flight, otherwise the
// re-entry that spawns new vault workers would race against the Store
// being closed. The deterministic surface: with the guard in place,
// Close blocks until a hostile no-op Reload that holds reloadMu
// releases. Without the guard, Close returns instantly.
func TestCloseAcquiresReloadMu(t *testing.T) {
	r := NewRegistry(context.Background(), store.Config{}, "")

	// Grab reloadMu manually to simulate an in-flight Reload that
	// has not yet released it. swapVault's first half holds it via
	// Reload's defer.
	r.reloadMu.Lock()

	done := make(chan struct{})
	go func() {
		r.Close()
		close(done)
	}()

	// If Close ignored reloadMu it would race past us and finish
	// almost immediately. Give it a generous head start, then assert
	// it is still blocked.
	select {
	case <-done:
		t.Fatal("Close returned while reloadMu was held — the Close/Reload race guard is missing")
	case <-time.After(50 * time.Millisecond):
	}

	r.reloadMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after reloadMu released")
	}
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
