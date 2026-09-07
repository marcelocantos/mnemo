// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// Benchmarks for the hot MCP tools (🎯T165). Run them with
//
//	make bench
//
// which pins the flags the baseline was recorded with; see
// docs/perf/baseline.md for the locked numbers and how to re-lock.
//
// Every benchmark reports three custom metrics beside ns/op:
//
//   - selects/op: SELECT statements issued on the read pool per call,
//     the N+1 detector.
//   - payload-bytes: size of the tool's indented-JSON answer, where the
//     benchmark stands in for a tool. This is what an agent's context
//     pays for. Only the aggregating tools report it: a search answer is
//     NOT byte-stable between processes, because message ids depend on
//     the order sixteen ingest workers happened to write 1,200 files,
//     and a tie in BM25 rank then breaks differently. The hit COUNT is
//     stable, and that is what the search benchmarks lock.
//   - hits/op or rows/op: how much the call returned, so a speed-up
//     that came from returning less is visible as such.

// perfSelects reports SELECT statements per operation from a delta of
// the read-pool counter.
func perfSelects(b *testing.B, before int64) {
	b.Helper()
	b.ReportMetric(float64(store.ReadSelectCount()-before)/float64(b.N), "selects/op")
}

func BenchmarkSearch(b *testing.B) {
	s := newPerfStore(b, perfBenchShape)

	b.Run("messages", func(b *testing.B) {
		b.ReportAllocs()
		before := store.ReadSelectCount()
		var hits int
		for i := 0; i < b.N; i++ {
			res, err := s.Search(perfQuery, perfSearchLimit, "interactive", "",
				perfContext, perfContext, true)
			if err != nil {
				b.Fatal(err)
			}
			hits = len(res)
		}
		perfSelects(b, before)
		b.ReportMetric(float64(hits), "hits/op")
	})

	b.Run("messages_repo_filter", func(b *testing.B) {
		b.ReportAllocs()
		before := store.ReadSelectCount()
		var hits int
		for i := 0; i < b.N; i++ {
			res, err := s.Search(perfQuery, perfSearchLimit, "interactive", "acme/repo-00",
				perfContext, perfContext, true)
			if err != nil {
				b.Fatal(err)
			}
			hits = len(res)
		}
		perfSelects(b, before)
		b.ReportMetric(float64(hits), "hits/op")
	})

	// The default mnemo_search path: every default corpus, message hits
	// with context and enclosing span.
	b.Run("unified_default", func(b *testing.B) {
		b.ReportAllocs()
		before := store.ReadSelectCount()
		var hits int
		opts := store.UnifiedOpts{
			Limit: perfSearchLimit, SessionType: "interactive",
			ContextBefore: perfContext, ContextAfter: perfContext, SubstantiveOnly: true,
		}
		for i := 0; i < b.N; i++ {
			res, err := s.UnifiedSearchOpts(perfQuery, opts, time.Now())
			if err != nil {
				b.Fatal(err)
			}
			hits = len(res.Hits)
		}
		perfSelects(b, before)
		b.ReportMetric(float64(hits), "hits/op")
	})

	b.Run("unified_all", func(b *testing.B) {
		b.ReportAllocs()
		before := store.ReadSelectCount()
		opts := store.UnifiedOpts{
			Kinds: store.AllCorpusKinds(), Limit: perfSearchLimit, SessionType: "interactive",
			ContextBefore: perfContext, ContextAfter: perfContext, SubstantiveOnly: true,
		}
		for i := 0; i < b.N; i++ {
			if _, err := s.UnifiedSearchOpts(perfQuery, opts, time.Now()); err != nil {
				b.Fatal(err)
			}
		}
		perfSelects(b, before)
	})
}

func BenchmarkRecentActivity(b *testing.B) {
	s := newPerfStore(b, perfBenchShape)
	for _, days := range []int{7, perfWindowDays} {
		b.Run(daysName(days), func(b *testing.B) {
			b.ReportAllocs()
			before := store.ReadSelectCount()
			var rows, bytes int
			for i := 0; i < b.N; i++ {
				res, err := s.RecentActivity(days, "")
				if err != nil {
					b.Fatal(err)
				}
				rows = len(res)
				if i == 0 {
					bytes = perfPayloadBytes(b, res)
				}
			}
			perfSelects(b, before)
			b.ReportMetric(float64(rows), "rows/op")
			b.ReportMetric(float64(bytes), "payload-bytes")
		})
	}
}

// timeBucketedUsage names the groupings whose answer depends on the wall
// clock: their period buckets shift as the corpus ages relative to now,
// which moves both the row count (a corpus spanning a day boundary
// produces two day rows, one otherwise) and the digits of each bucket's
// summed cost. Neither number is stable between runs, so these
// groupings report statements issued and nothing else, and payload and
// row counts are left to the groupings that are stable.
var timeBucketedUsage = map[string]bool{"day": true, "block": true}

func BenchmarkUsage(b *testing.B) {
	s := newPerfStore(b, perfBenchShape)
	for _, groupBy := range []string{"day", "model", "repo", "session", "block"} {
		b.Run(groupBy, func(b *testing.B) {
			b.ReportAllocs()
			before := store.ReadSelectCount()
			var rows, bytes int
			for i := 0; i < b.N; i++ {
				res, err := s.Usage(store.UsageParams{Days: perfWindowDays, GroupBy: groupBy})
				if err != nil {
					b.Fatal(err)
				}
				rows = len(res.Rows)
				if i == 0 {
					bytes = perfPayloadBytes(b, res)
				}
			}
			perfSelects(b, before)
			if !timeBucketedUsage[groupBy] {
				b.ReportMetric(float64(rows), "rows/op")
				b.ReportMetric(float64(bytes), "payload-bytes")
			}
		})
	}
}

// BenchmarkIngestTranscript measures indexing one new session file
// through the same path the watcher uses. Each iteration writes a fresh
// single-session transcript into the project dir and runs IngestAll,
// which skips the files already indexed and parses the new one.
func BenchmarkIngestTranscript(b *testing.B) {
	quietLogs(b)
	b.Setenv(store.MnemoHomeEnv, b.TempDir())
	projectDir := b.TempDir()
	s, err := store.New(filepath.Join(b.TempDir(), "ingest.db"), projectDir)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	s.AwaitStartup()

	// One transcript per seed; the shape matches the corpus sessions.
	shape := perfShape{sessions: 1, repos: 1}
	var written int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		before := perfTreeBytes(b, projectDir)
		gen := perfGenerator(shape, perfSeed+int64(i)+1)
		if err := gen.Write(projectDir); err != nil {
			b.Fatal(err)
		}
		written += perfTreeBytes(b, projectDir) - before
		b.StartTimer()
		if err := s.IngestAll(); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(written / int64(b.N))
}

// perfTreeBytes sums the size of every transcript under dir.
func perfTreeBytes(b *testing.B, dir string) int64 {
	b.Helper()
	var n int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	return n
}

func daysName(days int) string {
	return fmt.Sprintf("%dd", days)
}
