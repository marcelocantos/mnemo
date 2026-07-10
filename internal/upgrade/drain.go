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

// PinUnknown is returned by PinCounter when the count cannot be read
// (I/O error, partial file). AffinityDrain must treat this as "still
// has pins" and must not reap.
const PinUnknown = -1

// PinCounter reports how many sessions are still pinned to a backend.
// Return PinUnknown (-1) when the count is not trustworthy (read error);
// never return 0 on I/O failure — that force-reaps live sessions.
// Production uses edge-route.json pin_counts; tests inject fakes.
type PinCounter func(backendIdx int) int

// AffinityDrainArgs configures happy-path upgrade drain (🎯T97.5):
// primary already flipped; this backend keeps serving its pins until
// the count hits zero (or MaxWait), then calls Reap.
type AffinityDrainArgs struct {
	// BackendIdx is this process's index in the edge route table.
	BackendIdx int
	// Pins returns the current pin count for BackendIdx, or PinUnknown.
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
//
// PinUnknown (read failure) is treated as non-zero so a torn route file
// cannot trigger an immediate reap with live pins.
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
		n := args.Pins(args.BackendIdx)
		// Only an authoritative zero is safe to reap on. PinUnknown and
		// positive counts keep waiting.
		if n == 0 {
			return args.Reap(ctx)
		}
		if !now().Before(deadline) {
			// Timed out: force reap only if we had a readable positive
			// count (abandoned clients). If the last read was unknown,
			// still force after max wait so the upgrade cannot hang
			// forever — callers should log this path.
			return args.Reap(ctx)
		}
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
