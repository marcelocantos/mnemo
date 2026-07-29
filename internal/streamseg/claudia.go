// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/mnemo/internal/store"
)

// claudiaSummariser runs one drip per claudia Task (🎯T132.2).
//
// This was first built on claudia's Session mode — a persistent agent in a
// tmux window — on the theory that the model's own memory of the spans it
// had opened was what made each drip cheap. Measuring it showed both
// halves of that to be wrong.
//
// It does not work. Session mode's Send drives the Claude Code TUI through
// tmux keystrokes, and a multi-line drip of a few KB is detected by the
// TUI as a PASTE. Pasted content sits in the composer unsubmitted, so the
// model never sees it and WaitForResponse waits forever. Observed
// directly: three drips queued as "[Pasted text #1 +17 lines]" and a
// 40-minute timeout with no reply.
//
// And it was never needed. renderDrip restates the rolling summary and the
// open spans on every drip — deliberately, so that a restarted agent needs
// no special first prompt — which means the model is already stateless
// between drips. The bounded state lives in the automaton, not in the
// conversation.
//
// So each drip is a fresh Task, exactly as the compactor and reviewer do
// it. That is the only claudia path with production evidence behind it,
// and it makes the linear-cost property trivially true rather than argued:
// nothing accumulates anywhere.
type claudiaSummariser struct {
	workDir string
	model   string

	// ceiling bounds total tokens for this session; 0 disables.
	ceiling int

	mu       sync.Mutex
	calls    int
	inTokens int
	outTok   int
	costUSD  float64
}

// NewClaudiaSummariser returns a summariser that runs one claude task per
// drip in workDir. An empty model uses claudia's default.
func NewClaudiaSummariser(workDir, model string) Summariser {
	return &claudiaSummariser{
		workDir: workDir, model: model,
		ceiling: DefaultSessionTokenCeiling,
	}
}

// taskConfig is the configuration every drip runs under. Extracted so a
// test can assert on what is actually passed (🎯T139): the Session-mode
// binding this replaced DID restrict tools, and the Task-mode rewrite
// dropped that silently — because Task mode ignored the concept entirely
// until claudia v0.20.0, so nothing failed and nothing warned.
func (c *claudiaSummariser) taskConfig() claudia.TaskConfig {
	return claudia.TaskConfig{
		WorkDir:       c.workDir,
		Model:         c.model,
		DisallowTools: store.SummariserDisallowedTools,
	}
}

// ErrSpendCeiling stops a session whose summarisation has cost more than
// DefaultSessionTokenCeiling.
//
// The backstop of 🎯T139's three mitigations, and the one that matters
// when the other two are bypassed. Framing can be ignored — in the
// incident this target exists for, the summariser ignored its wrapper
// entirely — and tool restrictions only remove the ability to ACT, not
// the ability to keep generating. A ceiling is what bounds the damage
// when something still goes wrong.
//
// Terminal by design: the runner stops rather than retries. Retrying on
// failure is precisely what turned that misfire into ~33,000 subagents,
// with blocked tool calls retried rather than aborted.
var ErrSpendCeiling = errors.New("streamseg: session token ceiling reached")

// DefaultSessionTokenCeiling bounds one session's total summarisation.
//
// Sized from measurement, not intuition: a drip costs ~45,000 input
// tokens (dominated by fixed per-call overhead), and a 150-message
// session takes ~12 drips, so ordinary sessions land near 540k. Ten
// million is roughly twenty times the largest ordinary case — high enough
// that no honest session trips it, low enough that a runaway stops in
// minutes rather than hours.
const DefaultSessionTokenCeiling = 10_000_000

func (c *claudiaSummariser) Ask(ctx context.Context, drip string) (string, error) {
	c.mu.Lock()
	spent := c.inTokens + c.outTok
	ceiling := c.ceiling
	c.mu.Unlock()
	if ceiling > 0 && spent >= ceiling {
		return "", fmt.Errorf("%w: %d tokens spent on this session", ErrSpendCeiling, spent)
	}

	// The marker keeps the summariser's own transcript out of the
	// compaction candidate set at ingest (session_meta.compactor_internal),
	// which is the same recursion guard the compactor relies on. Without
	// it a summariser session becomes a session to be summarised.
	combined := store.CompactorMarker + "\n\n" + SystemPrompt + "\n\n" + drip
	combined = sanitizePrompt(combined)

	task := claudia.NewTask(c.taskConfig())
	ch, err := task.Run(ctx, combined)
	if err != nil {
		return "", fmt.Errorf("claudia: run task: %w", err)
	}

	var text strings.Builder
	for ev := range ch {
		switch ev.Type {
		case claudia.TaskEventText:
			text.WriteString(ev.Content)
		case claudia.TaskEventResult:
			c.mu.Lock()
			c.calls++
			c.inTokens += ev.Usage.InputTokens +
				ev.Usage.CacheCreationInputTokens +
				ev.Usage.CacheReadInputTokens
			c.outTok += ev.Usage.OutputTokens
			c.costUSD += ev.CostUSD
			c.mu.Unlock()
		case claudia.TaskEventError:
			return "", fmt.Errorf("claudia: %s", ev.ErrorMsg)
		}
	}
	return text.String(), nil
}

// Restart is a no-op under Task mode: there is no accumulated context to
// reclaim, because there is no conversation. The runner still calls it
// when the automaton's budget estimate fills, which costs nothing and
// keeps the seam for any future implementation that does hold state.
func (c *claudiaSummariser) Restart(context.Context) error { return nil }

func (c *claudiaSummariser) Close() {}

// Usage reports what this summariser has spent, for the operating-point
// sweep (🎯T132.4). Cost per active-session-day is an acceptance
// criterion there, and it cannot be reconstructed after the fact.
func (c *claudiaSummariser) Usage() (calls, inTokens, outTokens int, costUSD float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.inTokens, c.outTok, c.costUSD
}

// UsageReporter is implemented by summarisers that can account for their
// own spend. The sweep type-asserts for it rather than widening
// Summariser, so a scripted or fake summariser need not pretend to have
// costs.
type UsageReporter interface {
	Usage() (calls, inTokens, outTokens int, costUSD float64)
}

// sanitizePrompt strips control characters that break the exec boundary.
// A NUL byte anywhere in the prompt makes the call fail with EINVAL, and
// a transcript drip is arbitrary user content that can contain anything.
func sanitizePrompt(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
