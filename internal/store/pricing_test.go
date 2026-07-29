// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"math"
	"testing"
	"time"
)

// Rates verified against published pricing and an independent
// implementation (🎯T135).
var sonnet45 = ModelRate{
	Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6,
	CacheWrite5m: 3.75e-6, CacheWrite1h: 6e-6,
	InputAbove: 6e-6, OutputAbove: 22.5e-6, CacheReadAbove: 0.6e-6,
	CacheWrite5mAbove: 7.5e-6, CacheWrite1hAbove: 12e-6,
	ContextThreshold: 200000,
}

// opus45 is the model that exposed the whole problem: its input rate is
// $5/M where the old prefix-matching table charged opus-4's $15/M.
var opus45 = ModelRate{
	Input: 5e-6, Output: 25e-6, CacheRead: 0.5e-6,
	CacheWrite5m: 6.25e-6, CacheWrite1h: 10e-6,
	ContextThreshold: 200000,
}

func near(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 0.0001 {
		t.Errorf("%s = %.5f, want %.5f", what, got, want)
	}
}

// TestCostMatchesReferenceImplementation pins the formula against a figure
// computed independently: a day of 8,660 in / 478,062 5m-write / 5,920,470
// read / 220 out on sonnet-4-5 came to $3.5982.
//
// That day is many requests, none of which crossed the long-context
// threshold, so base rates apply throughout. The test prices it with the
// threshold disabled to represent exactly that — pricing the DAY's totals
// against a per-request threshold is the error this guards (see
// TestThresholdIsPerRequestNotPerAggregate).
func TestCostMatchesReferenceImplementation(t *testing.T) {
	perRequest := sonnet45
	perRequest.ContextThreshold = 0
	usd, ok := perRequest.Cost(TokenCounts{
		Input: 8660, CacheWrite5m: 478062, CacheRead: 5920470, Output: 220,
	})
	if !ok {
		t.Fatal("sonnet-4-5 reported unpriced")
	}
	near(t, usd, 3.5982, "cost")
}

// TestThresholdIsPerRequestNotPerAggregate pins the mistake directly.
//
// Summing a day and asking whether the total exceeds 200k treats hundreds
// of ordinary requests as one enormous one. On the day above that doubles
// the bill, because every above-threshold rate on this model is exactly
// twice its base.
func TestThresholdIsPerRequestNotPerAggregate(t *testing.T) {
	day := TokenCounts{Input: 8660, CacheWrite5m: 478062, CacheRead: 5920470, Output: 220}

	asAggregate, _ := sonnet45.Cost(day) // WRONG: threshold sees the day
	noThreshold := sonnet45
	noThreshold.ContextThreshold = 0
	asRequests, _ := noThreshold.Cost(day) // right: no request crossed it

	// Close to 2x, but not exactly: on this model the above-threshold
	// output rate is 1.5x its base while the other classes double, so the
	// blend depends on the mix. The point is the magnitude, not a constant.
	ratio := asAggregate / asRequests
	if ratio < 1.9 || ratio > 2.0 {
		t.Errorf("aggregate pricing inflated %.3fx; expected close to 2x "+
			"(every class but output doubles above the threshold)", ratio)
	}
}

// TestLongTTLCacheWritesPriceHigher is the case a flat cache-write rate
// cannot produce by any choice of constant: 894 in / 372,322 1h-write /
// 8,377,691 read / 259 out on opus-4-5 came to $7.9230, where the same
// tokens at the 5-minute rate give $6.527.
func TestLongTTLCacheWritesPriceHigher(t *testing.T) {
	perRequest := opus45
	perRequest.ContextThreshold = 0 // this figure is a day, not one request
	long, ok := perRequest.Cost(TokenCounts{
		Input: 894, CacheWrite1h: 372322, CacheRead: 8377691, Output: 259,
	})
	if !ok {
		t.Fatal("opus-4-5 reported unpriced")
	}
	near(t, long, 7.9230, "1h-tier cost")

	short, _ := perRequest.Cost(TokenCounts{
		Input: 894, CacheWrite5m: 372322, CacheRead: 8377691, Output: 259,
	})
	near(t, short, 6.5268, "5m-tier cost")
	if long <= short {
		t.Error("the long TTL must cost more than the short one")
	}
}

// TestFlatCacheWriteUnderstatesRatherThanInflates: with no TTL split
// recorded, charge the cheaper tier. Guessing high would inflate a figure
// meant to reconcile against an invoice.
func TestFlatCacheWriteChargesShorterTier(t *testing.T) {
	flat, _ := opus45.Cost(TokenCounts{CacheWriteFlat: 372322})
	short, _ := opus45.Cost(TokenCounts{CacheWrite5m: 372322})
	near(t, flat, short, "flat vs 5m")
}

// TestLongContextReprices: above the threshold every class reprices, and
// the threshold is crossed routinely — single requests here exceed 800k
// cache-read tokens.
func TestLongContextReprices(t *testing.T) {
	under, _ := sonnet45.Cost(TokenCounts{Input: 1000, CacheRead: 100000})
	over, _ := sonnet45.Cost(TokenCounts{Input: 1000, CacheRead: 300000})
	// Same input tokens, but the second crosses 200k so input doubles.
	perInputUnder := under - 100000*0.3e-6
	perInputOver := over - 300000*0.6e-6
	near(t, perInputUnder, 1000*3e-6, "input under threshold")
	near(t, perInputOver, 1000*6e-6, "input over threshold")
}

// TestUnpricedModelIsNotFree is the failure the reference implementation
// has: an unknown model reported at $0.00 for 52,879 tokens. A newly
// released model is exactly the one missing from a rate card and exactly
// the one whose spend matters.
func TestUnpricedModelIsNotFree(t *testing.T) {
	card := &RateCard{Rates: map[string]ModelRate{"known": sonnet45}}
	if _, ok := card.Rate("brand-new-model"); ok {
		t.Fatal("an unknown model resolved to a rate")
	}
	// And an entry that prices nothing must report itself incomplete.
	if _, ok := (ModelRate{}).Cost(TokenCounts{Input: 52879}); ok {
		t.Error("an empty rate reported a complete price; 52,879 tokens would read as free")
	}
}

// TestRateLookupIsExact guards the bug this replaced: prefix matching gave
// every claude-opus-4-* the opus-4 rate, a 3x overcharge on opus-4-5.
func TestRateLookupIsExact(t *testing.T) {
	card := &RateCard{Rates: map[string]ModelRate{"claude-opus-4-5-20251101": opus45}}
	if _, ok := card.Rate("claude-opus-4"); ok {
		t.Error("a prefix matched a full model id")
	}
	if _, ok := card.Rate("claude-opus-4-5"); ok {
		t.Error("a truncated id matched; only the exact identifier may resolve")
	}
	if _, ok := card.Rate("claude-opus-4-5-20251101"); !ok {
		t.Error("the exact identifier failed to resolve")
	}
}

// installTestRateCard seeds the process-wide card so pricing tests do not
// depend on a fetched file.
func installTestRateCard(t *testing.T, rates map[string]ModelRate) {
	t.Helper()
	t.Cleanup(SetRateCard(&RateCard{Rates: rates}))
}

// TestDedupCountsUnidentifiedRecordsOnce is the NULL-key trap.
//
// A record carrying no message id cannot be shown to duplicate anything,
// so it must be counted once. Grouping on a NULL key instead collapses
// every such record into a single row — which silently discards an entire
// provider's corpus and looks exactly like "you used it less".
func TestDedupCountsUnidentifiedRecordsOnce(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	now := time.Now().UTC()
	// Fixtures carry no message.id, which is the shape that broke.
	writeJSONL(t, dir, "p", "sess-dedup", []map[string]any{
		asstTok("one", now.Add(-3*time.Minute).Format(time.RFC3339), 100, 0, 1000),
		asstTok("two", now.Add(-2*time.Minute).Format(time.RFC3339), 200, 0, 2000),
		asstTok("three", now.Add(-1*time.Minute).Format(time.RFC3339), 300, 0, 3000),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	res, err := s.Usage(UsageParams{Days: 1, GroupBy: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Total.OutputTokens; got != 600 {
		t.Errorf("output tokens = %d, want 600 (3 records counted once each); "+
			"a collapsed NULL key would report 300 or less", got)
	}
}
