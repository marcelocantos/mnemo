// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"testing"
)

// TestEffectiveHomeRespectsMNEMOHOME verifies the 🎯T73 isolation
// invariant: when MNEMO_HOME is set, EffectiveHome returns it
// verbatim, bypassing os.UserHomeDir entirely. This is the structural
// guarantee that a test daemon launched with MNEMO_HOME=<tempdir>
// cannot read or mutate the real $HOME-rooted state.
func TestEffectiveHomeRespectsMNEMOHOME(t *testing.T) {
	want := t.TempDir()
	t.Setenv(MnemoHomeEnv, want)
	got, err := EffectiveHome()
	if err != nil {
		t.Fatalf("EffectiveHome: %v", err)
	}
	if got != want {
		t.Errorf("EffectiveHome under MNEMO_HOME=%q: got %q want %q", want, got, want)
	}
}

func TestEffectiveHomeFallsBackToUserHomeDir(t *testing.T) {
	// Explicitly unset to ensure the fallback path runs.
	t.Setenv(MnemoHomeEnv, "")
	if err := os.Unsetenv(MnemoHomeEnv); err != nil {
		t.Fatalf("unset: %v", err)
	}
	got, err := EffectiveHome()
	if err != nil {
		t.Fatalf("EffectiveHome: %v", err)
	}
	osHome, _ := os.UserHomeDir()
	if got != osHome {
		t.Errorf("EffectiveHome without MNEMO_HOME: got %q want %q (os.UserHomeDir)", got, osHome)
	}
}

func TestResolveHomeForEmptyUsernameRespectsMNEMOHOME(t *testing.T) {
	// ResolveHomeFor("") is the default-identity path. It must
	// route through EffectiveHome so the empty-username case is
	// MNEMO_HOME-overridable.
	want := t.TempDir()
	t.Setenv(MnemoHomeEnv, want)
	got, err := ResolveHomeFor("")
	if err != nil {
		t.Fatalf("ResolveHomeFor(\"\"): %v", err)
	}
	if got != want {
		t.Errorf("ResolveHomeFor(\"\") under MNEMO_HOME=%q: got %q want %q", want, got, want)
	}
}

func TestResolveHomeForProcessOwnerRespectsMNEMOHOME(t *testing.T) {
	// Critical isolation invariant: when the daemon's eager-start
	// path passes the resolved CurrentUsername() (e.g. "marcelo")
	// to ResolveHomeFor, MNEMO_HOME must still win — otherwise the
	// daemon under a test snapshot reads the real $HOME/.mnemo/mnemo.db
	// and the isolation is gone.
	cur, err := CurrentUsername()
	if err != nil {
		t.Skipf("CurrentUsername unavailable: %v", err)
	}
	want := t.TempDir()
	t.Setenv(MnemoHomeEnv, want)
	got, err := ResolveHomeFor(cur)
	if err != nil {
		t.Fatalf("ResolveHomeFor(%q): %v", cur, err)
	}
	if got != want {
		t.Errorf("ResolveHomeFor(%q) under MNEMO_HOME=%q: got %q want %q (real-home leak)",
			cur, want, got, want)
	}
}
