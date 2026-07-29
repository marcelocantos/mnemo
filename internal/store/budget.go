// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"sort"
	"time"
)

// Budget reporting against a resetting period (🎯T135).
//
// The premise is that mnemo cannot stop most spend. Sub-agent fan-outs and
// tool-driven work pass through nothing controllable, so the only universal
// capability is observation — every session writes a transcript, and mnemo
// already ingests all of them. Control is partial and lives elsewhere
// (🎯T136); the user closes the gap.
//
// That makes the report the instrument, which sets the bar for it: a
// number alone is not actionable. Three things have to be true.
//
// It must be about the FUTURE. "80% consumed" arrives after the decision
// that caused it. "At this rate the month ends 40% over, on the 19th" is a
// statement you can still do something about.
//
// It must name the CULPRITS. A monthly total tells you that something is
// burning money, not what. When the answer is a fan-out where no single
// agent looks unusual, the aggregate is the only place the shape is
// visible.
//
// It must offer a way to ACT. A culprit resolved to a live pid and a
// working directory can be looked at, attached to, or killed. One that is
// merely a session id cannot.

// BudgetPeriod is one budget cycle, in the configured timezone.
type BudgetPeriod struct {
	Label string    `json:"label"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"` // exclusive
}

// monthlyPeriod returns the calendar month containing now, in loc.
//
// The end is the first instant of the next month rather than the last
// instant of this one: an exclusive bound has no gap to fall through, and
// the alternative is a rounding decision about the final second.
func monthlyPeriod(now time.Time, loc *time.Location) BudgetPeriod {
	n := now.In(loc)
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	return BudgetPeriod{
		Label: start.Format("2006-01"),
		Start: start,
		End:   start.AddDate(0, 1, 0),
	}
}

// BudgetCulprit is one attributed slice of the period's spend.
type BudgetCulprit struct {
	SessionID  string  `json:"session_id"`
	Repo       string  `json:"repo,omitempty"`
	Cwd        string  `json:"cwd,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	PctOfSpend float64 `json:"pct_of_spend"`
	Messages   int     `json:"messages"`

	// PID is the live process holding this session's transcript open, or
	// 0 when the session has finished. A finished session cannot be
	// stopped, so the distinction decides which affordance is even
	// offered.
	PID int `json:"pid,omitempty"`

	// Action names what can be done about this culprit, in plain terms,
	// so the report does not merely accuse.
	Action string `json:"action,omitempty"`
}

// BudgetStatus is the answer to "where am I, and am I heading for
// trouble".
type BudgetStatus struct {
	Period BudgetPeriod `json:"period"`

	CapUSD   float64 `json:"cap_usd"`
	SpentUSD float64 `json:"spent_usd"`
	// SpentPct is consumption so far, against the cap.
	SpentPct float64 `json:"spent_pct"`

	// ElapsedPct is how far through the period we are. Reported next to
	// SpentPct because the pair is the whole judgement: 60% spent is
	// comfortable on day 25 and alarming on day 3.
	ElapsedPct float64 `json:"elapsed_pct"`

	// BurnUSDPerDay is the recent daily rate the projection is built on,
	// measured over BurnWindowDays rather than the whole period. A
	// whole-period average is dominated by however the month started and
	// responds to a change in behaviour far too slowly to warn about it.
	BurnUSDPerDay  float64 `json:"burn_usd_per_day"`
	BurnWindowDays int     `json:"burn_window_days"`

	// ProjectedUSD is where the period lands if the recent rate holds.
	ProjectedUSD float64 `json:"projected_usd"`
	ProjectedPct float64 `json:"projected_pct"`

	// ExhaustionDate is when the cap is projected to be crossed, in the
	// configured timezone. Empty when the projection stays under it — the
	// point of the whole exercise is that this field is usually empty and
	// alarming when it is not.
	ExhaustionDate string `json:"exhaustion_date,omitempty"`

	// Severity is ok | warn | over. "over" means the cap is ALREADY
	// crossed; "warn" means it is projected to be.
	Severity string `json:"severity"`

	// Headline states the situation in one sentence, phrased for someone
	// reading a notification rather than a table.
	Headline string `json:"headline"`

	Culprits []BudgetCulprit `json:"culprits,omitempty"`

	// UnpricedModels and Uncounted carry the reporting obligations
	// forward. A budget figure that quietly omits a newly released model
	// or an entire provider is worse than no figure, because it will be
	// believed.
	UnpricedModels []string          `json:"unpriced_models,omitempty"`
	Uncounted      []UncountedVolume `json:"uncounted,omitempty"`

	// RateCardFetchedAt dates the prices behind every figure above, and
	// is empty when no card is available — in which case SpentUSD is zero
	// because nothing could be priced, NOT because nothing was spent.
	RateCardFetchedAt string `json:"rate_card_fetched_at,omitempty"`
	Priced            bool   `json:"priced"`
}

// BudgetBurnWindowDays is the trailing window the burn rate is measured
// over. A week smooths the weekday/weekend shape without averaging away a
// change that started on Monday.
const BudgetBurnWindowDays = 7

// MaxBudgetCulprits bounds how many sessions the report names. Enough to
// see a fan-out's shape, few enough to read.
const MaxBudgetCulprits = 10

// BudgetStatusNow reports the current period's standing.
//
// now is a parameter rather than time.Now() so the projection arithmetic
// is testable — a function whose output depends on the wall clock can only
// be tested for not crashing.
func (s *Store) BudgetStatusNow(cfg BudgetConfig, now time.Time) (*BudgetStatus, error) {
	loc := cfg.Location()
	period := monthlyPeriod(now, loc)

	st := &BudgetStatus{
		Period:         period,
		CapUSD:         cfg.MonthlyCapUSD,
		BurnWindowDays: BudgetBurnWindowDays,
		Severity:       "ok",
	}
	if card := LoadRateCard(); card != nil {
		st.Priced = true
		if !card.FetchedAt.IsZero() {
			st.RateCardFetchedAt = card.FetchedAt.UTC().Format(time.RFC3339)
		}
	}

	// Spend so far this period.
	spend, err := s.Usage(UsageParams{
		Since:   period.Start.UTC().Format(time.RFC3339),
		Until:   now.UTC().Format(time.RFC3339),
		GroupBy: "day",
	})
	if err != nil {
		return nil, err
	}
	st.SpentUSD = spend.Total.CostUSD
	st.UnpricedModels = spend.UnpricedModels
	st.Uncounted = spend.Uncounted

	// Elapsed fraction of the period. Clamped because a caller may ask
	// about a period that has already closed.
	total := period.End.Sub(period.Start).Seconds()
	elapsed := now.In(loc).Sub(period.Start).Seconds()
	switch {
	case elapsed < 0:
		elapsed = 0
	case elapsed > total:
		elapsed = total
	}
	if total > 0 {
		st.ElapsedPct = 100 * elapsed / total
	}
	if st.CapUSD > 0 {
		st.SpentPct = 100 * st.SpentUSD / st.CapUSD
	}

	// Burn rate over the trailing window, clipped to the period start so
	// that early in a month the rate is not diluted by days belonging to
	// the previous one.
	burnFrom := now.Add(-BudgetBurnWindowDays * 24 * time.Hour)
	if burnFrom.Before(period.Start) {
		burnFrom = period.Start
	}
	burnDays := now.Sub(burnFrom).Hours() / 24
	if burnDays > 0 {
		recent, err := s.Usage(UsageParams{
			Since:   burnFrom.UTC().Format(time.RFC3339),
			Until:   now.UTC().Format(time.RFC3339),
			GroupBy: "day",
		})
		if err != nil {
			return nil, err
		}
		st.BurnUSDPerDay = recent.Total.CostUSD / burnDays
	}

	// Projection: what has been spent, plus the recent rate applied to
	// the days remaining.
	remainingDays := period.End.Sub(now).Hours() / 24
	if remainingDays < 0 {
		remainingDays = 0
	}
	st.ProjectedUSD = st.SpentUSD + st.BurnUSDPerDay*remainingDays
	if st.CapUSD > 0 {
		st.ProjectedPct = 100 * st.ProjectedUSD / st.CapUSD

		switch {
		case st.SpentUSD >= st.CapUSD:
			st.Severity = "over"
		case st.ProjectedPct >= cfg.EffectiveWarnPct():
			st.Severity = "warn"
		}

		// The date the cap is projected to be crossed. Only meaningful
		// while it is still ahead of us and the rate is positive.
		if st.SpentUSD < st.CapUSD && st.BurnUSDPerDay > 0 {
			daysToCap := (st.CapUSD - st.SpentUSD) / st.BurnUSDPerDay
			if daysToCap <= remainingDays {
				when := now.In(loc).Add(time.Duration(daysToCap * float64(24*time.Hour)))
				st.ExhaustionDate = when.Format("2006-01-02")
			}
		}
	}

	st.Headline = budgetHeadline(st)

	// Culprits are only worth gathering when there is something to act
	// on. Naming the top spenders of a healthy month is noise.
	if st.Severity != "ok" {
		st.Culprits, err = s.budgetCulprits(period.Start, now, st.SpentUSD)
		if err != nil {
			return nil, err
		}
	}
	return st, nil
}

// budgetHeadline phrases the standing for a notification.
func budgetHeadline(st *BudgetStatus) string {
	if !st.Priced {
		return "Spend cannot be computed: no rate card. Token counts are " +
			"intact, but every model is unpriced — this is not $0.00 spent. " +
			`Enable pricing with {"pricing": {"enabled": true}}.`
	}
	if st.CapUSD <= 0 {
		return fmt.Sprintf("$%.2f spent in %s. No cap configured, so nothing is being watched.",
			st.SpentUSD, st.Period.Label)
	}
	switch st.Severity {
	case "over":
		return fmt.Sprintf("Over budget: $%.2f of $%.2f (%.0f%%) with %.0f%% of %s left to run.",
			st.SpentUSD, st.CapUSD, st.SpentPct, 100-st.ElapsedPct, st.Period.Label)
	case "warn":
		if st.ExhaustionDate != "" {
			return fmt.Sprintf("At $%.2f/day, %s exceeds its $%.2f cap on %s "+
				"(projected $%.2f, %.0f%%). $%.2f spent so far.",
				st.BurnUSDPerDay, st.Period.Label, st.CapUSD, st.ExhaustionDate,
				st.ProjectedUSD, st.ProjectedPct, st.SpentUSD)
		}
		return fmt.Sprintf("At $%.2f/day, %s is projected to finish at $%.2f (%.0f%% of a $%.2f cap).",
			st.BurnUSDPerDay, st.Period.Label, st.ProjectedUSD, st.ProjectedPct, st.CapUSD)
	default:
		return fmt.Sprintf("$%.2f of $%.2f spent (%.0f%%), %.0f%% through %s. "+
			"At $%.2f/day the period finishes near $%.2f.",
			st.SpentUSD, st.CapUSD, st.SpentPct, st.ElapsedPct, st.Period.Label,
			st.BurnUSDPerDay, st.ProjectedUSD)
	}
}

// budgetCulprits attributes the period's spend to sessions, newest and
// largest first, resolving each to a live process where one exists.
//
// Per-session rather than per-repo because a session is the thing that can
// actually be stopped. A repo is where the spend landed; a pid is
// something you can do about it.
func (s *Store) budgetCulprits(from, to time.Time, totalSpend float64) ([]BudgetCulprit, error) {
	res, err := s.Usage(UsageParams{
		Since:   from.UTC().Format(time.RFC3339),
		Until:   to.UTC().Format(time.RFC3339),
		GroupBy: "session",
	})
	if err != nil {
		return nil, err
	}
	rows := append([]UsageRow(nil), res.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].CostUSD > rows[j].CostUSD })
	if len(rows) > MaxBudgetCulprits {
		rows = rows[:MaxBudgetCulprits]
	}

	live := s.LiveSessions()
	out := make([]BudgetCulprit, 0, len(rows))
	for _, r := range rows {
		if r.CostUSD <= 0 {
			continue
		}
		id := r.SessionID
		if id == "" {
			id = r.Period
		}
		c := BudgetCulprit{
			SessionID: id,
			CostUSD:   r.CostUSD,
			Messages:  r.Messages,
			PID:       live[id],
		}
		if totalSpend > 0 {
			c.PctOfSpend = 100 * r.CostUSD / totalSpend
		}
		c.Repo, c.Cwd = s.sessionLocation(id)
		if c.PID > 0 {
			c.Action = fmt.Sprintf("live (pid %d) — mnemo_session_go to attach, or kill %d to stop it",
				c.PID, c.PID)
		} else {
			c.Action = "finished — spend already incurred, nothing to stop"
		}
		out = append(out, c)
	}
	return out, nil
}

// sessionLocation looks up a session's repo and working directory so a
// culprit can be found rather than merely identified.
func (s *Store) sessionLocation(sessionID string) (repo, cwd string) {
	row := s.readDB.QueryRow(
		`SELECT COALESCE(repo, ''), COALESCE(cwd, '') FROM session_meta WHERE session_id = ?`,
		sessionID)
	_ = row.Scan(&repo, &cwd)
	return repo, cwd
}
