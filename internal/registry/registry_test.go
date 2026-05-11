// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
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
