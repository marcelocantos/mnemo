// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveSummariserProviderExplicit(t *testing.T) {
	p, src := ResolveSummariserProvider("claude")
	if p != "claude" || src != "config" {
		t.Fatalf("got %q %q", p, src)
	}
	p, src = ResolveSummariserProvider("grok")
	if p != "grok" || src != "config" {
		t.Fatalf("got %q %q", p, src)
	}
	p, src = ResolveSummariserProvider("anthropic")
	if p != "claude" || src != "config" {
		t.Fatalf("alias anthropic: %q %q", p, src)
	}
}

// isolateProviderEnv points HOME/USERPROFILE and PATH at dir so LookPath
// and known-install candidates cannot see host binaries.
func isolateProviderEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOMEDRIVE", filepath.VolumeName(dir))
	t.Setenv("HOMEPATH", dir)
	t.Setenv("PATH", dir)
	t.Setenv("GROK_BIN", "")
	t.Setenv("CLAUDE_BIN", "")
}

func writeFakeCLI(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name = name + ".exe"
	}
	p := filepath.Join(dir, name)
	// Minimal content; resolve only Stats / LookPath, does not exec.
	if err := os.WriteFile(p, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveSummariserProviderAutoPrefersGrokWhenPresent(t *testing.T) {
	dir := t.TempDir()
	isolateProviderEnv(t, dir)
	writeFakeCLI(t, dir, "grok")

	p, src := ResolveSummariserProvider("")
	if p != "grok" || src != "auto:grok" {
		t.Fatalf("want auto grok, got %q %q", p, src)
	}
}

func TestResolveSummariserProviderAutoFallsBackToClaude(t *testing.T) {
	dir := t.TempDir()
	isolateProviderEnv(t, dir)
	writeFakeCLI(t, dir, "claude")

	p, src := ResolveSummariserProvider("auto")
	if p != "claude" || src != "auto:claude" {
		t.Fatalf("want auto claude, got %q %q", p, src)
	}
}

func TestResolveSummariserProviderNeitherBinary(t *testing.T) {
	dir := t.TempDir()
	isolateProviderEnv(t, dir)

	p, src := ResolveSummariserProvider("")
	if p != "claude" || src != "auto:none" {
		t.Fatalf("want claude auto:none, got %q %q", p, src)
	}
}

func TestValidateSummariserAllowsAuto(t *testing.T) {
	if err := (Config{}).validateSummariser(); err != nil {
		t.Fatal(err)
	}
	if err := (Config{Summariser: SummariserConfig{Provider: "auto"}}).validateSummariser(); err != nil {
		t.Fatal(err)
	}
}
