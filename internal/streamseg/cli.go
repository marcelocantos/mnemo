// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/mnemo/internal/claudecli"
	"github.com/marcelocantos/mnemo/internal/store"
)

// bareSummariser runs one drip per headless `claude -p` turn: the
// segmenter's own system prompt and nothing else — no built-in tools, no
// MCP servers, no settings files, no prompt caching.
//
// Why this exists alongside claudiaSummariser. A drip is a pure text→text
// call that never takes a second turn, but claudia.Task spawns a full
// Claude Code session, so every drip re-sent Claude Code's own system
// prompt, the built-in tool schemas, the user-scope MCP servers and their
// deferred-tool listing, the global CLAUDE.md and the skill listing — and
// wrote all of it to the 1-hour prompt cache, billed at 2x plain input,
// for a session nothing ever reads back. That is the same defect the
// compactor carried, and DefaultSessionTokenCeiling's own derivation
// names it: "a drip costs ~45,000 input tokens (dominated by fixed
// per-call overhead)" against an 840-byte payload.
//
// Claude only. Grok has no equivalent bare-spawn surface, so a Grok
// segmenter still goes through claudiaSummariser.
type bareSummariser struct {
	workDir string
	model   string

	// ceiling bounds total tokens for this session; 0 disables. Same
	// backstop as the claudia path (🎯T139): a cheaper spawn bounds the
	// per-call bill, not a runaway's call count.
	ceiling int

	mu       sync.Mutex
	calls    int
	inTokens int
	outTok   int
	costUSD  float64
}

// NewSummariser returns the right summariser for the configured
// provider: the bare claudecli spawn for Claude, claudia.Task otherwise.
// This is the constructor callers should use; NewClaudiaSummariser
// remains for Grok and for pinning the claudia path explicitly (the
// replay harness measures both arms through it).
func NewSummariser(opts ClaudiaSummariserOpts) Summariser {
	if parseStreamProvider(opts.Provider) != claudia.ProviderClaude {
		return NewClaudiaSummariser(opts)
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = defaultModel(claudia.ProviderClaude)
	}
	return &bareSummariser{
		workDir: opts.WorkDir,
		model:   model,
		ceiling: DefaultSessionTokenCeiling,
	}
}

// dripUserTurn composes the user message.
//
// The marker leads it, exactly as on the claudia path: store's
// compactor-internal guard matches on the leading text of the user
// message, and that is what keeps the segmenter's own spawned session
// out of the candidate sets at ingest. Anything inserted ahead of the
// marker disables the guard silently, so this is its own function for a
// test to assert on.
func dripUserTurn(drip string) string {
	return store.CompactorMarker + "\n\n" + drip
}

func (b *bareSummariser) Ask(ctx context.Context, drip string) (string, error) {
	b.mu.Lock()
	spent := b.inTokens + b.outTok
	ceiling := b.ceiling
	b.mu.Unlock()
	if ceiling > 0 && spent >= ceiling {
		return "", fmt.Errorf("%w: %d tokens spent on this session", ErrSpendCeiling, spent)
	}

	res, err := claudecli.Run(ctx, claudecli.Request{
		WorkDir: b.workDir,
		Model:   b.model,
		// The instructions go to --system-prompt rather than into the
		// user turn, so the transcript the model is asked to describe
		// cannot be confused with the rules for describing it (🎯T139).
		SystemPrompt:  sanitizePrompt(SystemPrompt),
		Prompt:        sanitizePrompt(dripUserTurn(drip)),
		DisallowTools: store.SummariserDisallowedTools,
	})
	if err != nil {
		return "", fmt.Errorf("streamseg: %w", err)
	}

	b.mu.Lock()
	b.calls++
	b.inTokens += res.PromptTokens()
	b.outTok += res.OutputTokens
	b.costUSD += res.CostUSD
	b.mu.Unlock()
	return res.Text, nil
}

// Restart is a no-op for the same reason it is on the claudia path:
// renderDrip restates the working set every drip, so nothing accumulates
// between calls.
func (b *bareSummariser) Restart(context.Context) error { return nil }

func (b *bareSummariser) Close() {}

// Usage reports what this summariser has spent, for the operating-point
// sweep and the replay harness.
func (b *bareSummariser) Usage() (calls, inTokens, outTokens int, costUSD float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.inTokens, b.outTok, b.costUSD
}
