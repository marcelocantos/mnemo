// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command benchgate compares a `go test -bench` run against a locked
// baseline and fails when any benchmark moved outside tolerance — in
// EITHER direction (🎯T165).
//
// A rise is a regression. A fall is a real improvement that the baseline
// no longer describes, and letting it through silently is how baselines
// drift until they guard nothing: the next regression only has to stay
// under the stale number. So an improvement fails too, with the
// instruction to re-lock (`make bench-lock`) in the same commit as the
// change that earned it.
//
// Metrics are compared by kind. Deterministic ones — payload-bytes,
// selects/op, rows/op, hits/op — must match exactly: they are functions
// of the corpus and the code, and the corpus is synthetic and fixed.
// Allocation counts get a small tolerance, timing and bytes-per-op a
// wider one.
//
// -scope exact compares only the first group. That is what a run on a
// different machine can honestly decide: ns/op recorded on one box says
// nothing about another, and allocation counts differ between operating
// systems because the code paths under them do.
//
// Usage:
//
//	benchgate -base docs/perf/baseline.txt -new new.txt [-scope exact]
//
// Both files are raw `go test -bench` output. Repeated runs of a
// benchmark (-count=N) are collapsed to their median per metric first,
// and the GOMAXPROCS suffix is dropped from names so a baseline recorded
// on a 16-core box compares with a 12-core one.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tolerances are fractions of the baseline value. Timing tolerance is
// deliberately wide: the reference baseline is recorded on a developer
// machine that is rarely idle, and a gate that fails on scheduler noise
// gets switched off, which is worse than a coarse one.
const (
	timingTolerance = 0.25
	allocTolerance  = 0.10
)

// Values for -scope.
const (
	scopeAll   = "all"
	scopeExact = "exact"
)

// exactMetrics are locked to the digit. They are functions of the input
// corpus and the code, never of the machine — but they are still
// REPORTED as a per-iteration average, so a background worker's stray
// read shows up as 19.03 selects rather than 19. Their samples are
// therefore reduced by minimum and compared floored: noise only ever
// adds, and it adds a fraction, while a real extra statement adds one.
var exactMetrics = map[string]bool{
	"payload-bytes": true,
	"selects/op":    true,
	"rows/op":       true,
	"hits/op":       true,
}

// timingMetrics vary with the machine and its load.
var timingMetrics = map[string]bool{
	"ns/op": true,
	"B/op":  true,
	"MB/s":  true,
}

// benchLine matches one result line: name, iterations, then metric
// pairs. The name's -N suffix is GOMAXPROCS.
var benchLine = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+(\d+)\s+(.*)$`)

// results maps benchmark name → metric → observed values.
type results map[string]map[string][]float64

// parse reads raw `go test -bench` output.
func parse(r io.Reader) (results, error) {
	out := results{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		m := benchLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		name := m[1]
		fields := strings.Fields(m[3])
		if len(fields)%2 != 0 {
			return nil, fmt.Errorf("odd metric list on %q", sc.Text())
		}
		if out[name] == nil {
			out[name] = map[string][]float64{}
		}
		for i := 0; i < len(fields); i += 2 {
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				return nil, fmt.Errorf("%s: bad value %q: %w", name, fields[i], err)
			}
			unit := fields[i+1]
			out[name][unit] = append(out[name][unit], v)
		}
	}
	return out, sc.Err()
}

// median collapses repeated runs. For an even count it takes the lower
// middle, which keeps the value one that was actually observed.
func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[(len(s)-1)/2]
}

// reduce collapses one metric's repeated runs to the value the gate
// compares: the floored minimum for an exact metric (see exactMetrics),
// the median for everything else.
func reduce(metric string, xs []float64) float64 {
	if !exactMetrics[metric] {
		return median(xs)
	}
	lo := xs[0]
	for _, x := range xs[1:] {
		if x < lo {
			lo = x
		}
	}
	return math.Floor(lo)
}

// verdict is one row of the comparison.
type verdict struct {
	name, metric string
	base, got    float64
	delta        float64 // fraction of base; NaN-free (base 0 handled)
	status       string  // "ok", "REGRESSION", "IMPROVED (re-lock)", "NEW", "MISSING", "skip"
}

// compare produces one verdict per (benchmark, metric) in the union of
// both files.
func compare(base, got results, exactOnly bool) []verdict {
	var out []verdict
	names := map[string]bool{}
	for n := range base {
		names[n] = true
	}
	for n := range got {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		b, g := base[name], got[name]
		if b == nil {
			out = append(out, verdict{name: name, status: "NEW"})
			continue
		}
		if g == nil {
			out = append(out, verdict{name: name, status: "MISSING"})
			continue
		}
		var metrics []string
		for m := range b {
			metrics = append(metrics, m)
		}
		sort.Strings(metrics)
		for _, metric := range metrics {
			gv, ok := g[metric]
			if !ok {
				out = append(out, verdict{name: name, metric: metric, base: reduce(metric, b[metric]), status: "MISSING"})
				continue
			}
			v := verdict{name: name, metric: metric, base: reduce(metric, b[metric]), got: reduce(metric, gv)}
			if v.base != 0 {
				v.delta = (v.got - v.base) / v.base
			} else if v.got != 0 {
				v.delta = 1
			}
			var tol float64
			switch {
			case exactMetrics[metric]:
				tol = 0
			case !exactOnly && timingMetrics[metric]:
				tol = timingTolerance
			case !exactOnly: // allocs/op and anything unknown
				tol = allocTolerance
			default:
				v.status = "skip"
				out = append(out, v)
				continue
			}
			// MB/s runs the other way: more is better.
			delta := v.delta
			if metric == "MB/s" {
				delta = -delta
			}
			switch {
			case delta > tol:
				v.status = "REGRESSION"
			case delta < -tol:
				v.status = "IMPROVED (re-lock)"
			default:
				v.status = "ok"
			}
			out = append(out, v)
		}
	}
	return out
}

func main() {
	basePath := flag.String("base", "docs/perf/baseline.txt", "locked baseline (raw go test -bench output)")
	newPath := flag.String("new", "", "fresh run to compare (raw go test -bench output)")
	scope := flag.String("scope", scopeAll, "which metrics to compare: "+scopeAll+
		" (every metric) or "+scopeExact+" (only the machine-independent ones, for a run "+
		"on a different machine than the baseline)")
	flag.Parse()
	if *newPath == "" {
		fmt.Fprintln(os.Stderr, "benchgate: -new is required")
		os.Exit(2)
	}
	base, err := readResults(*basePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchgate:", err)
		os.Exit(2)
	}
	got, err := readResults(*newPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchgate:", err)
		os.Exit(2)
	}
	if len(got) == 0 {
		fmt.Fprintln(os.Stderr, "benchgate: no benchmark lines in", *newPath)
		os.Exit(2)
	}

	if *scope != scopeAll && *scope != scopeExact {
		fmt.Fprintf(os.Stderr, "benchgate: -scope must be %s or %s\n", scopeAll, scopeExact)
		os.Exit(2)
	}
	verdicts := compare(base, got, *scope == scopeExact)
	failed := 0
	w := os.Stdout
	fmt.Fprintf(w, "%-52s %-14s %14s %14s %8s  %s\n", "benchmark", "metric", "baseline", "now", "delta", "status")
	for _, v := range verdicts {
		switch v.status {
		case "REGRESSION", "IMPROVED (re-lock)", "MISSING":
			failed++
		}
		if v.metric == "" {
			fmt.Fprintf(w, "%-52s %-14s %14s %14s %8s  %s\n", v.name, "", "", "", "", v.status)
			continue
		}
		fmt.Fprintf(w, "%-52s %-14s %14.6g %14.6g %+7.1f%%  %s\n",
			v.name, v.metric, v.base, v.got, v.delta*100, v.status)
	}
	if failed > 0 {
		fmt.Fprintf(w, "\nbenchgate: %d metric(s) outside tolerance. A REGRESSION needs a fix; an IMPROVED "+
			"result needs the baseline re-locked in the same commit (make bench-lock).\n", failed)
		os.Exit(1)
	}
	fmt.Fprintln(w, "\nbenchgate: within tolerance")
}

func readResults(path string) (results, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f)
}
