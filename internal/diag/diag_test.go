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
