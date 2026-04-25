// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package targets

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const sampleYAML = `schema_version: 2
targets:
  T1:
    name: Triad integration
    status: identified
    depends_on:
    - T1.1
  T1.1:
    name: Executable acceptance checks
    status: achieved
  T1.4:
    name: Target-aware context compaction
    status: identified
  T2:
    name: Cross-repo dependency edges
    status: achieved
  T3:
    name: Protocol app priority sync
    status: identified
    depends_on:
    - T3.1
  T3.1:
    name: Priorities table
    status: identified
`

func writeBullseye(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", FileName, err)
	}
}

func TestLoadFromCWD_FoundAtRoot(t *testing.T) {
	dir := t.TempDir()
	writeBullseye(t, dir, sampleYAML)

	state, err := LoadFromCWD(dir)
	if err != nil {
		t.Fatalf("LoadFromCWD: %v", err)
	}
	if state == nil {
		t.Fatal("state is nil")
	}
	if state.RepoRoot != dir {
		t.Errorf("RepoRoot = %q, want %q", state.RepoRoot, dir)
	}
	if len(state.All) != 6 {
		t.Errorf("All = %d targets, want 6", len(state.All))
	}
	if len(state.Achieved) != 2 {
		t.Errorf("Achieved = %d, want 2 (T1.1, T2)", len(state.Achieved))
	}
	if len(state.Active) != 4 {
		t.Errorf("Active = %d, want 4 (T1, T1.4, T3, T3.1)", len(state.Active))
	}
}

func TestLoadFromCWD_WalksUp(t *testing.T) {
	root := t.TempDir()
	writeBullseye(t, root, sampleYAML)
	deep := filepath.Join(root, "src", "internal", "pkg")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	state, err := LoadFromCWD(deep)
	if err != nil {
		t.Fatalf("LoadFromCWD: %v", err)
	}
	if state == nil {
		t.Fatal("expected state from ancestor walk-up")
	}
	if state.RepoRoot != root {
		t.Errorf("RepoRoot = %q, want %q", state.RepoRoot, root)
	}
}

func TestLoadFromCWD_NoFile(t *testing.T) {
	dir := t.TempDir()
	state, err := LoadFromCWD(dir)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state for missing file, got %+v", state)
	}
}

func TestLoadFromCWD_EmptyCWD(t *testing.T) {
	state, err := LoadFromCWD("")
	if err != nil {
		t.Fatalf("empty cwd: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state for empty cwd, got %+v", state)
	}
}

func TestLoadFromCWD_Malformed(t *testing.T) {
	dir := t.TempDir()
	writeBullseye(t, dir, "schema_version: 2\ntargets: : ::\n")

	_, err := LoadFromCWD(dir)
	if err == nil {
		t.Fatal("expected parse error for malformed YAML")
	}
}

func TestComputeFrontier(t *testing.T) {
	dir := t.TempDir()
	writeBullseye(t, dir, sampleYAML)

	state, err := LoadFromCWD(dir)
	if err != nil {
		t.Fatalf("LoadFromCWD: %v", err)
	}

	got := append([]string(nil), state.FrontierIDs...)
	sort.Strings(got)
	// T1 blocked by T1.1 (achieved → unblocks); but T1 also has T1.4
	// reachable via the parent chain in the real schema. Here T1 only
	// declares depends_on:[T1.1] (achieved), so T1 IS frontier.
	// T1.4 has no deps → frontier.
	// T3 depends on T3.1 (active) → blocked.
	// T3.1 has no deps → frontier.
	want := []string{"T1", "T1.4", "T3.1"}
	if !equalStrings(got, want) {
		t.Errorf("FrontierIDs = %v, want %v", got, want)
	}
}

func TestSchemaVersion1_Compatible(t *testing.T) {
	dir := t.TempDir()
	v1 := `schema_version: 1
targets:
  T1:
    name: Legacy v1 target
    status: identified
`
	writeBullseye(t, dir, v1)

	state, err := LoadFromCWD(dir)
	if err != nil {
		t.Fatalf("v1 load: %v", err)
	}
	if state == nil || len(state.All) != 1 {
		t.Fatalf("expected 1 target, got %+v", state)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
