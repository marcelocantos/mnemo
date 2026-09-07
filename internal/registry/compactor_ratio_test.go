// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"testing"

	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/diag"
)

// snapshot builds a HealthSnapshot carrying just the tick tallies the
// ratio is computed from.
func snapshot(compacted, failed int64) compact.HealthSnapshot {
	return compact.HealthSnapshot{
		Counts:                map[string]int64{"compacted": compacted, "failed": failed},
		FailureRatioHealthy:   compact.FailureRatioHealthy,
		FailureRatioMinSample: compact.FailureRatioMinSample,
	}
}

// TestCompactionFailureRatioResult pins the severity boundaries of the
// ratio arm of compactor.breaker (🎯T167). The live daemon that prompted
// this sat at 1001 failed / 1261 compacted — a ratio of 0.79, four times
// the documented healthy ceiling — while the check reported "compaction
// watcher healthy", because a closed breaker was the only thing it looked
// at.
func TestCompactionFailureRatioResult(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hs        compact.HealthSnapshot
		unhealthy bool
		severity  diag.Severity
	}{
		{name: "no compactions yet", hs: snapshot(0, 0)},
		{
			// Below the sample floor a single failure yields a ratio of
			// 1.00, which must not warn: the daemon is young, not sick.
			name: "one failure in a young daemon is not judged",
			hs:   snapshot(1, 1),
		},
		{name: "clean steady state", hs: snapshot(100, 0)},
		{name: "at the healthy ceiling", hs: snapshot(100, 20)},
		{
			name:      "just past the healthy ceiling warns",
			hs:        snapshot(100, 21),
			unhealthy: true,
			severity:  diag.Warn,
		},
		{
			name:      "at the severe threshold still warns",
			hs:        snapshot(100, 50),
			unhealthy: true,
			severity:  diag.Warn,
		},
		{
			name:      "past the severe threshold fails",
			hs:        snapshot(100, 51),
			unhealthy: true,
			severity:  diag.Fail,
		},
		{
			name:      "the ratio observed on the live daemon fails",
			hs:        snapshot(1261, 1001),
			unhealthy: true,
			severity:  diag.Fail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, unhealthy := compactionFailureRatioResult(tc.hs)
			if unhealthy != tc.unhealthy {
				t.Fatalf("unhealthy = %v, want %v (counts %v)", unhealthy, tc.unhealthy, tc.hs.Counts)
			}
			if !tc.unhealthy {
				return
			}
			if res.Severity != tc.severity {
				t.Errorf("severity = %v, want %v", res.Severity, tc.severity)
			}
			if res.Detail == "" || res.Remediation == "" {
				t.Errorf("an unhealthy ratio must carry detail and remediation, got %+v", res)
			}
		})
	}
}

// TestFailureRatioThresholdIsShared guards the divergence this target
// exists to prevent: the number mnemo_ops op=compactor prints must be the
// number the health check enforces. The report reads it off the snapshot
// rather than importing this package, so the wiring is what can rot.
func TestFailureRatioThresholdIsShared(t *testing.T) {
	hs := snapshot(100, 0)
	if hs.FailureRatioHealthy != compact.FailureRatioHealthy {
		t.Errorf("snapshot carries %.2f, check enforces %.2f",
			hs.FailureRatioHealthy, compact.FailureRatioHealthy)
	}
	if compact.FailureRatioSevere <= compact.FailureRatioHealthy {
		t.Errorf("severe threshold %.2f must sit above healthy %.2f",
			compact.FailureRatioSevere, compact.FailureRatioHealthy)
	}
}
