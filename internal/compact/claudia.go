// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/marcelocantos/claudia"
)

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
	combined := systemPrompt + "\n\n" + userPrompt

	task := claudia.NewTask(claudia.TaskConfig{
		WorkDir: c.workDir,
		Model:   c.model,
	})

	ch, err := task.RunTask(ctx, combined)
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
