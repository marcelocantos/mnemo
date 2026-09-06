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

func statuses(t *testing.T, base, got string, timing bool) map[string]string {
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
	for _, v := range compare(b, g, timing) {
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

func TestExactMetricsLockBothWays(t *testing.T) {
	// One select fewer is an improvement; it still fails, because the
	// baseline must move with it.
	fewer := strings.ReplaceAll(sampleBase, "129.0 selects/op", "128.0 selects/op")
	if s := statuses(t, sampleBase, fewer, true)["BenchmarkSearch/messages selects/op"]; s != "IMPROVED (re-lock)" {
		t.Fatalf("fewer selects: status %q", s)
	}
	more := strings.ReplaceAll(sampleBase, "36732 payload-bytes", "36733 payload-bytes")
	if s := statuses(t, sampleBase, more, true)["BenchmarkRecentActivity/30d payload-bytes"]; s != "REGRESSION" {
		t.Fatalf("one byte more: status %q", s)
	}
}

func TestTimingHasToleranceAndCanBeSkipped(t *testing.T) {
	// A fresh run on a 12-core machine, 10% slower: within tolerance.
	slower := strings.ReplaceAll(strings.ReplaceAll(sampleBase, "-16", "-12"), "1000000 ns/op", "1100000 ns/op")
	if s := statuses(t, sampleBase, slower, true)["BenchmarkSearch/messages ns/op"]; s != "ok" {
		t.Fatalf("10%% slower: status %q", s)
	}
	// Twice as slow is a regression; twice as fast needs a re-lock.
	much := strings.NewReplacer(
		"1000000 ns/op", "1000000000 ns/op",
		"1100000 ns/op", "1100000000 ns/op",
		"900000 ns/op", "900000000 ns/op",
	).Replace(sampleBase)
	if s := statuses(t, sampleBase, much, true)["BenchmarkSearch/messages ns/op"]; s != "REGRESSION" {
		t.Fatalf("1000x slower: status %q", s)
	}
	if s := statuses(t, much, sampleBase, true)["BenchmarkSearch/messages ns/op"]; s != "IMPROVED (re-lock)" {
		t.Fatalf("1000x faster: status %q", s)
	}
	if s := statuses(t, much, sampleBase, false)["BenchmarkSearch/messages ns/op"]; s != "skip" {
		t.Fatalf("timing off: status %q", s)
	}
}

func TestMissingAndNewBenchmarks(t *testing.T) {
	onlySearch := strings.SplitAfter(sampleBase, "129.0 selects/op\n")[0]
	st := statuses(t, sampleBase, onlySearch, true)
	if st["BenchmarkRecentActivity/30d "] != "MISSING" {
		t.Fatalf("dropped benchmark not reported: %v", st)
	}
	st = statuses(t, onlySearch, sampleBase, true)
	if st["BenchmarkRecentActivity/30d "] != "NEW" {
		t.Fatalf("added benchmark not reported: %v", st)
	}
}
