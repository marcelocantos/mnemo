// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/marcelocantos/mnemo/internal/segment"
	"github.com/marcelocantos/mnemo/internal/store"
)

// The operating-point sweep (🎯T132.4).
//
// The streaming watcher has four knobs — model, effort, drip size, and
// seal-lookahead K — and no evidence about any of them. The prior is that
// low effort wins, because this is extraction rather than hard thinking;
// the sweep exists to falsify that, not to confirm it.
//
// The measurement is a REPLAY, deliberately, and that is what makes it
// possible at all. Scoring boundary placement needs hindsight gold to
// score against, and mnemo already holds 383 llm-method spans over 196
// historical sessions — spans the batch summariser drew with the whole
// window in front of it. Replaying those sessions through the automaton
// as though they were live, then scoring against what hindsight
// concluded, needs no live session, no production watcher, and no writes
// to the real index.

// SweepPoint is one configuration in the grid.
type SweepPoint struct {
	Model         string
	DripSize      int
	SealLookahead int
}

func (p SweepPoint) String() string {
	m := p.Model
	if m == "" {
		m = "default"
	}
	return fmt.Sprintf("model=%s drip=%d K=%d", m, p.DripSize, p.SealLookahead)
}

// GoldSession is a historical session and the boundaries hindsight drew
// over it.
type GoldSession struct {
	SessionID string
	Messages  []store.StreamMessage
	// GoldCuts are the to_msg_id of each llm-method span, ascending.
	GoldCuts []int
}

// SweepResult scores one point against one session.
type SweepResult struct {
	Point      SweepPoint
	SessionID  string
	StreamCuts []int
	GoldCuts   []int
	Pk         float64
	WindowDiff float64
	Spans      int
	Drips      int
	Elapsed    time.Duration
	Err        error
}

// replayStore serves a historical session's messages to the runner and
// collects the spans it seals, without touching any database. The sweep
// must be able to run against the real index without the risk of writing
// experimental spans into it.
type replayStore struct {
	msgs  []store.StreamMessage
	spans []store.StreamSpan
}

func (r *replayStore) SubstantiveMessagesSince(_ string, after, limit int) ([]store.StreamMessage, error) {
	var out []store.StreamMessage
	for _, m := range r.msgs {
		if m.ID > after {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *replayStore) PutStreamSpans(spans []store.StreamSpan) error {
	r.spans = append(r.spans, spans...)
	return nil
}

// StreamSealedThrough reports what this replay has sealed so far. It
// deliberately does not consult the real index: a replay must start from
// nothing, or it would inherit spans from an earlier run and score a
// mixture of the two.
func (r *replayStore) StreamSealedThrough(string) (int, error) {
	max := 0
	for _, sp := range r.spans {
		if sp.ToMsgID > max {
			max = sp.ToMsgID
		}
	}
	return max, nil
}

func (r *replayStore) StreamSpanIDAt(_ string, from, to int) (string, error) {
	for _, sp := range r.spans {
		if sp.FromMsgID == from && sp.ToMsgID == to {
			return fmt.Sprintf("%d-%d", from, to), nil
		}
	}
	return "", nil
}

func (r *replayStore) MarkSuperseded(string, string) error { return nil }

// RunPoint replays one session through one configuration and scores the
// boundaries it produced against the gold ones.
func RunPoint(ctx context.Context, p SweepPoint, g GoldSession, mk func(SweepPoint) Summariser) SweepResult {
	res := SweepResult{Point: p, SessionID: g.SessionID, GoldCuts: g.GoldCuts}
	start := time.Now()

	rs := &replayStore{msgs: g.Messages}
	summ := mk(p)
	defer summ.Close()

	r := &Runner{
		SessionID: g.SessionID,
		Store:     rs,
		Summ:      summ,
		DripSize:  p.DripSize,
		Cfg:       Config{SealLookahead: p.SealLookahead},
	}
	if err := r.Start(); err != nil {
		res.Err = err
		return res
	}

	// Drive to exhaustion. Step returns 0 when the transcript is spent,
	// which for a replay means the session is finished rather than
	// merely quiet.
	//
	// Bounded rather than open-ended. A replay has a known message count,
	// so the number of drips is knowable in advance and an unbounded loop
	// here can only ever mean the cursor stopped advancing — which is
	// exactly what it did the first time this ran. Failing loudly beats
	// hanging a sweep that is otherwise about to spend real money.
	maxDrips := len(g.Messages) + 8
	for res.Drips < maxDrips {
		n, err := r.Step(ctx)
		if err != nil {
			res.Err = err
			break
		}
		if n == 0 {
			break
		}
		res.Drips++
		if ctx.Err() != nil {
			break
		}
	}
	if res.Err == nil && res.Drips >= maxDrips {
		res.Err = fmt.Errorf("replay did not converge after %d drips: the cursor is not advancing", maxDrips)
	}

	for _, sp := range rs.spans {
		res.StreamCuts = append(res.StreamCuts, sp.ToMsgID)
	}
	sort.Ints(res.StreamCuts)
	res.Spans = len(rs.spans)
	res.Elapsed = time.Since(start)
	res.Pk, res.WindowDiff = ScoreCuts(g.Messages, g.GoldCuts, res.StreamCuts)
	return res
}

// ScoreCuts applies the standard segmentation penalties. Both are in
// [0,1] and lower is better; a scorer with no hypothesis to score
// returns 1, the worst possible, rather than 0, which would read as
// perfect.
//
// Cuts arrive as messages.id, which is a GLOBAL rowid, not a position
// within the session. Pk and WindowDiff are defined over a sequence of
// units and walk every index from 0 to n, so feeding them raw ids makes
// n the largest rowid in the database — 1.9 million for a 70-message
// session. That is not merely slow enough to look like a hang (it was);
// the resulting score is meaningless, because almost every window falls
// in the empty space between two ids. Ids are therefore mapped to their
// ordinal position among the session's substantive messages first.
func ScoreCuts(msgs []store.StreamMessage, gold, hyp []int) (pk, wd float64) {
	if len(gold) == 0 {
		return 0, 0
	}
	if len(hyp) == 0 {
		return 1, 1
	}
	ord := make(map[int]int, len(msgs))
	for i, m := range msgs {
		ord[m.ID] = i
	}
	n := len(msgs)
	if n == 0 {
		return 1, 1
	}
	toOrdinals := func(cuts []int) []int {
		var out []int
		for _, c := range cuts {
			if o, ok := ord[c]; ok {
				out = append(out, o)
			}
		}
		sort.Ints(out)
		return out
	}
	g, h := toOrdinals(gold), toOrdinals(hyp)
	if len(g) == 0 {
		return 0, 0
	}
	if len(h) == 0 {
		return 1, 1
	}
	window := n / (2 * len(g))
	if window < 1 {
		window = 1
	}
	return segment.Pk(n, g, h, window), segment.WindowDiff(n, g, h, window)
}

// Aggregate summarises every result for one point across sessions. The
// operating point is chosen on the mean, but the count of failures is
// reported alongside: a configuration that scores well on the sessions
// it survived while erroring on half of them is not a better
// configuration, and a mean alone would hide that.
type Aggregate struct {
	Point     SweepPoint
	Sessions  int
	Failures  int
	MeanPk    float64
	MeanWD    float64
	MeanSpans float64
	MeanDrips float64
	TotalTime time.Duration
}

// Aggregate collapses per-session results into one row per point.
func AggregateResults(results []SweepResult) []Aggregate {
	byPoint := map[string]*Aggregate{}
	order := []string{}
	for _, r := range results {
		k := r.Point.String()
		a, ok := byPoint[k]
		if !ok {
			a = &Aggregate{Point: r.Point}
			byPoint[k] = a
			order = append(order, k)
		}
		a.TotalTime += r.Elapsed
		if r.Err != nil {
			a.Failures++
			continue
		}
		a.Sessions++
		a.MeanPk += r.Pk
		a.MeanWD += r.WindowDiff
		a.MeanSpans += float64(r.Spans)
		a.MeanDrips += float64(r.Drips)
	}
	out := make([]Aggregate, 0, len(order))
	for _, k := range order {
		a := byPoint[k]
		if a.Sessions > 0 {
			n := float64(a.Sessions)
			a.MeanPk /= n
			a.MeanWD /= n
			a.MeanSpans /= n
			a.MeanDrips /= n
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MeanPk < out[j].MeanPk })
	return out
}
