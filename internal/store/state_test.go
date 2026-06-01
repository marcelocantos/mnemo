// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadStateMissingReturnsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState(missing): %v", err)
	}
	if s.Version != StateVersion {
		t.Errorf("fresh version: got %d want %d", s.Version, StateVersion)
	}
	if s.ReadOnly {
		t.Error("fresh state should not be read-only")
	}
	if s.VaultLayoutFirstSeen.V1 != nil || s.VaultLayoutFirstSeen.Both != nil || s.VaultLayoutFirstSeen.V2 != nil {
		t.Error("fresh first-seen should be all nil")
	}
}

func TestStampLayoutFirstSeenIdempotent(t *testing.T) {
	s := NewState()
	first := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if !s.StampLayoutFirstSeen(VaultLayoutBoth, first) {
		t.Fatal("first stamp should return true")
	}
	if got := s.LayoutFirstSeen(VaultLayoutBoth); !got.Equal(first) {
		t.Errorf("stamped time lost: got %v want %v", got, first)
	}
	// A later stamp on the same layout must not overwrite — the
	// first observation wins.
	later := first.Add(48 * time.Hour)
	if s.StampLayoutFirstSeen(VaultLayoutBoth, later) {
		t.Error("repeat stamp should return false")
	}
	if got := s.LayoutFirstSeen(VaultLayoutBoth); !got.Equal(first) {
		t.Errorf("repeat stamp overwrote: got %v want %v", got, first)
	}
}

func TestStampLayoutFirstSeenIndependentPerLayout(t *testing.T) {
	s := NewState()
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	s.StampLayoutFirstSeen(VaultLayoutBoth, t1)
	s.StampLayoutFirstSeen(VaultLayoutV2, t2)
	if got := s.LayoutFirstSeen(VaultLayoutBoth); !got.Equal(t1) {
		t.Errorf("both: %v want %v", got, t1)
	}
	if got := s.LayoutFirstSeen(VaultLayoutV2); !got.Equal(t2) {
		t.Errorf("v2: %v want %v", got, t2)
	}
	if got := s.LayoutFirstSeen(VaultLayoutV1); !got.IsZero() {
		t.Errorf("v1: %v want zero", got)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewState()
	s.VaultPath = "~/Documents/PKM"
	stamp := time.Date(2026, 5, 22, 14, 1, 55, 0, time.UTC)
	s.StampLayoutFirstSeen(VaultLayoutBoth, stamp)
	if err := s.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.VaultPath != s.VaultPath {
		t.Errorf("vault_path round-trip: got %q want %q", loaded.VaultPath, s.VaultPath)
	}
	if got := loaded.LayoutFirstSeen(VaultLayoutBoth); !got.Equal(stamp) {
		t.Errorf("first_seen round-trip: got %v want %v", got, stamp)
	}
}

func TestWritePreservesUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// Seed a state.json with both known and unknown fields.
	seeded := map[string]any{
		"version":   StateVersion,
		"vault_path": "/tmp/v",
		"vault_layout_first_seen": map[string]any{"v1": nil, "both": nil, "v2": nil},
		"future_field_added_in_v2_daemon": "preserve-me",
		"another_unknown": map[string]int{"k": 1},
	}
	raw, err := json.MarshalIndent(seeded, "", "  ")
	if err != nil {
		t.Fatalf("seed marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.Extras["future_field_added_in_v2_daemon"] == nil {
		t.Error("unknown field dropped on load")
	}
	// Write back and confirm unknown fields persist verbatim.
	if err := loaded.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var roundTripped map[string]any
	if err := json.Unmarshal(out, &roundTripped); err != nil {
		t.Fatalf("parse rewritten: %v", err)
	}
	if roundTripped["future_field_added_in_v2_daemon"] != "preserve-me" {
		t.Errorf("unknown field lost on rewrite: %v", roundTripped["future_field_added_in_v2_daemon"])
	}
	if _, ok := roundTripped["another_unknown"]; !ok {
		t.Error("second unknown field lost on rewrite")
	}
}

func TestReadOnlyModeOnNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seeded := map[string]any{
		"version":    StateVersion + 1, // newer than we understand
		"vault_path": "/tmp/v",
	}
	raw, _ := json.MarshalIndent(seeded, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !loaded.ReadOnly {
		t.Fatal("newer version should mark state read-only")
	}
	// Mutate something and Write — file must not be touched.
	originalBytes, _ := os.ReadFile(path)
	loaded.StampLayoutFirstSeen(VaultLayoutBoth, time.Now())
	if err := loaded.Write(path); err != nil {
		t.Fatalf("Write in read-only mode should be no-op, not error: %v", err)
	}
	afterBytes, _ := os.ReadFile(path)
	if string(originalBytes) != string(afterBytes) {
		t.Error("read-only Write mutated the file")
	}
}

func TestResetVaultLayoutFirstSeen(t *testing.T) {
	s := NewState()
	s.StampLayoutFirstSeen(VaultLayoutBoth, time.Now())
	s.StampLayoutFirstSeen(VaultLayoutV1, time.Now())
	s.ResetVaultLayoutFirstSeen()
	if s.VaultLayoutFirstSeen.V1 != nil || s.VaultLayoutFirstSeen.Both != nil || s.VaultLayoutFirstSeen.V2 != nil {
		t.Error("reset should clear all first-seen timestamps")
	}
}
