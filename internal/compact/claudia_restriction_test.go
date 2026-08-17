// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"testing"

	"github.com/marcelocantos/claudia"
)

func TestGrokConfigFromOpts(t *testing.T) {
	cfg := NewClaudiaCaller(ClaudiaCallerOpts{
		WorkDir: "/tmp/x", Provider: "grok",
	}).taskConfig()
	if cfg.Provider != claudia.ProviderGrok {
		t.Fatalf("provider=%q", cfg.Provider)
	}
	if cfg.Model != DefaultGrokModel {
		t.Fatalf("model=%q", cfg.Model)
	}
	if len(cfg.DisallowTools) != 0 {
		t.Fatalf("Grok must not set DisallowTools: %v", cfg.DisallowTools)
	}
}

func TestClaudeConfigStripsTools(t *testing.T) {
	cfg := NewClaudiaCaller(ClaudiaCallerOpts{
		WorkDir: "/tmp/x", Provider: "claude", Model: "sonnet",
	}).taskConfig()
	if cfg.Provider != claudia.ProviderClaude {
		t.Fatalf("provider=%q", cfg.Provider)
	}
	if len(cfg.DisallowTools) == 0 {
		t.Fatal("Claude path must DisallowTools")
	}
}

func TestEmptyProviderDefaultsToGrok(t *testing.T) {
	cfg := NewClaudiaCaller(ClaudiaCallerOpts{WorkDir: "/tmp/x"}).taskConfig()
	if cfg.Provider != claudia.ProviderGrok {
		t.Fatalf("default provider=%q", cfg.Provider)
	}
}
