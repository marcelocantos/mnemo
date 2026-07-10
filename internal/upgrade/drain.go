// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"fmt"
	"time"
)

// DefaultDrainPoll is how often AffinityDrain re-checks pin counts.
const DefaultDrainPoll = 200 * time.Millisecond

// DefaultDrainMaxWait caps how long AffinityDrain waits for pins to
// clear before reaping the old backend anyway.
const DefaultDrainMaxWait = 30 * time.Minute

// PinCounter reports how many sessions are still pinned to a backend.
// Production uses edge-route.json pin_counts; tests inject fakes.
type PinCounter func(backendIdx int) int

// AffinityDrainArgs configures happy-path upgrade drain (🎯T97.5):
// primary already flipped; this backend keeps serving its pins until
// the count hits zero (or MaxWait), then calls Reap.
type AffinityDrainArgs struct {
	// BackendIdx is this process's index in the edge route table.
	BackendIdx int
	// Pins returns the current pin count for BackendIdx.
	Pins PinCounter
	// MaxWait bounds the wait (default DefaultDrainMaxWait).
	MaxWait time.Duration
	// PollInterval between pin checks (default DefaultDrainPoll).
	PollInterval time.Duration
	// Sleep is time.Sleep; inject in tests.
	Sleep func(d time.Duration)
	// Now is time.Now; inject in tests.
	Now func() time.Time
	// Reap stops this backend (SIGTERM / shutdown). Required.
	Reap func(ctx context.Context) error
}

// AffinityDrain waits until no sessions are pinned to BackendIdx, then
// reaps. Never repins — that would send Mcp-Session-Id to a backend
// that does not hold the mcp-go session state.
func AffinityDrain(ctx context.Context, args *AffinityDrainArgs) error {
	if args == nil || args.Pins == nil || args.Reap == nil {
		return fmt.Errorf("upgrade: AffinityDrain requires Pins and Reap")
	}
	maxWait := args.MaxWait
	if maxWait <= 0 {
		maxWait = DefaultDrainMaxWait
	}
	poll := args.PollInterval
	if poll <= 0 {
		poll = DefaultDrainPoll
	}
	sleep := args.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	now := args.Now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(maxWait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if args.Pins(args.BackendIdx) <= 0 {
			return args.Reap(ctx)
		}
		if !now().Before(deadline) {
			// Timed out with pins remaining: still reap (sessions that
			// never disconnected). Prefer waiting; force is last resort.
			return args.Reap(ctx)
		}
		// Sleep in small steps so ctx cancel is noticed.
		sleep(poll)
	}
}

// FailoverRepin is the crash-only path: move every pin to primary.
// Must not be used for happy-path auto-apply (breaks stateful MCP).
type FailoverRepinFunc func() (moved int)

// FailoverRepin runs the crash repin hook if non-nil.
func FailoverRepin(fn FailoverRepinFunc) int {
	if fn == nil {
		return 0
	}
	return fn()
}
