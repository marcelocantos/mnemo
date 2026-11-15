// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package hours turns the owner's inputs and agents' activity into
// attended hours and agent hours per project (🎯T202).
//
// It is pure: the store materialises the events, and this package does
// the arithmetic, so every rule here is testable without a database.
//
// Attended time. Every human input across every thread goes on one
// timeline, so five threads worked in parallel count once. Inputs no
// more than Gap apart form a block; a block runs from Lead before its
// first input to Tail after its last. Within a block, the time up to
// each input is attributed to that input's project: reading the output
// of a thread and writing the next prompt to it is work on that thread,
// so the stretch before a prompt belongs to the prompt. The lead goes to
// the first input and the tail to the last, so a block's attribution
// sums to the block.
//
// Agent time. A session is busy for each minute in which it produced
// output, and a quiet stretch of up to AgentGap between busy minutes is
// bridged — but never across a human input: an agent that finished its
// turn and then waited for the owner was waiting, not working. After an
// input, the session is busy from the input (the agent starts on it at
// once) if its next output follows within AgentGap. A project's agent
// time is the union over its sessions, so parallel agents on one project
// count once; the total is the union over all projects. Agent time while
// the owner was not attending anywhere is reported separately.
package hours

import (
	"math"
	"slices"
	"sort"
	"time"
)

// Params are the knobs of the model. Every report discloses them.
type Params struct {
	// Gap is the longest silence between two inputs that still counts
	// as one stretch of attention.
	Gap time.Duration
	// Lead is attention before a block's first input: reading what
	// prompted it.
	Lead time.Duration
	// Tail is attention after a block's last input: watching the first
	// of the response.
	Tail time.Duration
	// AgentGap is the longest quiet stretch inside one session that is
	// still one stretch of agent work.
	AgentGap time.Duration
}

// Default parameters. Fifteen minutes is the conventional idle cut-off
// for time tracking; it is long enough to read a long answer, and short
// enough that a coffee break ends a block.
const (
	DefaultGap      = 15 * time.Minute
	DefaultLead     = 2 * time.Minute
	DefaultTail     = 5 * time.Minute
	DefaultAgentGap = 5 * time.Minute
)

// DefaultParams returns the default model.
func DefaultParams() Params {
	return Params{Gap: DefaultGap, Lead: DefaultLead, Tail: DefaultTail, AgentGap: DefaultAgentGap}
}

// Input is one human action: a prompt, a command, an answer to an
// agent's question, an interrupt.
type Input struct {
	At      time.Time
	Session string
	Repo    string
}

// AgentMinute is one minute in which a session produced output.
type AgentMinute struct {
	Session string
	Repo    string
	// Minute is Unix time in minutes.
	Minute int64
}

// Totals is the headline row, and the shape of each per-project and
// per-day row.
type Totals struct {
	AttendedHours        float64 `json:"attended_hours"`
	AgentHours           float64 `json:"agent_hours"`
	AgentUnattendedHours float64 `json:"agent_unattended_hours"`
	Inputs               int     `json:"inputs"`
}

// RepoRow is one project's figures.
type RepoRow struct {
	Repo string `json:"repo"`
	Totals
}

// DayRow is one local day's figures.
type DayRow struct {
	Day string `json:"day"`
	Totals
	// Blocks is the number of attention blocks that started that day.
	Blocks int `json:"blocks"`
}

// RepoDayRow is one project's figures on one local day. A filtered
// report sums these; the unfiltered per-day rows are unions instead,
// since agents on two projects at once are one stretch of wall clock.
type RepoDayRow struct {
	Repo string `json:"repo"`
	Day  string `json:"day"`
	Totals
}

// Result is a computed report.
type Result struct {
	Totals
	Blocks    int          `json:"blocks"`
	ByRepo    []RepoRow    `json:"by_repo"`
	ByDay     []DayRow     `json:"by_day"`
	ByRepoDay []RepoDayRow `json:"by_repo_day,omitempty"`
}

// interval is a half-open span [start, end) in milliseconds.
type interval struct{ start, end int64 }

func (iv interval) len() int64 { return iv.end - iv.start }

const msPerMinute = int64(time.Minute / time.Millisecond)

// Compute builds the report for [since, until) in loc. Inputs and
// minutes may extend beyond the range; only the part inside it counts,
// but events just outside still shape the blocks and intervals that
// cross the boundary.
func Compute(p Params, since, until time.Time, loc *time.Location, inputs []Input, minutes []AgentMinute) *Result {
	if loc == nil {
		loc = time.Local
	}
	lo, hi := since.UnixMilli(), until.UnixMilli()
	acc := newAccumulator(lo, hi, loc)

	// Attended: one timeline across every thread.
	sorted := append([]Input(nil), inputs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	gap, lead, tail := p.Gap.Milliseconds(), p.Lead.Milliseconds(), p.Tail.Milliseconds()
	var attended []interval
	blocks := 0
	prevEnd := int64(math.MinInt64)
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1].At.UnixMilli()-sorted[j].At.UnixMilli() <= gap {
			j++
		}
		first, last := sorted[i].At.UnixMilli(), sorted[j].At.UnixMilli()
		// A lead never reaches back into the previous block's tail, so
		// attribution cannot count the same minute twice when Lead+Tail
		// exceeds Gap.
		block := interval{max(first-lead, prevEnd), last + tail}
		prevEnd = block.end
		if first >= lo && first < hi {
			blocks++
			acc.day(first).Blocks++
		}
		attended = append(attended, block)
		// Attribution: the lead and the stretch up to each input go to
		// that input's project; the tail goes to the last input's.
		prev := block.start
		for k := i; k <= j; k++ {
			at := sorted[k].At.UnixMilli()
			acc.attend(sorted[k].Repo, interval{prev, at})
			prev = at
			if at >= lo && at < hi {
				acc.repo(sorted[k].Repo).Inputs++
				acc.repoDay(sorted[k].Repo, at).Inputs++
				acc.day(at).Inputs++
				acc.total.Inputs++
			}
		}
		acc.attend(sorted[j].Repo, interval{last, block.end})
		i = j + 1
	}
	attended = union(attended)
	acc.total.AttendedHours = hoursOf(clipAll(attended, lo, hi))

	// Agent: per-session busy intervals, then unions.
	bySession := map[string][]int64{}
	repoOf := map[string]string{}
	for _, m := range minutes {
		bySession[m.Session] = append(bySession[m.Session], m.Minute)
		repoOf[m.Session] = m.Repo
	}
	humanBySession := map[string][]int64{}
	for _, in := range sorted {
		humanBySession[in.Session] = append(humanBySession[in.Session], in.At.UnixMilli())
	}
	agentByRepo := map[string][]interval{}
	var agentAll []interval
	sessions := make([]string, 0, len(bySession))
	for s := range bySession {
		sessions = append(sessions, s)
	}
	sort.Strings(sessions)
	for _, s := range sessions {
		ivs := sessionIntervals(bySession[s], humanBySession[s], p.AgentGap.Milliseconds())
		agentByRepo[repoOf[s]] = append(agentByRepo[repoOf[s]], ivs...)
		agentAll = append(agentAll, ivs...)
	}
	for repo, ivs := range agentByRepo {
		u := clipAll(union(ivs), lo, hi)
		away := subtract(u, attended)
		row := acc.repo(repo)
		row.AgentHours = hoursOf(u)
		row.AgentUnattendedHours = hoursOf(away)
		for _, iv := range u {
			acc.splitDays(iv, func(day string, ms int64) { acc.repoDayKey(repo, day).AgentHours += msHours(ms) })
		}
		for _, iv := range away {
			acc.splitDays(iv, func(day string, ms int64) { acc.repoDayKey(repo, day).AgentUnattendedHours += msHours(ms) })
		}
	}
	all := clipAll(union(agentAll), lo, hi)
	acc.total.AgentHours = hoursOf(all)
	unattended := subtract(all, attended)
	acc.total.AgentUnattendedHours = hoursOf(unattended)
	for _, iv := range all {
		acc.splitDays(iv, func(day string, ms int64) { acc.dayKey(day).AgentHours += msHours(ms) })
	}
	for _, iv := range unattended {
		acc.splitDays(iv, func(day string, ms int64) { acc.dayKey(day).AgentUnattendedHours += msHours(ms) })
	}
	for _, iv := range clipAll(attended, lo, hi) {
		acc.splitDays(iv, func(day string, ms int64) { acc.dayKey(day).AttendedHours += msHours(ms) })
	}
	return acc.result(blocks)
}

// sessionIntervals turns one session's busy minutes into intervals,
// bridging quiet stretches of up to agentGap that contain no human input.
func sessionIntervals(mins []int64, human []int64, agentGap int64) []interval {
	slices.Sort(mins)
	slices.Sort(human)
	var out []interval
	var cur interval
	open := false
	for _, m := range mins {
		start, end := m*msPerMinute, (m+1)*msPerMinute
		if open && start <= cur.end {
			if end > cur.end {
				cur.end = end
			}
			continue
		}
		if open {
			lastHuman, inGap := latestIn(human, cur.end, start)
			if !inGap && start-cur.end <= agentGap {
				cur.end = end
				continue
			}
			out = append(out, cur)
			if inGap && start-lastHuman <= agentGap {
				start = lastHuman
			}
		}
		cur = interval{start, end}
		open = true
	}
	if open {
		out = append(out, cur)
	}
	return out
}

// latestIn returns the latest t in sorted ts with lo <= t < hi.
func latestIn(ts []int64, lo, hi int64) (int64, bool) {
	i := sort.Search(len(ts), func(i int) bool { return ts[i] >= hi })
	if i > 0 && ts[i-1] >= lo {
		return ts[i-1], true
	}
	return 0, false
}

// union merges overlapping or touching intervals.
func union(ivs []interval) []interval {
	if len(ivs) == 0 {
		return nil
	}
	s := append([]interval(nil), ivs...)
	sort.Slice(s, func(i, j int) bool { return s[i].start < s[j].start })
	out := []interval{s[0]}
	for _, iv := range s[1:] {
		last := &out[len(out)-1]
		if iv.start <= last.end {
			if iv.end > last.end {
				last.end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// subtract returns a minus b; both must be unions (sorted, disjoint).
func subtract(a, b []interval) []interval {
	var out []interval
	j := 0
	for _, iv := range a {
		start := iv.start
		for j < len(b) && b[j].end <= start {
			j++
		}
		k := j
		for k < len(b) && b[k].start < iv.end {
			if b[k].start > start {
				out = append(out, interval{start, b[k].start})
			}
			if b[k].end > start {
				start = b[k].end
			}
			k++
		}
		if start < iv.end {
			out = append(out, interval{start, iv.end})
		}
	}
	return out
}

func clip(iv interval, lo, hi int64) (interval, bool) {
	if iv.start < lo {
		iv.start = lo
	}
	if iv.end > hi {
		iv.end = hi
	}
	return iv, iv.end > iv.start
}

func clipAll(ivs []interval, lo, hi int64) []interval {
	var out []interval
	for _, iv := range ivs {
		if c, ok := clip(iv, lo, hi); ok {
			out = append(out, c)
		}
	}
	return out
}

func hoursOf(ivs []interval) float64 {
	var ms int64
	for _, iv := range ivs {
		ms += iv.len()
	}
	return msHours(ms)
}

func msHours(ms int64) float64 { return float64(ms) / float64(time.Hour/time.Millisecond) }

// accumulator collects per-project, per-day and per-project-day rows.
type accumulator struct {
	lo, hi   int64
	loc      *time.Location
	total    Totals
	repos    map[string]*RepoRow
	days     map[string]*DayRow
	repoDays map[[2]string]*RepoDayRow
}

func newAccumulator(lo, hi int64, loc *time.Location) *accumulator {
	return &accumulator{lo: lo, hi: hi, loc: loc,
		repos: map[string]*RepoRow{}, days: map[string]*DayRow{}, repoDays: map[[2]string]*RepoDayRow{}}
}

func (a *accumulator) repo(name string) *RepoRow {
	r, ok := a.repos[name]
	if !ok {
		r = &RepoRow{Repo: name}
		a.repos[name] = r
	}
	return r
}

func (a *accumulator) dayName(ms int64) string {
	return time.UnixMilli(ms).In(a.loc).Format(time.DateOnly)
}

func (a *accumulator) day(ms int64) *DayRow { return a.dayKey(a.dayName(ms)) }

func (a *accumulator) dayKey(key string) *DayRow {
	d, ok := a.days[key]
	if !ok {
		d = &DayRow{Day: key}
		a.days[key] = d
	}
	return d
}

func (a *accumulator) repoDay(repo string, ms int64) *RepoDayRow {
	return a.repoDayKey(repo, a.dayName(ms))
}

func (a *accumulator) repoDayKey(repo, day string) *RepoDayRow {
	k := [2]string{repo, day}
	r, ok := a.repoDays[k]
	if !ok {
		r = &RepoDayRow{Repo: repo, Day: day}
		a.repoDays[k] = r
	}
	return r
}

// attend attributes the in-range part of iv to repo.
func (a *accumulator) attend(repo string, iv interval) {
	c, ok := clip(iv, a.lo, a.hi)
	if !ok {
		return
	}
	a.repo(repo).AttendedHours += msHours(c.len())
	a.splitDays(c, func(day string, ms int64) { a.repoDayKey(repo, day).AttendedHours += msHours(ms) })
}

// splitDays calls f with the part of iv that falls in each local day.
func (a *accumulator) splitDays(iv interval, f func(day string, ms int64)) {
	for iv.start < iv.end {
		t := time.UnixMilli(iv.start).In(a.loc)
		next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, a.loc).UnixMilli()
		end := min(next, iv.end)
		f(t.Format(time.DateOnly), end-iv.start)
		iv.start = end
	}
}

func (a *accumulator) result(blocks int) *Result {
	res := &Result{Totals: round(a.total), Blocks: blocks}
	for _, r := range a.repos {
		row := *r
		row.Totals = round(row.Totals)
		res.ByRepo = append(res.ByRepo, row)
	}
	sort.Slice(res.ByRepo, func(i, j int) bool {
		x, y := res.ByRepo[i], res.ByRepo[j]
		if x.AttendedHours != y.AttendedHours {
			return x.AttendedHours > y.AttendedHours
		}
		if x.AgentHours != y.AgentHours {
			return x.AgentHours > y.AgentHours
		}
		return x.Repo < y.Repo
	})
	for _, d := range a.days {
		row := *d
		row.Totals = round(row.Totals)
		res.ByDay = append(res.ByDay, row)
	}
	sort.Slice(res.ByDay, func(i, j int) bool { return res.ByDay[i].Day < res.ByDay[j].Day })
	for _, d := range a.repoDays {
		row := *d
		row.Totals = round(row.Totals)
		res.ByRepoDay = append(res.ByRepoDay, row)
	}
	sort.Slice(res.ByRepoDay, func(i, j int) bool {
		x, y := res.ByRepoDay[i], res.ByRepoDay[j]
		if x.Day != y.Day {
			return x.Day < y.Day
		}
		return x.Repo < y.Repo
	})
	return res
}

// round keeps reports readable: hundredths of an hour are 36 seconds,
// finer than the model's own resolution.
func round(t Totals) Totals {
	r := func(h float64) float64 { return float64(int64(h*100+0.5)) / 100 }
	t.AttendedHours = r(t.AttendedHours)
	t.AgentHours = r(t.AgentHours)
	t.AgentUnattendedHours = r(t.AgentUnattendedHours)
	return t
}
