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

// ClaudiaCaller implements LLMCaller via claudia.Task (claude -p headless).
// Each call spawns a fresh Task; sessions are not reused across calls so the
// summariser stays stateless and trivially terminable.
type ClaudiaCaller struct {
	workDir string
	model   string
}

// NewClaudiaCaller returns a caller that runs claude -p in workDir with the
// given model (e.g. "sonnet"). An empty model uses claudia's default.
func NewClaudiaCaller(workDir, model string) *ClaudiaCaller {
	return &ClaudiaCaller{workDir: workDir, model: model}
}

// Call runs a single summarisation turn and returns the result.
// The LLM sees systemPrompt prepended to userPrompt as a combined message
// (claude -p does not have a native system-prompt flag, so we bake it in).
func (c *ClaudiaCaller) Call(ctx context.Context, systemPrompt, userPrompt string) (LLMResult, error) {
	// Prefix the recursion marker so the spawned claude -p session's
	// transcript is recognisable at ingest (🎯T72) — its first user
	// message starts with store.CompactorMarker, which sets
	// session_meta.compactor_internal = 1 and keeps the summariser
	// session out of the compaction candidate set. Then sanitize: a NUL
	// byte anywhere in the prompt makes the exec call fail with EINVAL.
	combined := store.CompactorMarker + "\n\n" + systemPrompt + "\n\n" + userPrompt
	combined = sanitizePrompt(combined)

	task := claudia.NewTask(claudia.TaskConfig{
		WorkDir: c.workDir,
		Model:   c.model,
	})

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
		model = "claude-sonnet-4-6"
	}
	if c.model != "" {
		model = c.model
	}

	return LLMResult{
		Text:         text.String(),
		Model:        model,
		PromptTokens: promptTok,
		OutputTokens: outputTok,
		CostUSD:      costUSD,
	}, nil
}
