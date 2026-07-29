// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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

// TestUnidentifiedRecordsAreQuarantinedNotCounted covers the NULL-key trap
// and the quarantine rule it turned into (🎯T135).
//
// A record with no message id cannot be shown to duplicate anything. Two
// wrong things can be done with it, and this pins against both.
//
// Grouping on the NULL key collapses every such record into ONE row,
// silently discarding an entire provider's corpus — that shows up here as
// a quarantined total below 600.
//
// Counting it in the headline total is the other error: the volume is not
// deduplicated, so it is not comparable with volume that is. Over one real
// month the unkeyed sources carried 62.9 billion input tokens against 64.8
// million from keyed ones. Admitting that to a total is not an
// approximation.
func TestUnidentifiedRecordsAreQuarantinedNotCounted(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)
	now := time.Now().UTC()
	// No message.id anywhere — the shape every non-Claude source has.
	writeJSONL(t, dir, "p", "sess-dedup", []map[string]any{
		unkeyedAsst(now.Add(-3*time.Minute), 100, 1000),
		unkeyedAsst(now.Add(-2*time.Minute), 200, 2000),
		unkeyedAsst(now.Add(-1*time.Minute), 300, 3000),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	res, err := s.Usage(UsageParams{Days: 1, GroupBy: "day"})
	if err != nil {
		t.Fatal(err)
	}

	if res.Total.OutputTokens != 0 || res.Total.CostUSD != 0 {
		t.Errorf("headline total = %d tokens / $%.4f, want zero: undeduplicable "+
			"volume must not be admitted to a total that claims to be deduplicated",
			res.Total.OutputTokens, res.Total.CostUSD)
	}

	if len(res.Uncounted) != 1 {
		t.Fatalf("Uncounted = %+v, want exactly one source; excluded volume "+
			"that is not reported is indistinguishable from absent volume", res.Uncounted)
	}
	u := res.Uncounted[0]
	if u.OutputTokens != 600 {
		t.Errorf("quarantined output = %d, want 600 (3 records counted once "+
			"each); a collapsed NULL key reports 300 or less", u.OutputTokens)
	}
	if u.Records != 3 {
		t.Errorf("quarantined records = %d, want 3", u.Records)
	}
	if u.Reason == "" {
		t.Error("quarantine carries no reason; an unexplained exclusion is " +
			"the failure mode this reports against")
	}
}

// unkeyedAsst builds an assistant record with NO message id, as every
// non-Claude source currently produces.
func unkeyedAsst(ts time.Time, out, in int) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"timestamp": ts.Format(time.RFC3339),
		"message": map[string]any{
			"role":    "assistant",
			"model":   "claude-sonnet-4-6",
			"content": "x",
			"usage": map[string]any{
				"input_tokens":  in,
				"output_tokens": out,
			},
		},
	}
}

// TestPricesAreContemporaneous pins the archive's selection rule.
//
// Applying today's card to last January's tokens is a prochronism — a
// later artifact projected backwards — and it is invisible, because
// per-model prices are stable after release and a blanket recompute is
// therefore usually approximately right. "Usually approximately right,
// silently" is the description of a bug nobody finds.
//
// Two snapshots with deliberately different rates make the selection
// observable: a record from March must price at March's rate even though
// a June card exists and is newer.
func TestPricesAreContemporaneous(t *testing.T) {
	home := t.TempDir()
	t.Setenv(MnemoHomeEnv, home)
	// The archive only applies to a card that was fetched, not installed;
	// an installed card is an override for all dates by design.
	t.Cleanup(SetRateCard(nil))

	dir := filepath.Join(home, pricingArchiveDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(day string, rate float64) {
		card := RateCard{
			FetchedAt: mustDay(t, day),
			Rates:     map[string]ModelRate{"m": {Output: rate}},
		}
		b, err := json.Marshal(card)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pricing-"+day+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-03-01", 10e-6)
	write("2026-06-01", 99e-6) // a later, much dearer card

	resetArchiveCache()

	got, _ := RateCardAsOf(mustDay(t, "2026-04-15")).Rate("m")
	if got.Output != 10e-6 {
		t.Errorf("April priced at %g, want March's 1e-05: the June card is "+
			"newer but did not exist in April", got.Output)
	}

	got, _ = RateCardAsOf(mustDay(t, "2026-07-15")).Rate("m")
	if got.Output != 99e-6 {
		t.Errorf("July priced at %g, want June's 9.9e-05", got.Output)
	}

	// A record older than every snapshot takes the OLDEST card. Reaching
	// forward for a newer one is the error this whole mechanism prevents.
	got, _ = RateCardAsOf(mustDay(t, "2026-01-01")).Rate("m")
	if got.Output != 10e-6 {
		t.Errorf("January priced at %g, want the oldest card's 1e-05", got.Output)
	}
}

// TestFrozenPeriodsDoNotRepriceOnRefetch is the freeze obligation.
//
// It needs no separate machinery: a settled period selects the same
// archived card every time, so recomputing it is idempotent. Fetching a
// new, dearer card must not move a figure for a month that has closed.
func TestFrozenPeriodsDoNotRepriceOnRefetch(t *testing.T) {
	home := t.TempDir()
	t.Setenv(MnemoHomeEnv, home)
	t.Cleanup(SetRateCard(nil))

	dir := filepath.Join(home, pricingArchiveDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	card := RateCard{
		FetchedAt: mustDay(t, "2026-03-01"),
		Rates:     map[string]ModelRate{"m": {Output: 10e-6}},
	}
	b, _ := json.Marshal(card)
	os.WriteFile(filepath.Join(dir, "pricing-2026-03-01.json"), b, 0o644)
	resetArchiveCache()

	before, _ := RateCardAsOf(mustDay(t, "2026-03-15")).Rate("m")

	// A price revision lands today.
	card.FetchedAt = mustDay(t, "2026-07-01")
	card.Rates = map[string]ModelRate{"m": {Output: 500e-6}}
	b, _ = json.Marshal(card)
	os.WriteFile(filepath.Join(dir, "pricing-2026-07-01.json"), b, 0o644)
	resetArchiveCache()

	after, _ := RateCardAsOf(mustDay(t, "2026-03-15")).Rate("m")
	if before.Output != after.Output {
		t.Errorf("March repriced from %g to %g when a July card arrived; a "+
			"settled period must not move under a later revision",
			before.Output, after.Output)
	}
}

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// resetArchiveCache drops the memoised index and parsed cards so a test
// that writes new snapshots sees them.
func resetArchiveCache() {
	archiveMu.Lock()
	archiveIndex, archiveRead, archiveCards = nil, time.Time{}, nil
	archiveMu.Unlock()
}
