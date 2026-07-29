// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build ccusage

// Validation of mnemo's cost accounting against an independent
// implementation (🎯T135).
//
// Behind a build tag, so `go test ./...` never compiles it and the daemon
// can never invoke it. That is a requirement, not tidiness, for two
// reasons.
//
// It is a Node package. mnemo ships as a single Go binary, and adopting an
// npm dependency to compute numbers it can compute itself would be a real
// erosion of that. As a build-tagged test the dependency exists on a
// developer's machine at the moment they choose to check, and nowhere
// else.
//
// And its shape is wrong for a daemon. ccusage is O(entire corpus) per
// invocation — about 2.2s warm over 9.9 GB here, against a 1.37s raw-read
// floor, with no result cache — where mnemo's ingest is O(new bytes). A
// daemon shelling out for live numbers would re-read the whole corpus
// every tick, competing with its own ingest for I/O.
//
// Run it deliberately:
//
//	go test -tags "sqlite_fts5 ccusage" -run TestCCUsage ./internal/store/ -v
//
// The failure modes this guards against are all silent, and every one of
// them produces a plausible number. That is precisely why the comparison
// has to be a standing test rather than a one-off reassurance.
package store

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ccusageDaily is the subset of ccusage's --json output this compares
// against. Its other fields are deliberately ignored: agreeing on totals
// mnemo does not compute proves nothing about the ones it does.
type ccusageDaily struct {
	Daily []struct {
		// The day, as "2006-01-02". Named "period" in the output, not
		// "date" — a mismatch that silently produced zero comparable days
		// and a passing SKIP, which is why the test fails rather than
		// skips when it ends up comparing nothing.
		Period          string  `json:"period"`
		TotalCost       float64 `json:"totalCost"`
		ModelBreakdowns []struct {
			ModelName           string  `json:"modelName"`
			InputTokens         int64   `json:"inputTokens"`
			OutputTokens        int64   `json:"outputTokens"`
			CacheReadTokens     int64   `json:"cacheReadTokens"`
			CacheCreationTokens int64   `json:"cacheCreationTokens"`
			Cost                float64 `json:"cost"`
		} `json:"modelBreakdowns"`
	} `json:"daily"`
}

// TestCCUsageAgreesOnPerDayPerModelFigures is the regression oracle.
//
// Same local transcripts, so any divergence is arithmetic rather than
// scope — which is what makes the comparison diagnostic. mnemo's counted
// set is Claude-only (every other source is quarantined for want of a
// dedup key), and ccusage reads only ~/.claude, so the two cover the same
// records by construction.
func TestCCUsageAgreesOnPerDayPerModelFigures(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available; this oracle is a developer/CI dependency only")
	}
	home, err := EffectiveHome()
	if err != nil {
		t.Skipf("no effective home: %v", err)
	}
	dbPath := filepath.Join(home, ".mnemo", "mnemo.db")
	if _, err := os.Stat(dbPath); err != nil {
		// No corpus to compare over. This oracle validates against real
		// transcripts; synthetic fixtures were tried and ccusage rejected
		// them for reasons never established, so a hermetic version of
		// this test does not currently exist. Saying so is better than
		// asserting something weaker and calling it validated.
		t.Skipf("no mnemo.db at %s; this oracle runs against a real corpus", dbPath)
	}

	// WHOLE days only, in UTC on both sides.
	//
	// Two harness defects hid here, and both produced divergence that
	// looked like an arithmetic bug.
	//
	// A `Days: N` window starts N*24h ago, i.e. mid-morning, so its first
	// day is a fragment — mnemo saw three hours of one day against
	// ccusage's twenty-four and came out 24x "low".
	//
	// And day bucketing is timezone-dependent (spec §6). ccusage groups in
	// the host's local zone by default; mnemo groups in UTC. At UTC+10
	// that shifts ten hours of volume across every boundary, which is why
	// mnemo read HIGH on some days and LOW on the ones beside them. The
	// same data, bucketed two ways, is not a discrepancy to explain — it
	// is a comparison that was never valid.
	const window = 8
	end := time.Now().UTC().Truncate(24 * time.Hour) // today 00:00 UTC, exclusive
	start := end.AddDate(0, 0, -window)              // whole days only
	since, until := start.Format("2006-01-02"), end.AddDate(0, 0, -1).Format("2006-01-02")

	ref := runCCUsage(t, since, until)
	got := openCorpus(t, dbPath)

	res, err := got.Usage(UsageParams{
		Since:   start.Format(time.RFC3339),
		Until:   end.Format(time.RFC3339),
		GroupBy: "day",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RateCardFetchedAt == "" {
		t.Skip(`no rate card; enable {"pricing": {"enabled": true}} and let the ` +
			`daemon fetch one before running this oracle`)
	}

	// Index mnemo's figures by day. Usage groups by day and re-aggregates
	// models, so compare at that granularity on both sides.
	//
	// Tokens are carried alongside cost because a divergence in cost alone
	// is ambiguous — it could be counting or it could be pricing, and
	// those have completely different causes. Reporting both makes one
	// run identify the layer instead of prompting another run.
	mine := map[string]float64{}
	mineTok := map[string][2]int64{}
	for _, r := range res.Rows {
		mine[r.Period] += r.CostUSD
		t := mineTok[r.Period]
		mineTok[r.Period] = [2]int64{t[0] + r.OutputTokens, t[1] + r.CacheReadTokens}
	}
	for _, u := range res.Uncounted {
		t.Logf("quarantined: %s — %d records, %d cache-read tokens",
			u.Source, u.Records, u.CacheReadTokens)
	}
	if len(res.UnpricedModels) > 0 {
		t.Logf("unpriced models: %v", res.UnpricedModels)
	}
	theirs := map[string]float64{}
	theirsTok := map[string][2]int64{}
	for _, d := range ref.Daily {
		if d.Period < since || d.Period > until {
			continue
		}
		for _, m := range d.ModelBreakdowns {
			// Claude only. ccusage aggregates across agents — one day here
			// mixed claude and codex, codex contributing $2,322 of a $3,756
			// total — and mnemo deliberately quarantines every source that
			// supplies no dedup key. Comparing across that difference
			// measures scope, not arithmetic.
			if !strings.HasPrefix(m.ModelName, "claude-") {
				continue
			}
			theirs[d.Period] += m.Cost
			t := theirsTok[d.Period]
			theirsTok[d.Period] = [2]int64{t[0] + m.OutputTokens, t[1] + m.CacheReadTokens}
		}
	}

	if len(theirs) == 0 {
		t.Fatal("no comparable days from ccusage; the oracle proved nothing. " +
			"A skip here would read as success")
	}

	// The two do NOT agree exactly, and requiring them to would enshrine a
	// defect rather than catch one.
	//
	// Claude Code writes one JSONL record per content block of an assistant
	// message, so a single billable call appears several times with its
	// usage block repeated VERBATIM. Observed directly here: three records
	// sharing msg_011CdRat7Wx2ks8UYjbLHWR2 AND req_011CdRasy7vs3xmx5Szdb2uW,
	// each reporting input=2, output=2157, cache_read=717136, seconds apart
	// with different uuids. One call, three lines. mnemo charges once;
	// ccusage charges more.
	//
	// So the oracle tests the RELATIONSHIP. It is falsifiable in both
	// directions and does not require the reference to be right:
	//
	//   mnemo <= ccusage       — mnemo deduplicates strictly more, so it can
	//                            never legitimately be higher. Exceeding the
	//                            reference means under-deduplicating or
	//                            double counting.
	//   mnemo >= ccusage/3     — mnemo must not collapse arbitrarily far. A
	//                            key that over-matches silently deletes
	//                            spend, which looks exactly like thrift.
	//
	// Days where the two agree closely are days with little duplication,
	// and those are the ones that pin PRICING: same tokens, same cost.
	const tolerance = 0.01
	// Deduplication is worth 1.95x-2.83x by class on this corpus. Three
	// leaves headroom above the largest measured factor while still failing
	// an unbounded collapse.
	const maxDedupFactor = 3.0

	var compared int
	var sumMine, sumTheirs float64
	for day, want := range theirs {
		have, ok := mine[day]
		if !ok {
			t.Errorf("%s: ccusage reports $%.4f of Claude spend, mnemo reports "+
				"nothing for that day", day, want)
			continue
		}
		compared++
		sumMine += have
		sumTheirs += want
		mt, tt := mineTok[day], theirsTok[day]

		if have > want+tolerance {
			t.Errorf("%s: mnemo $%.4f EXCEEDS ccusage $%.4f — mnemo deduplicates "+
				"strictly more, so this is under-deduplication or double counting.\n"+
				"    output tokens: mnemo %d vs ccusage %d\n"+
				"    cache read:    mnemo %d vs ccusage %d",
				day, have, want, mt[0], tt[0], mt[1], tt[1])
			continue
		}
		if want > 0 && have > 0 && have*maxDedupFactor < want {
			t.Errorf("%s: mnemo $%.4f is %.1fx below ccusage $%.4f, past the %.1fx "+
				"deduplication can account for. A key that over-matches deletes "+
				"real spend and reads as thrift.\n"+
				"    output tokens: mnemo %d vs ccusage %d\n"+
				"    cache read:    mnemo %d vs ccusage %d",
				day, have, want/have, want, maxDedupFactor, mt[0], tt[0], mt[1], tt[1])
			continue
		}

		// Where token counts agree, cost must agree too. That isolates
		// pricing from counting, and pricing has no excuse for a gap.
		if mt[0] == tt[0] && mt[1] == tt[1] && math.Abs(have-want) > tolerance {
			t.Errorf("%s: identical token counts but mnemo $%.4f vs ccusage $%.4f "+
				"— a pure PRICING discrepancy (rates, TTL tier, or long-context "+
				"threshold)", day, have, want)
		}
	}
	if compared == 0 {
		t.Fatal("no days compared; the oracle proved nothing")
	}
	t.Logf("compared %d Claude-only days: mnemo $%.2f vs ccusage $%.2f (%.2fx)",
		compared, sumMine, sumTheirs, sumTheirs/sumMine)
}

// runCCUsage invokes the reference implementation and parses its report.
func runCCUsage(t *testing.T, since, until string) ccusageDaily {
	t.Helper()
	// --timezone UTC is what makes the comparison meaningful at all: both
	// sides must agree on where a day starts before they can be asked to
	// agree on what it cost.
	cmd := exec.Command("npx", "-y", "ccusage@latest", "daily", "--json",
		"--timezone", "UTC", "--since", since, "--until", until)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("ccusage failed to run (%v); the oracle is unavailable, "+
			"which is not the same as mnemo being correct", err)
	}
	var ref ccusageDaily
	if err := json.Unmarshal(out, &ref); err != nil {
		t.Fatalf("parse ccusage output: %v", err)
	}
	return ref
}

// openCorpus opens a COPY of the live database.
//
// A copy, for two reasons. The daemon defers additive schema upgrades off
// the open path (🎯T114.1), so a store opened against the live file may
// still be on the running daemon's older schema — which is how this oracle
// first failed, on a column that exists in schema.sql but had not yet
// landed. Applying the migration here fixes that, and doing it to a copy
// means a validation run never mutates the user's production database as a
// side effect.
//
// The copy costs seconds on a multi-GB corpus. This is a deliberately
// invoked developer oracle; seconds is the right thing to spend.
func openCorpus(t *testing.T, path string) *Store {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "oracle.db")

	// -c uses clonefile on APFS where available, so the copy is close to
	// free; elsewhere it is a plain copy.
	if out, err := exec.Command("cp", "-c", path, dst).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("cp", path, dst).CombinedOutput(); err2 != nil {
			t.Skipf("cannot copy corpus: %v / %v (%s)", err, err2, out)
			_ = out2
		}
	}
	// WAL contents matter: recent writes live there, not in the main file.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = exec.Command("cp", path+suffix, dst+suffix).Run()
	}

	if err := applySchema(dst); err != nil {
		t.Skipf("cannot bring the copy up to the current schema: %v", err)
	}
	home, _ := EffectiveHome()
	s, err := New(dst, filepath.Join(home, ".claude", "projects"))
	if err != nil {
		t.Skipf("cannot open corpus copy: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestRefreshRateCardFetchesUpstream exercises the one outbound call in
// cost accounting against the real upstream.
//
// Tagged alongside the oracle because it is a network test, and because
// the oracle needs a card to compare prices at all. Asserts the shape the
// pricing code depends on — the TTL and long-context variants have to be
// present in the fetched data, not merely in the struct that reads it.
func TestRefreshRateCardFetchesUpstream(t *testing.T) {
	card, err := RefreshRateCard(context.Background(), "")
	if err != nil {
		t.Skipf("upstream unreachable: %v", err)
	}
	if len(card.Rates) < 100 {
		t.Fatalf("fetched %d models, expected thousands", len(card.Rates))
	}
	if card.FetchedAt.IsZero() {
		t.Error("card carries no fetch date; contemporaneous pricing needs one")
	}

	// A model known to have both a long-TTL cache-write rate and a
	// long-context tier. If upstream stops publishing these, pricing
	// silently loses two dimensions, so failing here is the point.
	r, ok := card.Rate("claude-sonnet-4-5")
	if !ok {
		t.Fatal("claude-sonnet-4-5 absent from the rate card")
	}
	if r.Input <= 0 || r.Output <= 0 || r.CacheRead <= 0 || r.CacheWrite5m <= 0 {
		t.Errorf("incomplete base rates: %+v", r)
	}
	if r.CacheWrite1h <= 0 {
		t.Error("no long-TTL cache-write rate; 73% of cache-write volume " +
			"prices at this tier, so its absence under-charges most writes")
	}
	t.Logf("fetched %d models; sonnet-4-5 in=%g out=%g read=%g cw5m=%g cw1h=%g threshold=%d",
		len(card.Rates), r.Input, r.Output, r.CacheRead, r.CacheWrite5m,
		r.CacheWrite1h, r.ContextThreshold)
}
