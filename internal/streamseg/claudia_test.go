// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelocantos/claudia"
)

// TestSummariserUsesGrokProvider: streaming segmenter matches the
// compactor — Grok Task via claudia, no DisallowTools (claudia v0.22).
func TestSummariserUsesGrokProvider(t *testing.T) {
	cfg := (&claudiaSummariser{workDir: "/tmp/x", model: DefaultStreamsegModel}).taskConfig()
	if cfg.Provider != claudia.ProviderGrok {
		t.Fatalf("provider=%q want %q", cfg.Provider, claudia.ProviderGrok)
	}
	if len(cfg.DisallowTools) != 0 {
		t.Fatalf("Grok task must not set DisallowTools; got %v", cfg.DisallowTools)
	}
}

// TestSpendCeilingStopsRatherThanRetries is 🎯T139's third mitigation.
//
// Framing can be ignored and (on Grok) tool restrictions are unavailable.
// The ceiling bounds what happens when the other mitigations are thin —
// and it must be TERMINAL, because retrying is exactly what turned the
// reported misfire into ~33,000 subagents.
func TestSpendCeilingStopsRatherThanRetries(t *testing.T) {
	c := &claudiaSummariser{workDir: "/tmp/x", ceiling: 1000}
	c.inTokens, c.outTok = 900, 200 // already past the ceiling

	_, err := c.Ask(context.Background(), "another drip")
	if !errors.Is(err, ErrSpendCeiling) {
		t.Fatalf("expected ErrSpendCeiling, got %v", err)
	}
	if c.calls != 0 {
		t.Error("the summariser spawned a task despite being over its ceiling")
	}
}

// TestSpendCeilingIsOnByDefault: a ceiling nobody enables protects nobody.
func TestSpendCeilingIsOnByDefault(t *testing.T) {
	c, ok := NewClaudiaSummariser("/tmp/x", "").(*claudiaSummariser)
	if !ok {
		t.Fatal("unexpected summariser type")
	}
	if c.ceiling <= 0 {
		t.Error("the default summariser has no spend ceiling")
	}
	if c.model != DefaultStreamsegModel {
		t.Errorf("default model=%q want %q", c.model, DefaultStreamsegModel)
	}
}
