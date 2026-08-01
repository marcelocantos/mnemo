// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSignal creates a profile signal file at rel under root with the
// given mtime.
func writeSignal(t *testing.T, root, rel string, mtime time.Time) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if err := os.Chtimes(abs, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", rel, err)
	}
}

func TestDetectVaultProfile(t *testing.T) {
	obsSig := filepath.Join(".obsidian", "workspace.json")
	logSig := filepath.Join("logseq", "config", "config.edn")
	foamSig := filepath.Join(".foam", "settings.json")

	t.Run("config override wins", func(t *testing.T) {
		root := t.TempDir()
		writeSignal(t, root, logSig, time.Now())
		c := Config{VaultProfile: VaultProfileObsidian}
		d := c.DetectVaultProfile(root)
		if d.Profile != VaultProfileObsidian || d.Source != "config" {
			t.Fatalf("got %+v, want obsidian/config", d)
		}
	})

	t.Run("no signals -> generic default", func(t *testing.T) {
		d := Config{}.DetectVaultProfile(t.TempDir())
		if d.Profile != VaultProfileGeneric || d.Source != "default" {
			t.Fatalf("got %+v, want generic/default", d)
		}
	})

	t.Run("empty path -> generic default", func(t *testing.T) {
		d := Config{}.DetectVaultProfile("")
		if d.Profile != VaultProfileGeneric || d.Source != "default" {
			t.Fatalf("got %+v, want generic/default", d)
		}
	})

	t.Run("single signal -> that profile", func(t *testing.T) {
		root := t.TempDir()
		writeSignal(t, root, foamSig, time.Now())
		d := Config{}.DetectVaultProfile(root)
		if d.Profile != VaultProfileFoam || d.Source != "auto" || d.SignalFile != foamSig {
			t.Fatalf("got %+v, want foam/auto/%s", d, foamSig)
		}
	})

	t.Run("most recent mtime wins beyond tie window", func(t *testing.T) {
		root := t.TempDir()
		now := time.Now()
		writeSignal(t, root, obsSig, now.Add(-48*time.Hour))
		writeSignal(t, root, logSig, now) // clearly newer
		d := Config{}.DetectVaultProfile(root)
		if d.Profile != VaultProfileLogseq {
			t.Fatalf("got %q, want logseq (newest)", d.Profile)
		}
		if len(d.Alternatives) != 1 || d.Alternatives[0] != VaultProfileObsidian {
			t.Fatalf("alternatives = %v, want [obsidian]", d.Alternatives)
		}
	})

	t.Run("within tie window -> obsidian preferred over logseq", func(t *testing.T) {
		root := t.TempDir()
		now := time.Now()
		// logseq slightly newer but inside the 1h tie window.
		writeSignal(t, root, obsSig, now.Add(-10*time.Minute))
		writeSignal(t, root, logSig, now)
		d := Config{}.DetectVaultProfile(root)
		if d.Profile != VaultProfileObsidian {
			t.Fatalf("got %q, want obsidian (tie-break)", d.Profile)
		}
	})

	t.Run("within tie window -> logseq preferred over foam", func(t *testing.T) {
		root := t.TempDir()
		now := time.Now()
		writeSignal(t, root, foamSig, now)
		writeSignal(t, root, logSig, now.Add(-5*time.Minute))
		d := Config{}.DetectVaultProfile(root)
		if d.Profile != VaultProfileLogseq {
			t.Fatalf("got %q, want logseq (tie-break over foam)", d.Profile)
		}
	})
}

func TestValidateVaultProfile(t *testing.T) {
	for _, p := range []string{"", "obsidian", "logseq", "foam", "generic"} {
		if err := (Config{VaultProfile: p}).validateVaultProfile(); err != nil {
			t.Errorf("profile %q rejected: %v", p, err)
		}
	}
	if err := (Config{VaultProfile: "notion"}).validateVaultProfile(); err == nil {
		t.Error("expected invalid profile to be rejected")
	}
}
