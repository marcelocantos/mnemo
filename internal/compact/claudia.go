// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/mnemo/internal/store"
)

// DefaultSummariserProvider is the CLI runtime for mnemo-spawned LLM
// tasks (compactor, reviewer, streaming segmenter). Grok Build via
// claudia v0.22+ ProviderGrok.
const DefaultSummariserProvider = claudia.ProviderGrok

// DefaultSummariserModel is used when config leaves model empty.
// Empty string would also work (claudia/grok default); pin an explicit
// id so spend and logs are comparable across restarts.
const DefaultSummariserModel = "grok-4"

// sanitizePrompt strips characters that make the combined prompt an
// invalid process argument or pollute the summariser input. The
// load-bearing one is NUL (U+0000): claudia passes the prompt as an
// argv element (task.go: `args = append(args, prompt)`), and Go's exec
// rejects any argument containing a NUL byte with EINVAL ("invalid
// argument"). Before 🎯T72 a single session whose transcript carried a
// stray NUL wedged the compactor — it was re-selected and re-failed
// every scan (775 lifetime failures from one poison session). Other C0
// control codes (except tab/newline/carriage-return) and DEL are
// dropped too: they carry no meaning in a text prompt and only risk
// arg/terminal quirks.
func sanitizePrompt(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// ClaudiaCaller implements LLMCaller via claudia.Task (headless provider
// CLI). Despite the historical name, the default provider is Grok
// (ProviderGrok), not Claude Code.
//
// Each call spawns a fresh Task; sessions are not reused across calls so
// the summariser stays stateless and trivially terminable.
type ClaudiaCaller struct {
	workDir  string
	model    string
	provider claudia.Provider
}

// NewClaudiaCaller returns a caller that runs the default summariser
// provider in workDir with the given model (e.g. "grok-4"). An empty
// model uses DefaultSummariserModel.
func NewClaudiaCaller(workDir, model string) *ClaudiaCaller {
	if model == "" {
		model = DefaultSummariserModel
	}
	return &ClaudiaCaller{
		workDir:  workDir,
		model:    model,
		provider: DefaultSummariserProvider,
	}
}

// taskConfig is the configuration every summarisation task runs under.
// Extracted so a test can assert on what is actually handed to claudia
// rather than on what this file appears to intend.
//
// Grok note (claudia v0.22): DisallowTools is Claude-only. Setting it on
// ProviderGrok makes Task.Run refuse before spawn (grokTaskPrecheck).
// Tool stripping for Grok is not yet oraclable in claudia, so we omit
// DisallowTools and rely on CompactorMarker framing, system prompts,
// and the streamseg spend ceiling (🎯T139). When claudia wires Grok
// --disallowed-tools, restore store.SummariserDisallowedTools here.
func (c *ClaudiaCaller) taskConfig() claudia.TaskConfig {
	p := c.provider
	if p == "" {
		p = DefaultSummariserProvider
	}
	cfg := claudia.TaskConfig{
		Provider: p,
		WorkDir:  c.workDir,
		Model:    c.model,
	}
	// Claude path retains tool stripping if someone forces ProviderClaude.
	if p == claudia.ProviderClaude || p == "" {
		cfg.DisallowTools = store.SummariserDisallowedTools
	}
	return cfg
}

// Call runs a single summarisation turn and returns the result.
// The LLM sees systemPrompt prepended to userPrompt as a combined message
// (headless CLIs typically lack a native system-prompt flag, so we bake it in).
func (c *ClaudiaCaller) Call(ctx context.Context, systemPrompt, userPrompt string) (LLMResult, error) {
	// Prefix the recursion marker so the spawned session's transcript is
	// recognisable at ingest (🎯T72) — its first user message starts with
	// store.CompactorMarker, which sets session_meta.compactor_internal = 1
	// and keeps the summariser session out of the compaction candidate set.
	// Then sanitize: a NUL byte anywhere in the prompt makes the exec call
	// fail with EINVAL.
	combined := store.CompactorMarker + "\n\n" + systemPrompt + "\n\n" + userPrompt
	combined = sanitizePrompt(combined)

	task := claudia.NewTask(c.taskConfig())

	ch, err := task.Run(ctx, combined)
	if err != nil {
		return LLMResult{}, fmt.Errorf("claudia: run task: %w", err)
	}

	var text strings.Builder
	var model string
	var promptTok, outputTok int
	var costUSD float64

	for ev := range ch {
		switch ev.Type {
		case claudia.TaskEventText:
			text.WriteString(ev.Content)
		case claudia.TaskEventInit:
			if ev.Model != "" {
				model = ev.Model
			}
		case claudia.TaskEventResult:
			costUSD = ev.CostUSD
			promptTok = ev.Usage.InputTokens +
				ev.Usage.CacheCreationInputTokens +
				ev.Usage.CacheReadInputTokens
			outputTok = ev.Usage.OutputTokens
		case claudia.TaskEventError:
			return LLMResult{}, fmt.Errorf("claudia: %s", ev.ErrorMsg)
		}
	}

	if model == "" {
		model = c.model
	}
	if model == "" {
		model = DefaultSummariserModel
	}

	return LLMResult{
		Text:         text.String(),
		Model:        model,
		PromptTokens: promptTok,
		OutputTokens: outputTok,
		CostUSD:      costUSD,
	}, nil
}
