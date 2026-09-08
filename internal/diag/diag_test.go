// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"context"
	"testing"
	"time"
)

var now = time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

func ok(name string, tier Tier) Check {
	return Check{Name: name, Tier: tier, Run: func(context.Context) CheckResult { return Healthy("fine") }}
}

func TestRunTiersAndTally(t *testing.T) {
	r := NewRegistry()
	r.Register(
		ok("fast-ok", Fast),
		Check{Name: "fast-warn", Tier: Fast, Run: func(context.Context) CheckResult {
			return Warning("degraded", "do X")
		}},
		Check{Name: "full-fail", Tier: Full, Run: func(context.Context) CheckResult {
			return Failure("broken", "do Y")
		}},
	)

	// Fast pass: skips the Full check.
	fast := r.Run(context.Background(), false, now)
	if len(fast.Results) != 2 || fast.Fail != 0 || fast.Warn != 1 || fast.OK != 1 {
		t.Fatalf("fast run wrong: %+v", fast)
	}
	if fast.Worst() != Warn {
		t.Errorf("fast worst = %v, want warn", fast.Worst())
	}

	// Full pass: runs everything.
	full := r.Run(context.Background(), true, now)
	if len(full.Results) != 3 || full.Fail != 1 {
		t.Fatalf("full run wrong: %+v", full)
	}
	if full.Worst() != Fail {
		t.Errorf("full worst = %v, want fail", full.Worst())
	}
	if !full.GeneratedAt.Equal(now) {
		t.Errorf("GeneratedAt not stamped")
	}
}

func TestRunRecordsDurationMs(t *testing.T) {
	r := NewRegistry()
	r.Register(Check{Name: "slowish", Tier: Fast, Run: func(context.Context) CheckResult {
		time.Sleep(5 * time.Millisecond)
		return Healthy("ok")
	}})
	rep := r.Run(context.Background(), true, now)
	if len(rep.Results) != 1 {
		t.Fatalf("results: %+v", rep.Results)
	}
	if rep.Results[0].DurationMs < 5 {
		t.Fatalf("duration_ms=%d, want >= 5", rep.Results[0].DurationMs)
	}
}

func TestPanicBecomesFail(t *testing.T) {
	r := NewRegistry()
	r.Register(Check{Name: "boom", Tier: Fast, Run: func(context.Context) CheckResult {
		panic("kaboom")
	}})
	rep := r.Run(context.Background(), true, now)
	if rep.Fail != 1 || rep.Results[0].Severity != "fail" {
		t.Fatalf("panic should map to fail: %+v", rep)
	}
}

func TestSeverityAndTierStrings(t *testing.T) {
	if OK.String() != "ok" || Warn.String() != "warn" || Fail.String() != "fail" {
		t.Error("severity strings")
	}
	if Fast.String() != "fast" || Full.String() != "full" {
		t.Error("tier strings")
	}
}

// TestCheckTimeoutContainsAPathologicalCheck covers the maintainer
// brief's item 1, suggestion 4.
//
// The report is assembled sequentially with no per-check bound, so one
// slow check held the whole /health response — which is how a Fast-tier
// check running a full-table blob scan took the endpoint to 95s. A check
// that will not answer must become a failed check, not a stalled report.
func TestCheckTimeoutContainsAPathologicalCheck(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Check{Name: "wedged", Tier: Fast, Run: func(ctx context.Context) CheckResult {
		<-ctx.Done() // never answers on its own
		return Healthy("unreachable")
	}})
	reg.Register(Check{Name: "quick", Tier: Fast, Run: func(context.Context) CheckResult {
		return Healthy("fine")
	}})

	// Drive the PER-CHECK bound, not the caller's deadline: a caller
	// deadline that expires mid-report would fail every remaining check,
	// which is a different behaviour from the one under test.
	defer CheckTimeoutForTest(CheckTimeoutForTest(200 * time.Millisecond))

	start := time.Now()
	rep := reg.Run(context.Background(), false, time.Now())
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("Run took %s; a wedged check must not hold the report", elapsed)
	}
	var wedged, quick *Result
	for i := range rep.Results {
		switch rep.Results[i].Name {
		case "wedged":
			wedged = &rep.Results[i]
		case "quick":
			quick = &rep.Results[i]
		}
	}
	if wedged == nil || quick == nil {
		t.Fatalf("both checks must appear in the report, got %+v", rep.Results)
	}
	if wedged.Severity != Fail.String() {
		t.Errorf("wedged check severity = %q, want fail", wedged.Severity)
	}
	if quick.Severity != OK.String() {
		t.Errorf("a healthy check must still report ok alongside a wedged one, got %q", quick.Severity)
	}
}
