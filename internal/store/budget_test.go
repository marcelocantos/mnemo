// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// sydney is a zone with a non-zero, non-integer-hour-from-UTC offset, so a
// period boundary computed in the wrong zone lands on a different day
// rather than coincidentally agreeing.
func sydney(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

// TestPeriodResetsOnTheFirstInLocalTime pins the boundary.
//
// A budget "resetting on the 1st" is a claim about a wall clock somewhere,
// and the somewhere has to be stated. On the last day of a month, Sydney is
// already into the new month while UTC is not — so a period computed in UTC
// would report the first hours of a new budget cycle against the old one's
// cap, at exactly the moment the reset is supposed to give you headroom.
func TestPeriodResetsOnTheFirstInLocalTime(t *testing.T) {
	loc := sydney(t)

	// 2026-07-31 22:00 UTC is 2026-08-01 08:00 in Sydney.
	utcInstant := time.Date(2026, 7, 31, 22, 0, 0, 0, time.UTC)

	local := monthlyPeriod(utcInstant, loc)
	if local.Label != "2026-08" {
		t.Errorf("local period = %s, want 2026-08: it is already August in Sydney",
			local.Label)
	}
	if local.Start.Day() != 1 || local.Start.Hour() != 0 {
		t.Errorf("period starts at %s, want midnight on the 1st", local.Start)
	}

	// The same instant read in UTC is still July, which is the point: the
	// answer depends on the zone, so the zone must be configured rather
	// than inherited from whichever machine ran the report.
	if utc := monthlyPeriod(utcInstant, time.UTC); utc.Label != "2026-07" {
		t.Errorf("UTC period = %s, want 2026-07", utc.Label)
	}
}

// TestPeriodEndIsExclusiveAndCoversTheWholeMonth guards the arithmetic the
// elapsed-fraction and projection both divide by.
func TestPeriodEndIsExclusiveAndCoversTheWholeMonth(t *testing.T) {
	loc := sydney(t)
	p := monthlyPeriod(time.Date(2026, 2, 14, 12, 0, 0, 0, loc), loc)
	if p.End.Month() != time.March || p.End.Day() != 1 {
		t.Errorf("end = %s, want 1 March (exclusive bound)", p.End)
	}
	if days := p.End.Sub(p.Start).Hours() / 24; days != 28 {
		t.Errorf("February 2026 measured %v days, want 28", days)
	}
}

// budgetFixture writes daily spend of a known size into a fresh store.
//
// Each day gets one keyed assistant record on a model the test rate card
// prices, so the resulting cost is exact arithmetic rather than an
// approximation to assert loosely against.
func budgetFixture(t *testing.T, days []time.Time, outPerDay int) *Store {
	t.Helper()
	dir := t.TempDir()
	s := newTestStore(t, dir)
	recs := make([]map[string]any, 0, len(days))
	for i, d := range days {
		recs = append(recs, map[string]any{
			"type":      "assistant",
			"timestamp": d.UTC().Format(time.RFC3339),
			"message": map[string]any{
				"role":    "assistant",
				"id":      fmt.Sprintf("msg_budget_%d", i),
				"model":   "claude-sonnet-4-6",
				"content": "x",
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": outPerDay,
				},
			},
		})
	}
	writeJSONL(t, dir, "p", "sess-budget", recs)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestProjectionWarnsBeforeTheCapIsCrossed is the whole point of the
// feature.
//
// A threshold alarm on consumed-so-far fires after the decision that
// caused it. This asserts the opposite: a month that is only a third spent
// but burning fast enough to blow through the cap must warn NOW, and name
// the date it happens.
func TestProjectionWarnsBeforeTheCapIsCrossed(t *testing.T) {
	installTestRateCard(t, testRateCard())
	loc := time.UTC

	// Ten days into a 30-day month, spending steadily.
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	var days []time.Time
	for i := 1; i <= 10; i++ {
		days = append(days, time.Date(2026, 6, i, 12, 0, 0, 0, loc))
	}
	// 1M output tokens/day at $15/M = $15/day → $150 spent, $450 projected.
	s := budgetFixture(t, days, 1_000_000)

	st, err := s.BudgetStatusNow(BudgetConfig{MonthlyCapUSD: 300}, now)
	if err != nil {
		t.Fatal(err)
	}

	if st.SpentUSD >= st.CapUSD {
		t.Fatalf("fixture spent $%.2f of a $%.2f cap; this test must exercise "+
			"the not-yet-crossed case", st.SpentUSD, st.CapUSD)
	}
	if st.Severity != "warn" {
		t.Errorf("severity = %q, want warn: $%.2f spent, projected $%.2f against "+
			"a $%.2f cap. Waiting until the cap is crossed defeats the feature",
			st.Severity, st.SpentUSD, st.ProjectedUSD, st.CapUSD)
	}
	if st.ProjectedUSD <= st.SpentUSD {
		t.Errorf("projection $%.2f does not exceed spend-to-date $%.2f, so it "+
			"is not a projection", st.ProjectedUSD, st.SpentUSD)
	}
	if st.ExhaustionDate == "" {
		t.Error("no exhaustion date on a projection that crosses the cap; " +
			"'you will run out on the 19th' is the actionable half")
	}
	if st.BurnUSDPerDay <= 0 {
		t.Error("burn rate is zero despite ten days of spend")
	}
}

// TestNoCapReportsWithoutAlerting covers the default posture: mnemo
// measures whether or not you have asked it to police anything.
func TestNoCapReportsWithoutAlerting(t *testing.T) {
	installTestRateCard(t, testRateCard())
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	s := budgetFixture(t, []time.Time{
		time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
	}, 1_000_000)

	st, err := s.BudgetStatusNow(BudgetConfig{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Severity != "ok" {
		t.Errorf("severity = %q with no cap configured, want ok", st.Severity)
	}
	if st.SpentUSD <= 0 {
		t.Error("spend not reported; a budget with no cap must still measure")
	}
	if st.ExhaustionDate != "" {
		t.Errorf("exhaustion date %q with no cap to exhaust", st.ExhaustionDate)
	}
}

// TestUnpricedBudgetDoesNotReadAsZeroSpend is the failure mode that would
// be worst in production.
//
// With no rate card, every cost is zero. A report that says "$0.00 of
// $500" is not merely incomplete — it is the single most reassuring thing
// it could say, and it would be wrong. The headline has to distinguish
// "nothing spent" from "nothing priced".
func TestUnpricedBudgetDoesNotReadAsZeroSpend(t *testing.T) {
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	s := budgetFixture(t, []time.Time{
		time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
	}, 1_000_000)

	// After the fixture: newTestStore installs a card for every other
	// test's benefit, and this is the one test that must run without one.
	//
	// Clearing the installed card is not enough — LoadRateCard falls back
	// to the cache file under the mnemo home, so on a developer machine
	// that has ever fetched one this test would silently price everything
	// and assert nothing. Point the home at an empty directory.
	t.Setenv(MnemoHomeEnv, t.TempDir())
	t.Cleanup(SetRateCard(nil))

	st, err := s.BudgetStatusNow(BudgetConfig{MonthlyCapUSD: 300}, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Priced {
		t.Fatal("reported as priced with no rate card installed")
	}
	if st.SpentUSD != 0 {
		t.Errorf("spend = $%.2f with no rate card; expected zero, which is "+
			"exactly why Priced must be reported alongside it", st.SpentUSD)
	}
	if len(st.UnpricedModels) == 0 {
		t.Error("no unpriced models reported despite nothing being priceable")
	}
	// The headline must not be readable as "you have spent nothing".
	for _, bad := range []string{"$0.00 of", "0% "} {
		if strings.Contains(st.Headline, bad) {
			t.Errorf("headline %q reads as zero spend rather than absent prices",
				st.Headline)
		}
	}
	if !strings.Contains(st.Headline, "unpriced") {
		t.Errorf("headline %q does not say why there is no figure", st.Headline)
	}
}

// TestOverBudgetNamesCulprits pins the third requirement: a report that
// accuses without pointing is not actionable.
func TestOverBudgetNamesCulprits(t *testing.T) {
	installTestRateCard(t, testRateCard())
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	var days []time.Time
	for i := 1; i <= 10; i++ {
		days = append(days, time.Date(2026, 6, i, 12, 0, 0, 0, time.UTC))
	}
	s := budgetFixture(t, days, 1_000_000) // $150 spent

	st, err := s.BudgetStatusNow(BudgetConfig{MonthlyCapUSD: 10}, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Severity != "over" {
		t.Fatalf("severity = %q, want over ($%.2f spent against a $10 cap)",
			st.Severity, st.SpentUSD)
	}
	if len(st.Culprits) == 0 {
		t.Fatal("no culprits named while over budget; a monthly total says " +
			"something is burning money, not what")
	}
	c := st.Culprits[0]
	if c.SessionID == "" || c.CostUSD <= 0 {
		t.Errorf("culprit %+v carries no session or no cost", c)
	}
	if c.Action == "" {
		t.Error("culprit carries no action; the report must say whether this " +
			"session can still be stopped")
	}
}
