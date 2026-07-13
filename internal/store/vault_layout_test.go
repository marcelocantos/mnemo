// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolvedVaultLayoutExplicit(t *testing.T) {
	for _, want := range []string{VaultLayoutV1, VaultLayoutBoth, VaultLayoutV2} {
		t.Run(want, func(t *testing.T) {
			cfg := Config{VaultLayout: want}
			if got := cfg.ResolvedVaultLayout(t.TempDir()); got != want {
				t.Errorf("explicit layout %q lost: got %q", want, got)
			}
		})
	}
}

func TestResolvedVaultLayoutDefaultsNewVault(t *testing.T) {
	vault := t.TempDir() // empty
	if got := (Config{}).ResolvedVaultLayout(vault); got != VaultLayoutV2 {
		t.Errorf("empty vault should default to %q, got %q", VaultLayoutV2, got)
	}
}

func TestResolvedVaultLayoutDefaultsV1Vault(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, "sessions"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := (Config{}).ResolvedVaultLayout(vault); got != VaultLayoutBoth {
		t.Errorf("v1 vault should default to %q, got %q", VaultLayoutBoth, got)
	}
}

func TestResolvedVaultLayoutDefaultsV2VaultWhenMnemoPresent(t *testing.T) {
	vault := t.TempDir()
	// _mnemo/ present overrides v1-marker presence — already on the wing.
	if err := os.MkdirAll(filepath.Join(vault, "_mnemo"), 0o755); err != nil {
		t.Fatalf("seed _mnemo: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(vault, "sessions"), 0o755); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	if got := (Config{}).ResolvedVaultLayout(vault); got != VaultLayoutV2 {
		t.Errorf("v2-with-leftovers vault should default to %q, got %q", VaultLayoutV2, got)
	}
}

func TestResolvedVaultLayoutEmptyPath(t *testing.T) {
	if got := (Config{}).ResolvedVaultLayout(""); got != VaultLayoutV2 {
		t.Errorf("empty path should default to %q, got %q", VaultLayoutV2, got)
	}
}

func TestResolvedVaultLayoutSoakWarnAfterDefault(t *testing.T) {
	if got := (Config{}).ResolvedVaultLayoutSoakWarnAfter(); got != defaultVaultLayoutSoakWarnAfter {
		t.Errorf("default soak: got %v want %v", got, defaultVaultLayoutSoakWarnAfter)
	}
}

func TestResolvedVaultLayoutSoakWarnAfterParsed(t *testing.T) {
	cfg := Config{VaultLayoutSoakWarnAfter: "48h"}
	if got := cfg.ResolvedVaultLayoutSoakWarnAfter(); got != 48*time.Hour {
		t.Errorf("parsed soak: got %v want %v", got, 48*time.Hour)
	}
}

func TestResolvedVaultLayoutSoakWarnAfterFallsBackOnGarbage(t *testing.T) {
	// Unparseable and non-positive both fall back to the default —
	// a corrupted config never silently disables the warning.
	cases := []string{"banana", "0s", "-1h"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			cfg := Config{VaultLayoutSoakWarnAfter: in}
			if got := cfg.ResolvedVaultLayoutSoakWarnAfter(); got != defaultVaultLayoutSoakWarnAfter {
				t.Errorf("got %v want default %v", got, defaultVaultLayoutSoakWarnAfter)
			}
		})
	}
}

func TestValidateVaultLayoutAcceptsKnown(t *testing.T) {
	for _, v := range []string{"", VaultLayoutV1, VaultLayoutBoth, VaultLayoutV2} {
		if err := (Config{VaultLayout: v}).validateVaultLayout(); err != nil {
			t.Errorf("validate(%q) unexpected error: %v", v, err)
		}
	}
}

func TestValidateVaultLayoutRejectsUnknown(t *testing.T) {
	cases := []string{"v3", "Both", "V2", "vault2"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			if err := (Config{VaultLayout: v}).validateVaultLayout(); err == nil {
				t.Errorf("validate(%q) accepted; want error", v)
			}
		})
	}
}

func TestValidateVaultLayoutRejectsBadSoak(t *testing.T) {
	cases := []string{"banana", "0s", "-1h"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			cfg := Config{VaultLayoutSoakWarnAfter: v}
			if err := cfg.validateVaultLayout(); err == nil {
				t.Errorf("soak %q accepted; want error", v)
			}
		})
	}
}
