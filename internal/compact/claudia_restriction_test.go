// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"slices"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestCompactorCannotAct is the 🎯T139 guard for the compactor, which is
// the higher-exposure path: it summarises EVERY session in the corpus,
// thousands of them, any of which may contain imperative text.
//
// Its prompt already frames the transcript as data — and the incident
// this guards against showed framing alone is not enough, because the
// summariser there ignored its wrapper and obeyed the embedded text.
func TestCompactorCannotAct(t *testing.T) {
	cfg := NewClaudiaCaller("/tmp/x", "sonnet").taskConfig()

	if len(cfg.DisallowTools) == 0 {
		t.Fatal("the compactor passes NO tool restrictions")
	}
	for _, tool := range []string{"Bash", "WebFetch", "WebSearch", "Write", "Edit"} {
		if !slices.Contains(cfg.DisallowTools, tool) {
			t.Errorf("%q is available to the compactor", tool)
		}
	}
	if !slices.Equal(cfg.DisallowTools, store.SummariserDisallowedTools) {
		t.Errorf("compactor uses its own list %v rather than the shared one %v",
			cfg.DisallowTools, store.SummariserDisallowedTools)
	}
}
