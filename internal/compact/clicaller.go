// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/mnemo/internal/claudecli"
	"github.com/marcelocantos/mnemo/internal/store"
)

// CLICaller implements LLMCaller over claudecli: one headless `claude -p`
// turn with the caller's own system prompt and nothing else — no built-in
// tools, no MCP servers, no settings files, no prompt caching.
//
// Why this exists alongside ClaudiaCaller. A summarisation turn is pure
// text→text, but claudia.Task spawns a full Claude Code session, so every
// call re-sent Claude Code's own system prompt, the built-in tool schemas,
// the user-scope MCP servers and their deferred-tool listing, the global
// CLAUDE.md and the skill listing — and wrote all of it to the 1-hour
// prompt cache, which bills at 2× plain input, for a session that never
// takes a second turn.
//
// Measured against the production summariser's own working directory
// (/tmp/mnemo-summariser, claude-sonnet-5, same one-sentence prompt):
//
//	claudia.Task spawn: 2 input + 21,108 cache-write + 16,886 cache-read
//	                    = 37,996 prompt tokens, $0.0881
//	claudecli spawn:      559 input + 0 cache = 559 prompt tokens, $0.0015
//
// The ~38k difference is fixed per call and independent of transcript
// length, so it was the majority of a 4,499-call, 293M-prompt-token bill.
//
// Claude only. Grok has no equivalent bare-spawn surface, so a Grok
// summariser still goes through ClaudiaCaller.
type CLICaller struct {
	workDir string
	model   string
}

// NewSummariserCaller returns the right LLMCaller for the configured
// provider: the bare claudecli spawn for Claude, claudia.Task otherwise.
// This is the constructor callers should use; NewClaudiaCaller remains
// for tests and for pinning a caller to the claudia path explicitly.
func NewSummariserCaller(opts ClaudiaCallerOpts) LLMCaller {
	if ParseProvider(opts.Provider) != claudia.ProviderClaude {
		return NewClaudiaCaller(opts)
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = DefaultClaudeModel
	}
	return &CLICaller{workDir: opts.WorkDir, model: model}
}

// compactorUserTurn composes the user message. It is its own function
// only so a test can assert that the marker still leads it: the guard is
// a prefix match, so anything inserted ahead of the marker disables it
// silently and the symptom surfaces at ingest, far from here.
func compactorUserTurn(userPrompt string) string {
	return store.CompactorMarker + "\n\n" + userPrompt
}

// Call runs one summarisation turn.
//
// The system prompt goes to --system-prompt rather than being baked into
// the user turn, so the transcript the model is asked to summarise cannot
// be confused with the instructions about how to summarise it. The user
// turn keeps the CompactorMarker prefix: store.IsCompactorMarker matches
// on the leading text of the user message, and that is what keeps the
// summariser's own spawned session out of the compaction candidate set
// at ingest (🎯T72).
func (c *CLICaller) Call(ctx context.Context, systemPrompt, userPrompt string) (LLMResult, error) {
	res, err := claudecli.Run(ctx, claudecli.Request{
		WorkDir:       c.workDir,
		Model:         c.model,
		SystemPrompt:  sanitizePrompt(systemPrompt),
		Prompt:        sanitizePrompt(compactorUserTurn(userPrompt)),
		DisallowTools: store.SummariserDisallowedTools,
	})
	if err != nil {
		return LLMResult{}, fmt.Errorf("compact: %w", err)
	}
	model := res.Model
	if model == "" {
		model = c.model
	}
	return LLMResult{
		Text:         res.Text,
		Model:        model,
		PromptTokens: res.PromptTokens(),
		OutputTokens: res.OutputTokens,
		CostUSD:      res.CostUSD,
	}, nil
}
