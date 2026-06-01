// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"strings"
	"testing"
	"time"
)

type fakeHealthReporter struct{ h CompactorHealth }

func (f fakeHealthReporter) Health() CompactorHealth { return f.h }

func resolver(h CompactorHealth) func(string) CompactorHealthReporter {
	return func(string) CompactorHealthReporter { return fakeHealthReporter{h: h} }
}

// TestCompactorStatusStuckHeuristic verifies 🎯T71: the "watcher
// appears stuck" warning only fires when BOTH the last scan AND the
// last tick are older than 2× scan_interval. A scan returning a full
// batch can legitimately push last_scan_at well past the threshold
// while the watcher ticks through candidates, so last_tick_at carries
// the proof-of-life signal alongside last_scan_at.
func TestCompactorStatusStuckHeuristic(t *testing.T) {
	now := time.Now()
	const scanInterval = time.Minute
	stale := 5 * scanInterval // well past 2 × scan_interval

	cases := []struct {
		name      string
		lastScan  time.Time
		lastTick  time.Time
		wantStuck bool
	}{
		{
			name:      "recent_scan_recent_tick",
			lastScan:  now.Add(-10 * time.Second),
			lastTick:  now.Add(-5 * time.Second),
			wantStuck: false,
		},
		{
			name:      "stale_scan_recent_tick", // T71 regression: was false-positive
			lastScan:  now.Add(-stale),
			lastTick:  now.Add(-10 * time.Second),
			wantStuck: false,
		},
		{
			name:      "recent_scan_stale_tick",
			lastScan:  now.Add(-10 * time.Second),
			lastTick:  now.Add(-stale),
			wantStuck: false,
		},
		{
			name:      "stale_scan_stale_tick", // genuinely wedged
			lastScan:  now.Add(-stale),
			lastTick:  now.Add(-stale),
			wantStuck: true,
		},
		{
			name:      "stale_scan_no_tick_yet", // startup before first tick
			lastScan:  now.Add(-stale),
			lastTick:  time.Time{},
			wantStuck: true,
		},
		{
			name:      "no_scan_yet", // daemon just started
			lastScan:  time.Time{},
			lastTick:  time.Time{},
			wantStuck: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := CompactorHealth{
				LastScanAt:   c.lastScan,
				LastTickAt:   c.lastTick,
				ScanInterval: scanInterval,
				Counts:       map[string]int64{},
			}
			ch := &callHandler{cc: CallContext{Username: "test"}}
			out, _, err := ch.compactorStatus(resolver(h))
			if err != nil {
				t.Fatalf("compactorStatus: %v", err)
			}
			hasWarning := strings.Contains(out, "appears stuck")
			if hasWarning != c.wantStuck {
				t.Errorf("warning=%v want=%v\noutput:\n%s", hasWarning, c.wantStuck, out)
			}
		})
	}
}
