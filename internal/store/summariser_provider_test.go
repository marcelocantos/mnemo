// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
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

func isolateProviderEnv(t *testing.T, pathDir string) {
	t.Helper()
	// Empty HOME so known install dirs under ~/.grok and ~/.local do not
	// find real host binaries.
	t.Setenv("HOME", pathDir)
	t.Setenv("PATH", pathDir)
	t.Setenv("GROK_BIN", "")
	t.Setenv("CLAUDE_BIN", "")
	// Unset via empty may leave LookPath finding brew if PATH is wrong —
	// pathDir-only PATH is the isolation.
}

func TestResolveSummariserProviderAutoPrefersGrokWhenPresent(t *testing.T) {
	dir := t.TempDir()
	isolateProviderEnv(t, dir)
	grok := filepath.Join(dir, "grok")
	if err := os.WriteFile(grok, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	p, src := ResolveSummariserProvider("")
	if p != "grok" || src != "auto:grok" {
		t.Fatalf("want auto grok, got %q %q", p, src)
	}
}

func TestResolveSummariserProviderAutoFallsBackToClaude(t *testing.T) {
	dir := t.TempDir()
	isolateProviderEnv(t, dir)
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

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
