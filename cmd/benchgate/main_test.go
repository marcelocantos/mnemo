// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

const sampleBase = `goos: darwin
goarch: arm64
pkg: github.com/marcelocantos/mnemo/internal/store
BenchmarkSearch/messages-16    100   1000000 ns/op   50000 B/op   400 allocs/op   20.00 hits/op   129.0 selects/op
BenchmarkSearch/messages-16    100   1100000 ns/op   50000 B/op   400 allocs/op   20.00 hits/op   129.0 selects/op
BenchmarkSearch/messages-16    100    900000 ns/op   50000 B/op   400 allocs/op   20.00 hits/op   129.0 selects/op
BenchmarkRecentActivity/30d-16  10  20000000 ns/op  900000 B/op  9000 allocs/op   300 rows/op   36732 payload-bytes   1.000 selects/op
PASS
`

func statuses(t *testing.T, base, got string, exactOnly bool) map[string]string {
	t.Helper()
	b, err := parse(strings.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	g, err := parse(strings.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, v := range compare(b, g, exactOnly) {
		out[v.name+" "+v.metric] = v.status
	}
	return out
}

func TestParseCollapsesRepeatsAndDropsProcSuffix(t *testing.T) {
	r, err := parse(strings.NewReader(sampleBase))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := r["BenchmarkSearch/messages"]
	if !ok {
		t.Fatalf("name not normalised: %v", r)
	}
	if got := median(m["ns/op"]); got != 1000000 {
		t.Fatalf("median ns/op = %v, want 1000000", got)
	}
}

// A benchmark's samples are reduced to their floor, not their middle:
// competing load only ever makes a run slower, so the fastest sample is
// the closest thing to an uncontended one. Allocation counts, which do
// not move with load, keep the median.
func TestDurationsReduceToTheirFloor(t *testing.T) {
	r, err := parse(strings.NewReader(sampleBase))
	if err != nil {
		t.Fatal(err)
	}
	m := r["BenchmarkSearch/messages"]
	if got := reduce("ns/op", m["ns/op"]); got != 900000 {
		t.Fatalf("ns/op reduced to %v, want the minimum 900000", got)
	}
	if got := reduce("allocs/op", m["allocs/op"]); got != 400 {
		t.Fatalf("allocs/op reduced to %v, want 400", got)
	}
	// A run whose slowest sample doubled but whose floor held is a
	// contended machine, not a regression.
	contended := strings.Replace(sampleBase, "1100000 ns/op", "9100000 ns/op", 1)
	if s := statuses(t, sampleBase, contended, false)["BenchmarkSearch/messages ns/op"]; s != "ok" {
		t.Fatalf("one contended sample: status %q", s)
	}
	// A floor that moved is a real change, in either direction.
	slower := strings.NewReplacer(
		"1000000 ns/op", "2000000 ns/op",
		"1100000 ns/op", "2100000 ns/op",
		"900000 ns/op", "1900000 ns/op",
	).Replace(sampleBase)
	if s := statuses(t, sampleBase, slower, false)["BenchmarkSearch/messages ns/op"]; s != "REGRESSION" {
		t.Fatalf("floor doubled: status %q", s)
	}
}

// A stray background read inflates a per-iteration average by a
// fraction; a real extra statement adds a whole one. The gate must tell
// them apart, or it fails on noise and gets switched off.
func TestExactMetricsIgnoreFractionalNoiseButNotAWholeStatement(t *testing.T) {
	noisy := strings.Replace(sampleBase, "129.0 selects/op", "129.09 selects/op", 1)
	if s := statuses(t, sampleBase, noisy, false)["BenchmarkSearch/messages selects/op"]; s != "ok" {
		t.Fatalf("fractional noise: status %q", s)
	}
	real := strings.ReplaceAll(sampleBase, "129.0 selects/op", "130.0 selects/op")
	if s := statuses(t, sampleBase, real, false)["BenchmarkSearch/messages selects/op"]; s != "REGRESSION" {
		t.Fatalf("one whole statement more: status %q", s)
	}
}

func TestExactMetricsLockBothWays(t *testing.T) {
	// One select fewer is an improvement; it still fails, because the
	// baseline must move with it.
	fewer := strings.ReplaceAll(sampleBase, "129.0 selects/op", "128.0 selects/op")
	if s := statuses(t, sampleBase, fewer, false)["BenchmarkSearch/messages selects/op"]; s != "IMPROVED (re-lock)" {
		t.Fatalf("fewer selects: status %q", s)
	}
	more := strings.ReplaceAll(sampleBase, "36732 payload-bytes", "36733 payload-bytes")
	if s := statuses(t, sampleBase, more, false)["BenchmarkRecentActivity/30d payload-bytes"]; s != "REGRESSION" {
		t.Fatalf("one byte more: status %q", s)
	}
}

func TestTimingHasToleranceAndIsSkippedOutOfScope(t *testing.T) {
	// A fresh run on a 12-core machine, 10% slower: within tolerance.
	slower := strings.ReplaceAll(strings.ReplaceAll(sampleBase, "-16", "-12"), "1000000 ns/op", "1100000 ns/op")
	if s := statuses(t, sampleBase, slower, false)["BenchmarkSearch/messages ns/op"]; s != "ok" {
		t.Fatalf("10%% slower: status %q", s)
	}
	// Twice as slow is a regression; twice as fast needs a re-lock.
	much := strings.NewReplacer(
		"1000000 ns/op", "1000000000 ns/op",
		"1100000 ns/op", "1100000000 ns/op",
		"900000 ns/op", "900000000 ns/op",
	).Replace(sampleBase)
	if s := statuses(t, sampleBase, much, false)["BenchmarkSearch/messages ns/op"]; s != "REGRESSION" {
		t.Fatalf("1000x slower: status %q", s)
	}
	if s := statuses(t, much, sampleBase, false)["BenchmarkSearch/messages ns/op"]; s != "IMPROVED (re-lock)" {
		t.Fatalf("1000x faster: status %q", s)
	}
	if s := statuses(t, much, sampleBase, true)["BenchmarkSearch/messages ns/op"]; s != "skip" {
		t.Fatalf("-scope exact: status %q", s)
	}
}

// -scope exact is what CI runs. It still locks the metrics that cannot
// vary between machines, and skips allocation counts, which can.
func TestExactScopeKeepsTheExactLocksAndDropsAllocations(t *testing.T) {
	more := strings.ReplaceAll(sampleBase, "36732 payload-bytes", "40000 payload-bytes")
	if s := statuses(t, sampleBase, more, true)["BenchmarkRecentActivity/30d payload-bytes"]; s != "REGRESSION" {
		t.Fatalf("payload growth under -scope exact: status %q", s)
	}
	allocs := strings.ReplaceAll(sampleBase, "400 allocs/op", "4000 allocs/op")
	if s := statuses(t, sampleBase, allocs, true)["BenchmarkSearch/messages allocs/op"]; s != "skip" {
		t.Fatalf("allocations under -scope exact: status %q", s)
	}
	if s := statuses(t, sampleBase, allocs, false)["BenchmarkSearch/messages allocs/op"]; s != "REGRESSION" {
		t.Fatalf("allocations under -scope all: status %q", s)
	}
}

func TestMissingAndNewBenchmarks(t *testing.T) {
	onlySearch := strings.SplitAfter(sampleBase, "129.0 selects/op\n")[0]
	st := statuses(t, sampleBase, onlySearch, false)
	if st["BenchmarkRecentActivity/30d "] != "MISSING" {
		t.Fatalf("dropped benchmark not reported: %v", st)
	}
	st = statuses(t, onlySearch, sampleBase, false)
	if st["BenchmarkRecentActivity/30d "] != "NEW" {
		t.Fatalf("added benchmark not reported: %v", st)
	}
}
