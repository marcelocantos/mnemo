// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestAffinityDrainWaitsForZeroPinsThenReaps(t *testing.T) {
	t.Parallel()
	var clock time.Time
	clock = time.Unix(1_000_000, 0)
	pins := 2
	var reaped atomic.Bool
	var sleeps int
	err := AffinityDrain(context.Background(), &AffinityDrainArgs{
		BackendIdx: 0,
		Pins: func(idx int) int {
			if idx != 0 {
				t.Fatalf("idx %d", idx)
			}
			return pins
		},
		MaxWait:      time.Minute,
		PollInterval: 10 * time.Millisecond,
		Now:          func() time.Time { return clock },
		Sleep: func(d time.Duration) {
			sleeps++
			clock = clock.Add(d)
			if sleeps == 2 {
				pins = 0 // clear after two polls
			}
		},
		Reap: func(ctx context.Context) error {
			reaped.Store(true)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reaped.Load() {
		t.Fatal("expected reap")
	}
	if sleeps < 2 {
		t.Fatalf("expected wait polls, sleeps=%d", sleeps)
	}
}

func TestAffinityDrainReapsImmediatelyWhenAlreadyZero(t *testing.T) {
	t.Parallel()
	var reaped atomic.Bool
	err := AffinityDrain(context.Background(), &AffinityDrainArgs{
		BackendIdx: 1,
		Pins:       func(int) int { return 0 },
		Reap: func(ctx context.Context) error {
			reaped.Store(true)
			return nil
		},
	})
	if err != nil || !reaped.Load() {
		t.Fatalf("err=%v reaped=%v", err, reaped.Load())
	}
}

func TestAffinityDrainMaxWaitForcesReap(t *testing.T) {
	t.Parallel()
	var clock time.Time
	clock = time.Unix(0, 0)
	var reaped atomic.Bool
	err := AffinityDrain(context.Background(), &AffinityDrainArgs{
		BackendIdx:   0,
		Pins:         func(int) int { return 5 },
		MaxWait:      50 * time.Millisecond,
		PollInterval: 20 * time.Millisecond,
		Now:          func() time.Time { return clock },
		Sleep:        func(d time.Duration) { clock = clock.Add(d) },
		Reap: func(ctx context.Context) error {
			reaped.Store(true)
			return nil
		},
	})
	if err != nil || !reaped.Load() {
		t.Fatalf("err=%v reaped=%v", err, reaped.Load())
	}
}

func TestFailoverRepinCallsHook(t *testing.T) {
	t.Parallel()
	if FailoverRepin(nil) != 0 {
		t.Fatal("nil")
	}
	if n := FailoverRepin(func() int { return 3 }); n != 3 {
		t.Fatalf("n=%d", n)
	}
}
