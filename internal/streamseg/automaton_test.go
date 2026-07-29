// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"fmt"
	"testing"

	"github.com/marcelocantos/mnemo/internal/segment"
)

func mkMsgs(from, n int) []segment.Message {
	out := make([]segment.Message, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, segment.Message{
			ID:        from + i,
			Role:      "user",
			Text:      fmt.Sprintf("message %d with enough text to have some weight behind it", from+i),
			Timestamp: "2026-07-28T00:00:00Z",
		})
	}
	return out
}

// contextCost is what the summariser would be shown for one drip: the
// rolling summary, the open spans, and the unsealed tail. It is the
// quantity the bounded-state design exists to bound, so it is the
// quantity the test measures.
func contextCost(a *Automaton) int {
	n := len(a.State().RollingSummary)
	for _, sp := range a.OpenSpans() {
		n += len(sp.Label)
	}
	for _, m := range a.State().Tail {
		n += len(m.Text)
	}
	return n
}

// driveSession runs n messages through the automaton in drips, sealing a
// span every sealEvery messages, and returns the total context offered
// across all drips — the sum the naive design makes quadratic.
func driveSession(t *testing.T, n, drip, sealEvery int) int {
	t.Helper()
	a := New("sess", Config{}, nil)
	total := 0
	openRef := 0
	for start := 1; start <= n; start += drip {
		a.Ingest(mkMsgs(start, drip))
		total += contextCost(a)

		if start/sealEvery > openRef {
			ref := fmt.Sprintf("s%d", openRef)
			a.Apply([]Event{{Kind: EventOpen, Ref: ref, From: max(1, start-sealEvery), Label: "topic"}})
			// Seal well behind the head so the lookahead is satisfied.
			a.Apply([]Event{{Kind: EventSeal, Ref: ref, To: start - 1, Summary: "done"}})
			a.SetRollingSummary("a summary that stands in for everything sealed so far")
			openRef++
		}
	}
	return total
}

// TestBoundedStateIsLinear is the 🎯T132.2 acceptance, measured.
//
// The naive streaming design accumulates the transcript in the
// summariser's context and re-reads it each drip. That is quadratic in
// session length, and prompt caching does not rescue it: a 0.1x
// cache-read on a GROWING prefix, paid once per drip, still sums
// quadratically.
//
// Quadrupling the session length must therefore quadruple the total
// context offered, not multiply it by sixteen. The bar is deliberately
// generous — the point is to catch a return to quadratic growth, not to
// pin a constant.
func TestBoundedStateIsLinear(t *testing.T) {
	small := driveSession(t, 500, 20, 100)
	large := driveSession(t, 2000, 20, 100)

	ratio := float64(large) / float64(small)
	t.Logf("context offered: n=500 -> %d, n=2000 -> %d (ratio %.2f)", small, large, ratio)

	if ratio > 6.0 {
		t.Errorf("4x the messages cost %.2fx the context — that is superlinear; "+
			"quadratic growth would be ~16x and is the failure this guards", ratio)
	}
	if ratio < 2.0 {
		t.Errorf("ratio %.2f is implausibly low — the drive loop probably stopped "+
			"feeding messages, so this test would pass no matter what", ratio)
	}
}

// TestTailStaysBounded pins the mechanism the linearity rests on. If the
// tail grows without limit the automaton is bounded in name only.
func TestTailStaysBounded(t *testing.T) {
	a := New("sess", Config{MaxTail: 10}, nil)
	for start := 1; start <= 500; start += 5 {
		a.Ingest(mkMsgs(start, 5))
		if got := len(a.State().Tail); got > 10 {
			t.Fatalf("tail reached %d, bound is 10", got)
		}
	}
}

// TestSealShrinksWorkingSet: sealing must actually remove the span from
// the working set and advance the watermark, or nothing ever leaves.
func TestSealShrinksWorkingSet(t *testing.T) {
	a := New("sess", Config{SealLookahead: 1}, nil)
	a.Ingest(mkMsgs(1, 20))

	a.Apply([]Event{{Kind: EventOpen, Ref: "s1", From: 1, Label: "first topic"}})
	if len(a.OpenSpans()) != 1 {
		t.Fatal("open did not register")
	}
	sealed := a.Apply([]Event{{Kind: EventSeal, Ref: "s1", To: 10, Summary: "covered"}})
	if len(sealed) != 1 {
		t.Fatalf("seal produced %d spans, want 1", len(sealed))
	}
	if sealed[0].From != 1 || sealed[0].To != 10 {
		t.Errorf("sealed span = [%d,%d], want [1,10]", sealed[0].From, sealed[0].To)
	}
	if len(a.OpenSpans()) != 0 {
		t.Error("span stayed open after sealing — the working set never shrinks")
	}
	if a.State().SealedThrough != 10 {
		t.Errorf("SealedThrough = %d, want 10", a.State().SealedThrough)
	}
}

// TestSealLookaheadHoldsPrematureSeal: a live segmenter cannot see the
// future, so it must not seal the instant the model senses a lull. A
// conversation that circles back would otherwise be split into two spans
// that should have been one.
func TestSealLookaheadHoldsPrematureSeal(t *testing.T) {
	a := New("sess", Config{SealLookahead: 3}, nil)
	a.Ingest(mkMsgs(1, 5))
	a.Apply([]Event{{Kind: EventOpen, Ref: "s1", From: 1, Label: "topic"}})

	// Seal at 4 with only one message (id 5) past it — not enough.
	if sealed := a.Apply([]Event{{Kind: EventSeal, Ref: "s1", To: 4, Summary: "x"}}); len(sealed) != 0 {
		t.Fatal("sealed with insufficient lookahead")
	}
	if len(a.OpenSpans()) != 1 {
		t.Fatal("span should still be open after a held seal")
	}

	// More messages arrive; the same seal now has enough behind it.
	a.Ingest(mkMsgs(6, 5))
	if sealed := a.Apply([]Event{{Kind: EventSeal, Ref: "s1", To: 4, Summary: "x"}}); len(sealed) != 1 {
		t.Fatal("seal should succeed once lookahead is satisfied")
	}
}

// TestIngestSkipsNoiseAndReplayedMessages: recovery replays from the last
// sealed state, so re-offering already-covered messages must be a no-op
// rather than duplicate coverage.
func TestIngestSkipsNoiseAndReplayedMessages(t *testing.T) {
	a := New("sess", Config{}, nil)
	a.State().SealedThrough = 50

	msgs := mkMsgs(45, 10) // ids 45..54; 45-50 already sealed
	msgs[7].IsNoise = true // id 52
	fresh := a.Ingest(msgs)

	for _, m := range fresh {
		if m.ID <= 50 {
			t.Errorf("re-ingested already-sealed message %d", m.ID)
		}
		if m.IsNoise {
			t.Errorf("ingested noise message %d", m.ID)
		}
	}
	if len(fresh) != 3 { // 51, 53, 54
		t.Errorf("fresh = %d messages, want 3", len(fresh))
	}
}

// TestRestartReseedsFromOwnState: when the context budget fills, the
// agent restarts and is re-seeded from the working set — never from the
// transcript. If NoteRestarted did not reset the estimate to the working
// set's size, a long session would restart continuously.
func TestRestartReseedsFromOwnState(t *testing.T) {
	a := New("sess", Config{RestartTokens: 1000, MaxTail: 5}, nil)
	for start := 1; start <= 400; start += 5 {
		a.Ingest(mkMsgs(start, 5))
	}
	if !a.NeedsRestart() {
		t.Fatal("budget should be spent after 400 messages")
	}
	a.SetRollingSummary("short summary")
	a.NoteRestarted()
	if a.NeedsRestart() {
		t.Error("still over budget straight after a re-seed — the automaton would " +
			"restart on every drip forever")
	}
}

// TestSupersedesAreQueuedNotApplied: supersession is an edge the caller
// persists, never a delete. The divergence between what the stream
// believed and what hindsight concludes is the freshness metric.
func TestSupersedesAreQueuedNotApplied(t *testing.T) {
	a := New("sess", Config{SealLookahead: 1}, nil)
	a.Ingest(mkMsgs(1, 20))
	a.Apply([]Event{{Kind: EventOpen, Ref: "s1", From: 1, Label: "early view"}})
	a.Apply([]Event{{Kind: EventSeal, Ref: "s1", To: 5, Summary: "early"}})

	a.Apply([]Event{{Kind: EventSupersede, Ref: "s1", By: "s2", Reason: "later work overturned it"}})
	q := a.Supersedes()
	if len(q) != 1 || q[0].Ref != "s1" || q[0].By != "s2" {
		t.Fatalf("supersede not queued for the caller: %+v", q)
	}
	if got := a.Supersedes(); len(got) != 0 {
		t.Error("draining twice returned the same event — the caller would persist it twice")
	}
}

// TestSealAllOpenSalvagesSpansAtTranscriptEnd is the 🎯T132.4 gap.
//
// Only sealed spans persist, so a span still open when a session stops was
// discarded outright. A session's final stretch is often its most active,
// which made the live tier quiet exactly where it mattered most — and the
// sweep measured every configuration worse than it really was as a result.
func TestSealAllOpenSalvagesSpansAtTranscriptEnd(t *testing.T) {
	a := New("sess", Config{SealLookahead: 99}, nil) // lookahead can never be met
	a.Ingest(mkMsgs(1, 20))
	a.Apply([]Event{{Kind: EventOpen, Ref: "t1", From: 1, Label: "unfinished topic"}})

	// Normal sealing is impossible here: the lookahead will never be
	// satisfied, which is the shape of a session that simply stopped.
	if sealed := a.Apply([]Event{{Kind: EventSeal, Ref: "t1", To: 10, Summary: "x"}}); len(sealed) != 0 {
		t.Fatal("lookahead should have held this seal")
	}

	got := a.SealAllOpen(a.LastTailID())
	if len(got) != 1 {
		t.Fatalf("SealAllOpen returned %d spans, want 1 — the span was lost", len(got))
	}
	if got[0].From != 1 || got[0].To != 20 {
		t.Errorf("span = [%d,%d], want [1,20]", got[0].From, got[0].To)
	}
	if got[0].Label != "unfinished topic" || got[0].Summary == "" {
		t.Errorf("force-sealed span lost its label/summary: %+v", got[0])
	}
	if len(a.OpenSpans()) != 0 {
		t.Error("span still open after SealAllOpen")
	}
}

// TestSealAllOpenIsANoOpWithNothingOpen: Finish is called on every session
// end, so the common case must cost nothing.
func TestSealAllOpenIsANoOpWithNothingOpen(t *testing.T) {
	a := New("sess", Config{}, nil)
	a.Ingest(mkMsgs(1, 5))
	if got := a.SealAllOpen(5); got != nil {
		t.Errorf("expected no spans, got %d", len(got))
	}
}

// TestForceSealStaleRescuesHeldSpans is the 🎯T132.4 under-sealing
// backstop.
//
// The measured failure was not vagueness but SILENCE: the model held
// spans open across whole sessions, and because only sealed spans persist,
// a session hindsight cut into five topics produced one. A crude boundary
// beats no span at all.
func TestForceSealStaleRescuesHeldSpans(t *testing.T) {
	a := New("sess", Config{SealLookahead: 99, MaxOpenSpanMessages: 20, MaxTail: 1000}, nil)
	// Open first, then feed: a span only ages on messages that arrive
	// after it opens.
	a.Ingest(mkMsgs(1, 1))
	a.Apply([]Event{{Kind: EventOpen, Ref: "t1", From: 1, Label: "held open forever"}})
	a.Ingest(mkMsgs(2, 40))

	got := a.ForceSealStale()
	if len(got) != 1 {
		t.Fatalf("force-sealed %d spans, want 1", len(got))
	}
	if got[0].To != 21 {
		t.Errorf("sealed at %d, want 21 — the 20th message after the span opened", got[0].To)
	}
	if len(a.OpenSpans()) != 0 {
		t.Error("stale span still open")
	}
}

// TestStalenessCountsMessagesNotIdGaps is the bug that made the first
// attempt at this WORSE than no backstop (🎯T132.4).
//
// messages.id is a global rowid, so one session's 70 substantive messages
// can span 313 ids. Measuring a span's age as (head - from) therefore
// measured id distance, firing the backstop roughly 4x too eagerly, and
// sealing at (from + cap) landed on an id naming no message at all — a
// boundary that maps to no ordinal position and scores as no span. The
// sweep caught it: Pk went 0.445 -> 0.595 with one session returning the
// no-hypothesis sentinel despite reporting four spans.
func TestStalenessCountsMessagesNotIdGaps(t *testing.T) {
	a := New("sess", Config{SealLookahead: 99, MaxOpenSpanMessages: 5, MaxTail: 1000}, nil)

	// Ids with large gaps, exactly like a real session interleaved with
	// noise and other sessions.
	sparse := []segment.Message{}
	for i, id := range []int{1000, 1050, 1120, 1200, 1310, 1400, 1500} {
		sparse = append(sparse, segment.Message{
			ID: id, Role: "user", Text: fmt.Sprintf("message %d", i),
		})
	}
	a.Ingest(sparse[:1])
	a.Apply([]Event{{Kind: EventOpen, Ref: "t1", From: 1000, Label: "topic"}})

	// Four more messages: under the cap of 5, despite spanning 310 ids.
	a.Ingest(sparse[1:5])
	if got := a.ForceSealStale(); len(got) != 0 {
		t.Fatalf("backstop fired after 4 messages spanning 310 ids; it is measuring "+
			"id distance rather than messages (%+v)", got)
	}

	// Two more crosses the cap.
	a.Ingest(sparse[5:])
	got := a.ForceSealStale()
	if len(got) != 1 {
		t.Fatalf("backstop did not fire after 6 messages past the cap of 5")
	}
	// And it must seal at a REAL message id, or the boundary maps to no
	// position and the span scores as if it never existed.
	real := map[int]bool{}
	for _, m := range sparse {
		real[m.ID] = true
	}
	if !real[got[0].To] {
		t.Errorf("sealed at id %d, which names no message; a boundary at a "+
			"non-existent id maps to no ordinal and scores as no span", got[0].To)
	}
}

// TestForceSealStaleLeavesYoungSpansAlone: the backstop must not trim
// legitimately long topics, only spans the model has clearly abandoned.
func TestForceSealStaleLeavesYoungSpansAlone(t *testing.T) {
	a := New("sess", Config{MaxOpenSpanMessages: 60}, nil)
	a.Ingest(mkMsgs(1, 30))
	a.Apply([]Event{{Kind: EventOpen, Ref: "t1", From: 1, Label: "still running"}})

	if got := a.ForceSealStale(); len(got) != 0 {
		t.Errorf("force-sealed %d young spans; the backstop should only fire on "+
			"spans held far past the observed topic length", len(got))
	}
	if len(a.OpenSpans()) != 1 {
		t.Error("young span was closed")
	}
}
