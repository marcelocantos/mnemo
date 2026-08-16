// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelocantos/claudia"
)

func TestSummariserGrokFromOpts(t *testing.T) {
	cfg := NewClaudiaSummariser(ClaudiaSummariserOpts{
		WorkDir: "/tmp/x", Provider: "grok",
	}).(*claudiaSummariser).taskConfig()
	if cfg.Provider != claudia.ProviderGrok {
		t.Fatalf("provider=%q", cfg.Provider)
	}
	if len(cfg.DisallowTools) != 0 {
		t.Fatalf("Grok DisallowTools=%v", cfg.DisallowTools)
	}
}

func TestSummariserClaudeStripsTools(t *testing.T) {
	cfg := NewClaudiaSummariser(ClaudiaSummariserOpts{
		WorkDir: "/tmp/x", Provider: "claude",
	}).(*claudiaSummariser).taskConfig()
	if cfg.Provider != claudia.ProviderClaude {
		t.Fatalf("provider=%q", cfg.Provider)
	}
	if len(cfg.DisallowTools) == 0 {
		t.Fatal("Claude path must DisallowTools")
	}
}

func TestSpendCeilingStopsRatherThanRetries(t *testing.T) {
	c := &claudiaSummariser{workDir: "/tmp/x", ceiling: 1000}
	c.inTokens, c.outTok = 900, 200
	_, err := c.Ask(context.Background(), "another drip")
	if !errors.Is(err, ErrSpendCeiling) {
		t.Fatalf("expected ErrSpendCeiling, got %v", err)
	}
}

func TestSpendCeilingIsOnByDefault(t *testing.T) {
	c, ok := NewClaudiaSummariser(ClaudiaSummariserOpts{WorkDir: "/tmp/x"}).(*claudiaSummariser)
	if !ok {
		t.Fatal("unexpected type")
	}
	if c.ceiling <= 0 {
		t.Error("no default spend ceiling")
	}
}
