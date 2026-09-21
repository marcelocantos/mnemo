// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"context"
	"testing"
	"time"
)

func names(rep Report) []string {
	var out []string
	for _, r := range rep.Results {
		out = append(out, r.Name)
	}
	return out
}

// The defect this type exists to prevent: a Fast run carries only Fast
// checks, so serving the latest run as-is drops every Full result for up
// to an hour after each tick.
func TestSnapshotKeepsFullResultsAcrossFastRuns(t *testing.T) {
	s := NewSnapshot()
	t0 := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	s.Observe(Report{GeneratedAt: t0, Results: []Result{
		{Name: "fast.a", Tier: "fast", Severity: "ok"},
		{Name: "full.b", Tier: "full", Severity: "warn"},
	}}, true)
	s.Observe(Report{GeneratedAt: t0.Add(3 * time.Minute), Results: []Result{
		{Name: "fast.a", Tier: "fast", Severity: "fail"},
	}}, false)

	rep, ok := s.Report()
	if !ok {
		t.Fatal("snapshot reported empty after two runs")
	}
	if got := names(rep); len(got) != 2 || got[0] != "fast.a" || got[1] != "full.b" {
		t.Fatalf("results = %v, want [fast.a full.b] in registration order", got)
	}
	if rep.Results[0].Severity != "fail" {
		t.Errorf("fast.a = %q, want the newer fail", rep.Results[0].Severity)
	}
	if rep.Results[1].Severity != "warn" {
		t.Errorf("full.b = %q, want the Full run's warn to survive the Fast run", rep.Results[1].Severity)
	}
	if rep.OK != 0 || rep.Warn != 1 || rep.Fail != 1 {
		t.Errorf("counts ok=%d warn=%d fail=%d, want 0/1/1", rep.OK, rep.Warn, rep.Fail)
	}
	if !rep.GeneratedAt.Equal(t0.Add(3 * time.Minute)) {
		t.Errorf("GeneratedAt = %v, want the newest run", rep.GeneratedAt)
	}
}

// A full run decides which checks exist, so a dynamic check that has
// gone away does not linger in /health forever.
func TestSnapshotFullRunReplacesMembership(t *testing.T) {
	s := NewSnapshot()
	s.Observe(Report{Results: []Result{{Name: "plugin.x", Severity: "ok"}, {Name: "a", Severity: "ok"}}}, true)
	s.Observe(Report{Results: []Result{{Name: "a", Severity: "ok"}}}, true)
	rep, _ := s.Report()
	if got := names(rep); len(got) != 1 || got[0] != "a" {
		t.Fatalf("results = %v, want the departed plugin.x gone after a full run", got)
	}
}

func TestSnapshotEmptyUntilObserved(t *testing.T) {
	if _, ok := NewSnapshot().Report(); ok {
		t.Fatal("an empty snapshot must report ok=false so /health can fall back to a live run")
	}
}

// The scheduler hands its sink the merged report, not the run that just
// finished — the shim previously lost Full results after every Fast tick.
func TestSchedulerSinkReceivesMergedReport(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Check{Name: "fast.a", Tier: Fast, Run: func(context.Context) CheckResult { return Healthy("") }})
	reg.Register(Check{Name: "full.b", Tier: Full, Run: func(context.Context) CheckResult { return Healthy("") }})
	sch := NewScheduler(reg, nil, 0, 0)
	var last Report
	sch.OnReport(func(r Report) { last = r })

	sch.runOnce(context.Background(), true)
	sch.runOnce(context.Background(), false)

	if got := names(last); len(got) != 2 {
		t.Fatalf("sink after a Fast run got %v, want both checks", got)
	}
	latest, ok := sch.Latest()
	if !ok || len(latest.Results) != 2 {
		t.Fatalf("Latest() = %v ok=%v, want both checks", names(latest), ok)
	}
}

// Every result carries its own age, because a snapshot mixes runs from
// different cadences.
func TestResultsCarryCheckedAt(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Check{Name: "a", Tier: Fast, Run: func(context.Context) CheckResult { return Healthy("") }})
	before := time.Now().UTC().Add(-time.Second)
	rep := reg.Run(context.Background(), true, time.Now())
	if rep.Results[0].CheckedAt.Before(before) {
		t.Fatalf("CheckedAt = %v, want it set when the check ran", rep.Results[0].CheckedAt)
	}
}
