// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/mnemo/internal/store"
)

// TestSummariserCannotAct is the 🎯T139 guard, and it exists because this
// exact protection was lost once already.
//
// The Session-mode binding passed DisallowTools. The Task-mode rewrite
// dropped it — and nothing failed, because claudia's Task mode ignored
// tool restrictions entirely until v0.20.0. A summariser runs untrusted
// transcript text through a live model that cannot tell data from
// instructions; one handed "research X and Y. go deep with fanout" obeyed
// it and spawned ~33,000 subagents.
func TestSummariserCannotAct(t *testing.T) {
	cfg := (&claudiaSummariser{workDir: "/tmp/x", model: "sonnet"}).taskConfig()

	if len(cfg.DisallowTools) == 0 {
		t.Fatal("the streaming summariser passes NO tool restrictions")
	}
	// The ones that turn a prompt-injection misfire into an expensive
	// one: spawning, shell, and network.
	for _, tool := range []string{"Bash", "WebFetch", "WebSearch", "Write", "Edit"} {
		if !slices.Contains(cfg.DisallowTools, tool) {
			t.Errorf("%q is available to the summariser", tool)
		}
	}
	// Agent comes from claudia's own baseline rather than from here, so
	// assert the baseline still names it — if claudia ever drops it, this
	// is where we find out.
	if !slices.Contains(splitBaseline(), "Agent") {
		t.Error("claudia's BaseDisallowedTools no longer removes Agent; " +
			"mnemo must add it explicitly")
	}
}

func splitBaseline() []string {
	var out []string
	cur := ""
	for _, r := range claudia.BaseDisallowedTools {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// TestSummariserToolListIsShared: both summariser paths must draw from one
// list, or one of them will be hardened and the other forgotten.
func TestSummariserToolListIsShared(t *testing.T) {
	cfg := (&claudiaSummariser{}).taskConfig()
	if !slices.Equal(cfg.DisallowTools, store.SummariserDisallowedTools) {
		t.Errorf("streamseg uses its own list %v rather than the shared one %v",
			cfg.DisallowTools, store.SummariserDisallowedTools)
	}
}

// TestSpendCeilingStopsRatherThanRetries is 🎯T139's third mitigation.
//
// Framing can be ignored and tool restrictions only remove the ability to
// ACT, not to keep generating. The ceiling bounds what happens when the
// first two are bypassed — and it must be TERMINAL, because retrying is
// exactly what turned the reported misfire into ~33,000 subagents.
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
	c, ok := NewClaudiaSummariser("/tmp/x", "sonnet").(*claudiaSummariser)
	if !ok {
		t.Fatal("unexpected summariser type")
	}
	if c.ceiling <= 0 {
		t.Error("the default summariser has no spend ceiling")
	}
}
