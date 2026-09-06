// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// The perf corpus is the shared fixture behind the benchmarks in
// perf_bench_test.go and the deterministic ratchet in
// perf_ratchet_test.go (🎯T165). It is synthetic on purpose: the
// numbers it produces are comparable across machines and days, which
// a snapshot of the owner's index is not. Its shape follows the live
// index where the shape is what hurts — many repos per window, a
// distinct topic per session, several assistant records per session —
// without needing to be large.
//
// Everything about the corpus is fixed by the constants below. Change
// one and every locked number in the ratchet moves; that is the point of
// locking them, so re-lock deliberately.
const (
	// perfSeed fixes the generator's RNG.
	perfSeed = 2026

	// perfEpochAgo places every session inside the widest window the
	// benchmarks use (30 days) and outside none of them. Sessions are a
	// minute apart, so the corpus spans under a day from this point.
	perfEpochAgo = 3 * 24 * time.Hour

	// perfSearchLimit and perfContext are the tool's defaults.
	perfSearchLimit = 20
	perfContext     = 3

	// perfWindowDays is the window the recent_activity and usage
	// benchmarks ask for — the 30-day call is the one that returned
	// 1.7 MB in production.
	perfWindowDays = 30

	// perfQuery is drawn from the generator's vocabulary so that every
	// corpus size has plenty of hits; the OR-relaxed form matches most
	// assistant messages.
	perfQuery = "refactor scheduler compactor"
)

// perfShape sizes a corpus. The benchmark shape is larger than the
// ratchet's because the ratchet runs in every `go test ./...` and only
// needs enough rows for the locked numbers to be meaningful.
type perfShape struct {
	sessions int
	repos    int
}

var (
	perfBenchShape   = perfShape{sessions: 1200, repos: 300}
	perfRatchetShape = perfShape{sessions: 300, repos: 60}
)

// perfRepoNames returns n distinct org/repo names.
func perfRepoNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("acme/repo-%03d", i)
	}
	return names
}

// perfGenerator returns the generator for one corpus shape. Epoch is
// relative to now so the window queries always see every session.
func perfGenerator(shape perfShape, seed int64) *storetest.Generator {
	return &storetest.Generator{
		Seed:        seed,
		Sessions:    shape.sessions,
		Repos:       perfRepoNames(shape.repos),
		MsgsDist:    storetest.Distribution{Min: 8, Max: 60, Mean: 24},
		TokensDist:  storetest.Distribution{Min: 80, Max: 900, Mean: 300},
		ToolUseRate: 0.35,
		Epoch:       time.Now().UTC().Add(-perfEpochAgo).Truncate(time.Minute),
	}
}

// perfRateCard prices the generator's model so the usage path exercises
// costing rather than reporting everything unpriced.
func perfRateCard() *store.RateCard {
	return &store.RateCard{Rates: map[string]store.ModelRate{
		"claude-sonnet-4-6": {
			Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6,
			CacheWrite5m: 3.75e-6, CacheWrite1h: 6e-6,
		},
	}}
}

// newPerfStore writes the corpus for shape under a temp project dir,
// ingests it, and returns the settled store. The process home is
// redirected so nothing under ~/.mnemo (config, rate card, codex or
// grok roots) leaks into the measurement.
func newPerfStore(tb testing.TB, shape perfShape) *store.Store {
	tb.Helper()
	quietLogs(tb)
	tb.Setenv(store.MnemoHomeEnv, tb.TempDir())
	tb.Cleanup(store.SetRateCard(perfRateCard()))

	projectDir := tb.TempDir()
	if err := perfGenerator(shape, perfSeed).Write(projectDir); err != nil {
		tb.Fatal(err)
	}
	s, err := store.New(fmt.Sprintf("%s/perf.db", tb.TempDir()), projectDir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	s.AwaitStartup()
	if err := s.IngestAll(); err != nil {
		tb.Fatal(err)
	}
	return s
}

// quietLogs silences the store's progress logging for the duration of
// a benchmark: go test prints a benchmark's name before running it and
// its result after, so anything logged in between lands on the result
// line and breaks benchstat's parser.
func quietLogs(tb testing.TB) {
	tb.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	tb.Cleanup(func() { slog.SetDefault(prev) })
}

// perfPayloadBytes renders a tool result the way the MCP handlers do
// (indented JSON) and returns its size. Payload size is the metric that
// matters for a tool answer — it is what lands in an agent's context.
func perfPayloadBytes(tb testing.TB, v any) int {
	tb.Helper()
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		tb.Fatal(err)
	}
	return len(out)
}
