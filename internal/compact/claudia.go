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

// DefaultSummariserProvider is used when config omits summariser.provider.
const DefaultSummariserProvider = claudia.ProviderGrok

// DefaultGrokModel is used when provider is Grok and model is empty.
const DefaultGrokModel = "grok-4"

// DefaultClaudeModel is used when provider is Claude and model is empty.
const DefaultClaudeModel = "sonnet"

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
// CLI). Provider and model come from Config.Summariser (via the registry),
// not compile-time hardcoding of a single backend.
//
// Each call spawns a fresh Task; sessions are not reused across calls so
// the summariser stays stateless and trivially terminable.
type ClaudiaCaller struct {
	workDir  string
	model    string
	provider claudia.Provider
}

// ClaudiaCallerOpts configures a summariser Task caller.
type ClaudiaCallerOpts struct {
	WorkDir string
	// Model is the provider model id; empty uses the provider default.
	Model string
	// Provider is "grok", "claude", or empty (→ Grok default).
	// Prefer store.SummariserConfig.EffectiveProvider().
	Provider string
}

// NewClaudiaCaller returns a caller for the configured provider/model.
// Empty provider → Grok; empty model → provider default.
func NewClaudiaCaller(opts ClaudiaCallerOpts) *ClaudiaCaller {
	p := ParseProvider(opts.Provider)
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = DefaultModelFor(p)
	}
	return &ClaudiaCaller{
		workDir:  opts.WorkDir,
		model:    model,
		provider: p,
	}
}

// ParseProvider maps config strings to claudia.Provider.
// Unknown names fall back to Grok after LoadConfig validation should
// have rejected them; still safe for tests.
func ParseProvider(s string) claudia.Provider {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "grok", "xai":
		return claudia.ProviderGrok
	case "claude", "anthropic":
		return claudia.ProviderClaude
	default:
		return DefaultSummariserProvider
	}
}

// DefaultModelFor returns the empty-config model for a provider.
func DefaultModelFor(p claudia.Provider) string {
	if p == claudia.ProviderClaude {
		return DefaultClaudeModel
	}
	return DefaultGrokModel
}

// taskConfig is the configuration every summarisation task runs under.
// Extracted so a test can assert on what is actually handed to claudia
// rather than on what this file appears to intend.
//
// Grok: DisallowTools must stay empty — claudia v0.22 refuses Grok tasks
// with tool restrictions (CapabilityToolRestrictions unsupported). Rely
// on CompactorMarker framing, system prompts, and streamseg spend ceiling.
// Claude: applies store.SummariserDisallowedTools (🎯T139).
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
	if p == claudia.ProviderClaude {
		cfg.DisallowTools = store.SummariserDisallowedTools
	}
	return cfg
}

// Call runs a single summarisation turn and returns the result.
// The LLM sees systemPrompt prepended to userPrompt as a combined message
// (headless CLIs typically lack a native system-prompt flag, so we bake it in).
func (c *ClaudiaCaller) Call(ctx context.Context, systemPrompt, userPrompt string) (LLMResult, error) {
	// Prefix the recursion marker so the spawned session's transcript is
	// recognisable at ingest (🎯T72). Then sanitize NULs for exec argv.
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
		model = DefaultModelFor(c.provider)
	}

	return LLMResult{
		Text:         text.String(),
		Model:        model,
		PromptTokens: promptTok,
		OutputTokens: outputTok,
		CostUSD:      costUSD,
	}, nil
}
