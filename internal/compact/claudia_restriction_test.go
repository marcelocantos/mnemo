// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"testing"

	"github.com/marcelocantos/claudia"
)

// TestCompactorUsesGrokProvider: mnemo's default summariser backend is
// Grok via claudia ProviderGrok (not Claude Code).
func TestCompactorUsesGrokProvider(t *testing.T) {
	cfg := NewClaudiaCaller("/tmp/x", "").taskConfig()
	if cfg.Provider != claudia.ProviderGrok {
		t.Fatalf("provider=%q want %q", cfg.Provider, claudia.ProviderGrok)
	}
	if cfg.Model != DefaultSummariserModel {
		t.Fatalf("model=%q want %q", cfg.Model, DefaultSummariserModel)
	}
	// Grok Task refuses DisallowTools in claudia v0.22 — must stay empty
	// so Run does not fail closed before spawn.
	if len(cfg.DisallowTools) != 0 {
		t.Fatalf("Grok task must not set DisallowTools (claudia refuses); got %v", cfg.DisallowTools)
	}
}

// TestClaudePathStillStripsTools: if someone forces ProviderClaude, the
// historical tool strip must remain for the 🎯T139 incident class.
func TestClaudePathStillStripsTools(t *testing.T) {
	c := &ClaudiaCaller{workDir: "/tmp/x", model: "sonnet", provider: claudia.ProviderClaude}
	cfg := c.taskConfig()
	if len(cfg.DisallowTools) == 0 {
		t.Fatal("Claude summariser path must still DisallowTools")
	}
}
