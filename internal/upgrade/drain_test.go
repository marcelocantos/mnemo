// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// PinUnknown must not be treated as zero — otherwise a torn route file
// reaps immediately with live pins.
func TestAffinityDrainPinUnknownDoesNotReapEarly(t *testing.T) {
	t.Parallel()
	var clock time.Time
	clock = time.Unix(0, 0)
	var reaped atomic.Bool
	reads := 0
	err := AffinityDrain(context.Background(), &AffinityDrainArgs{
		BackendIdx: 0,
		Pins: func(int) int {
			reads++
			if reads < 3 {
				return PinUnknown
			}
			return 0
		},
		MaxWait:      time.Minute,
		PollInterval: 10 * time.Millisecond,
		Now:          func() time.Time { return clock },
		Sleep:        func(d time.Duration) { clock = clock.Add(d) },
		Reap: func(ctx context.Context) error {
			reaped.Store(true)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reaped.Load() {
		t.Fatal("expected reap after authoritative zero")
	}
	if reads < 3 {
		t.Fatalf("should have waited through unknown reads, reads=%d", reads)
	}
}

func TestWriteRouteFileAtomicReadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edge-route.json")
	if err := WriteRouteFile(path, RouteFile{
		Backends:  []string{"http://127.0.0.1:1"},
		Primary:   0,
		PinCounts: []int{2},
	}); err != nil {
		t.Fatal(err)
	}
	// Concurrent readers must always get valid JSON when a read succeeds.
	// On Windows, os.Rename of the destination can briefly deny ReadFile
	// with a sharing violation; retry that — the invariant is "no partial
	// JSON", not "every open succeeds mid-replace".
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			var data []byte
			var err error
			for attempt := 0; attempt < 20; attempt++ {
				data, err = os.ReadFile(path)
				if err == nil {
					break
				}
				// Sharing violation / exclusive lock during replace.
				time.Sleep(time.Millisecond)
			}
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			var rf RouteFile
			if err := json.Unmarshal(data, &rf); err != nil {
				t.Errorf("partial json: %v data=%q", err, data)
				return
			}
		}
	}()
	for i := 0; i < 50; i++ {
		_ = WriteRouteFile(path, RouteFile{
			Backends:  []string{"http://127.0.0.1:1"},
			Primary:   0,
			PinCounts: []int{i % 3},
		})
	}
	<-done
}
