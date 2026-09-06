// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestNewSummariserCallerPicksBareSpawnForClaude(t *testing.T) {
	for _, provider := range []string{"claude", "anthropic", "Claude"} {
		caller := NewSummariserCaller(ClaudiaCallerOpts{Provider: provider})
		cli, ok := caller.(*CLICaller)
		if !ok {
			t.Fatalf("provider %q got %T, want *CLICaller: the claudia.Task path re-sends ~38k fixed tokens per call", provider, caller)
		}
		if cli.model != DefaultClaudeModel {
			t.Errorf("provider %q model=%q, want %q", provider, cli.model, DefaultClaudeModel)
		}
	}
}

func TestNewSummariserCallerKeepsGrokOnClaudia(t *testing.T) {
	for _, provider := range []string{"", "grok", "xai"} {
		caller := NewSummariserCaller(ClaudiaCallerOpts{Provider: provider})
		if _, ok := caller.(*ClaudiaCaller); !ok {
			t.Errorf("provider %q got %T, want *ClaudiaCaller: claudecli spawns claude, not grok", provider, caller)
		}
	}
}

func TestNewSummariserCallerHonoursExplicitModel(t *testing.T) {
	caller := NewSummariserCaller(ClaudiaCallerOpts{Provider: "claude", Model: "claude-opus-5"})
	cli, ok := caller.(*CLICaller)
	if !ok {
		t.Fatalf("got %T, want *CLICaller", caller)
	}
	if cli.model != "claude-opus-5" {
		t.Errorf("model=%q, want claude-opus-5", cli.model)
	}
}

// The compactor's recursion guard (🎯T72) matches on the leading text of
// the spawned session's first user message. The bare-spawn path moves the
// system prompt out of the user turn, so this pins that the marker did
// not move with it.
func TestCLICallerUserTurnStillLeadsWithMarker(t *testing.T) {
	const userPrompt = "transcript span"
	// Mirrors the composition in Call. Kept as an explicit assertion
	// because store.IsCompactorMarker is a prefix match: any text
	// inserted ahead of the marker silently disables the guard, and the
	// symptom (the summariser summarising itself) appears at ingest,
	// far from this file.
	got := compactorUserTurn(userPrompt)
	if !store.IsCompactorMarker(got) {
		t.Fatalf("user turn does not lead with the compactor marker: %q", got)
	}
	if !strings.Contains(got, userPrompt) {
		t.Error("user turn dropped the transcript")
	}
}
