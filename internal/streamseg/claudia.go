// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"fmt"
	"sync"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/mnemo/internal/store"
)

// claudiaSummariser drives a long-lived Claude Code session (🎯T132.2).
//
// This is the first use of claudia's Session mode in mnemo. Everything
// else — the compactor, the reviewer — uses one-shot Task mode
// deliberately, so the summariser stays stateless and trivially
// terminable. Streaming needs the opposite: a conversation that
// accumulates, because the model's own memory of the spans it opened is
// what makes each drip cheap.
//
// The tmux substrate matters here rather than being incidental. The agent
// survives the consumer dying, so a daemon restart does not orphan a
// half-summarised session; and the crash-recovery path exists anyway,
// because the alternative to it is paying the summariser twice.
type claudiaSummariser struct {
	workDir string
	model   string

	mu    sync.Mutex
	agent *claudia.Agent
	// seed is prepended to the first drip of a fresh agent, carrying the
	// system prompt. Session mode has no separate system-prompt channel,
	// so it rides the first message.
	seedSent bool
}

// NewClaudiaSummariser creates a summariser backed by a persistent agent.
// workDir should be a stable scratch directory: the agent is a Claude
// Code process and will otherwise treat whatever directory it lands in as
// the project under discussion.
func NewClaudiaSummariser(workDir, model string) Summariser {
	return &claudiaSummariser{workDir: workDir, model: model}
}

func (c *claudiaSummariser) ensure(ctx context.Context) (*claudia.Agent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agent != nil && c.agent.Alive() {
		return c.agent, nil
	}
	ag, err := claudia.Start(claudia.Config{
		WorkDir: c.workDir,
		Model:   c.model,
		// The summariser reads a transcript that is handed to it; it has
		// no business touching the filesystem or the network, and a
		// watcher that could spawn sub-agents would be a recursion
		// hazard against the very sessions it is watching.
		DisallowTools: []string{
			"Bash", "Edit", "Write", "Read", "WebFetch", "WebSearch", "Task",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("start streaming summariser: %w", err)
	}
	if err := ag.WaitReady(ctx); err != nil {
		ag.Stop()
		return nil, fmt.Errorf("streaming summariser not ready: %w", err)
	}
	c.agent = ag
	c.seedSent = false
	return ag, nil
}

func (c *claudiaSummariser) Ask(ctx context.Context, drip string) (string, error) {
	ag, err := c.ensure(ctx)
	if err != nil {
		return "", err
	}
	msg := drip
	c.mu.Lock()
	if !c.seedSent {
		msg = store.CompactorMarker + "\n\n" + SystemPrompt + "\n\n" + drip
		c.seedSent = true
	}
	c.mu.Unlock()

	if err := ag.Send(msg); err != nil {
		return "", err
	}
	return ag.WaitForResponse(ctx)
}

// Restart drops the conversation and starts a fresh agent. The runner
// re-seeds from the automaton's bounded state, so this reclaims the
// context budget without losing a span.
func (c *claudiaSummariser) Restart(ctx context.Context) error {
	c.mu.Lock()
	if c.agent != nil {
		c.agent.Stop()
		c.agent = nil
	}
	c.seedSent = false
	c.mu.Unlock()
	_, err := c.ensure(ctx)
	return err
}

func (c *claudiaSummariser) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agent != nil {
		c.agent.Stop()
		c.agent = nil
	}
}
