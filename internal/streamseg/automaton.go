// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"sort"
	"strings"

	"github.com/marcelocantos/mnemo/internal/segment"
)

// The bounded-state contract (🎯T132.2).
//
// The obvious design keeps the transcript in the summariser's context and
// re-reads it each drip. That is QUADRATIC in session length, and prompt
// caching does not rescue it: a 0.1x cache-read on a GROWING prefix, paid
// once per drip, still sums quadratically. Over a 2,500-message session
// the difference is between viable and absurd.
//
// So the automaton holds a bounded working set:
//
//   - a rolling summary of everything already sealed,
//   - the spans currently open,
//   - the unsealed tail of messages.
//
// A sealed span is durable in topic_segments and leaves the working set.
// When the context budget fills, the agent is restarted and re-seeded from
// THIS state rather than from the transcript — so per-drip cost depends on
// the working set, not on how long the conversation has run.
//
// State is what makes that possible, so it is a real type rather than
// scattered fields, and it is serialisable because re-seeding a restarted
// agent and recovering after a crash are the same operation.

// OpenSpan is a span the conversation has not finished with.
type OpenSpan struct {
	Ref     string `json:"ref"`
	From    int    `json:"from"`
	Label   string `json:"label"`
	LastMsg int    `json:"last_msg"`
	// MsgsSince counts substantive messages ingested since this span
	// opened. A COUNT, deliberately, not a difference of message ids:
	// ids are global rowids with gaps, so one session's 70 messages can
	// span 313 ids. Subtracting them measures neither messages nor time.
	MsgsSince int `json:"msgs_since"`
	// StaleAtID is the id of the message at which this span crossed
	// MaxOpenSpanMessages, or 0 while it is still young. Recorded when
	// crossed rather than computed later, because it must be a REAL
	// message id — a span sealed at an id that names no message produces
	// a boundary that maps to no position and scores as no span at all.
	StaleAtID int `json:"stale_at_id"`
}

// State is the automaton's entire working set. Everything not in here has
// either been persisted or is deliberately forgotten.
type State struct {
	SessionID string `json:"session_id"`
	// RollingSummary stands in for every sealed span, so the sealed
	// spans themselves need not be carried.
	RollingSummary string `json:"rolling_summary"`
	// Open spans, keyed by the summariser's local ref.
	Open map[string]*OpenSpan `json:"open"`
	// Tail is the unsealed messages. It is bounded by MaxTail: past
	// that, the oldest are folded into RollingSummary rather than kept.
	Tail []segment.Message `json:"tail"`
	// SealedThrough is the highest message id covered by a sealed span.
	// Recovery resumes from here.
	SealedThrough int `json:"sealed_through"`
	// SealedRefs maps a summariser ref to the extent of the span it
	// sealed, so a later supersede naming that ref can be resolved to a
	// stored row. Bounded to the most recent maxSealedRefs: a model
	// overturning a conclusion does so while it still remembers holding
	// it, and keeping every ref for the life of a session would be one
	// more term growing without limit.
	SealedRefs map[string][2]int `json:"sealed_refs"`
	// ApproxTokens is a running estimate of what has been sent to the
	// current agent, used to decide when to restart it.
	ApproxTokens int `json:"approx_tokens"`
}

// Sealed is a span the automaton has finished with, ready to persist.
type Sealed struct {
	From    int
	To      int
	Label   string
	Summary string
	// SupersededByRef is the ref of the span that overturned this one,
	// resolved to a durable id by the caller that persists it.
	SupersededByRef string
}

// Config bounds the automaton. Every field is a bound, because every
// field exists to stop something growing without limit.
type Config struct {
	// MaxTail is how many unsealed messages are carried before the
	// oldest are folded into the rolling summary. This is the bound that
	// makes cost linear.
	MaxTail int
	// SealLookahead is how many substantive messages on a different
	// topic must arrive before a span may seal. This is the structural
	// layer's lookahead rule (DefaultSealLookahead) finally attached to
	// a decider that understands what the messages mean rather than how
	// far apart they arrived.
	SealLookahead int
	// MaxSummaryChars bounds the rolling summary so folding the tail
	// into it cannot itself become the unbounded term.
	MaxSummaryChars int
	// RestartTokens is the approximate context budget after which the
	// agent is restarted and re-seeded from State.
	RestartTokens int
	// MaxOpenSpanMessages force-seals a span held open for longer than
	// this many messages (🎯T132.4).
	//
	// A backstop, not the mechanism. The prompt asks the model to notice
	// stale spans and seal them, and that is where good boundaries come
	// from; this only guarantees the tier degrades to a coarse span
	// rather than to NOTHING, which is what it did when the model simply
	// held everything open. A crude boundary beats no span: unsealed
	// spans are never persisted, so under-sealing does not produce a
	// vague answer, it produces silence.
	MaxOpenSpanMessages int
}

// Defaults. SealLookahead is now a MEASURED choice (🎯T132.4): the sweep
// over model x drip x K picked sonnet / drip 12 / K=3, at meanPk 0.267
// against a 0.55-0.65 naive baseline. See the operating-point section of
// docs/design/streaming-segmentation.md.
//
// K=3 also happens to match the structural layer's existing rule, so the
// two tiers do not disagree about what "settled" means — but it is kept
// because it measured well, not because it matched.
const (
	DefaultMaxTail         = 40
	DefaultSealLookahead   = 3
	DefaultMaxSummaryChars = 4000
	DefaultRestartTokens   = 120_000
	// OFF by default. The stale-span backstop was measured and made
	// segmentation WORSE: 0.445 -> 0.507 meanPk over the same six
	// sessions, with span count rising 3.3 -> 4.7. The model sealed more
	// and in worse places, so cuts the backstop inserted were noise
	// rather than recovered structure. Kept because the machinery is
	// correct and cheap to re-enable, and because a future prompt might
	// need it; set MaxOpenSpanMessages explicitly to turn it on.
	DefaultMaxOpenSpanMessages = 0
)

func (c Config) withDefaults() Config {
	if c.MaxTail <= 0 {
		c.MaxTail = DefaultMaxTail
	}
	if c.SealLookahead <= 0 {
		c.SealLookahead = DefaultSealLookahead
	}
	if c.MaxSummaryChars <= 0 {
		c.MaxSummaryChars = DefaultMaxSummaryChars
	}
	if c.RestartTokens <= 0 {
		c.RestartTokens = DefaultRestartTokens
	}
	// Zero means disabled, so it is deliberately NOT defaulted upward.
	return c
}

// Automaton applies span events to a bounded state.
type Automaton struct {
	cfg   Config
	state *State
	// pendingSupersedes holds supersede events whose target is a durably
	// stored span rather than one sealed in this batch. Drained by the
	// caller, which has the store needed to resolve a ref to an id.
	pendingSupersedes []Event
}

// New creates an automaton over an existing state, or a fresh one.
func New(sessionID string, cfg Config, resume *State) *Automaton {
	cfg = cfg.withDefaults()
	st := resume
	if st == nil {
		st = &State{SessionID: sessionID, Open: map[string]*OpenSpan{}}
	}
	if st.Open == nil {
		st.Open = map[string]*OpenSpan{}
	}
	if st.SealedRefs == nil {
		st.SealedRefs = map[string][2]int{}
	}
	return &Automaton{cfg: cfg, state: st}
}

// State exposes the working set for persistence and re-seeding.
func (a *Automaton) State() *State { return a.state }

// Ingest folds newly-arrived messages into the tail and returns the drip
// the summariser should be shown: only the new messages, never the
// history. Older tail entries beyond MaxTail are dropped, their content
// having already been reflected in sealed spans or the rolling summary.
func (a *Automaton) Ingest(msgs []segment.Message) []segment.Message {
	var fresh []segment.Message
	for _, m := range msgs {
		if m.IsNoise || m.ID <= a.state.SealedThrough {
			continue
		}
		fresh = append(fresh, m)
	}
	if len(fresh) == 0 {
		return nil
	}
	// Age every open span by the messages that just arrived, and record
	// where each one goes stale. Done here because this is the only
	// place that sees messages in order.
	for _, m := range fresh {
		for _, sp := range a.state.Open {
			sp.MsgsSince++
			if a.cfg.MaxOpenSpanMessages > 0 && sp.StaleAtID == 0 &&
				sp.MsgsSince >= a.cfg.MaxOpenSpanMessages {
				sp.StaleAtID = m.ID
			}
		}
	}

	a.state.Tail = append(a.state.Tail, fresh...)
	if over := len(a.state.Tail) - a.cfg.MaxTail; over > 0 {
		// Dropping, not summarising, the overflow. A message that has
		// aged past the tail bound without any span claiming it is
		// covered by the rolling summary the model maintains; carrying
		// it further is exactly the unbounded growth this design
		// exists to prevent.
		a.state.Tail = append([]segment.Message(nil), a.state.Tail[over:]...)
	}
	a.state.ApproxTokens += approxTokens(fresh)
	return fresh
}

// Apply folds a batch of events into the state and returns the spans
// that sealed as a result, ready to persist.
//
// Sealing is where the working set shrinks: the span leaves Open, the
// tail is trimmed to what follows it, and SealedThrough advances. That
// is the mechanism the linear-cost claim rests on.
func (a *Automaton) Apply(events []Event) []Sealed {
	var sealed []Sealed
	for _, ev := range events {
		switch ev.Kind {
		case EventOpen:
			if _, exists := a.state.Open[ev.Ref]; exists {
				continue // idempotent: re-opening an open span is a no-op
			}
			a.state.Open[ev.Ref] = &OpenSpan{
				Ref: ev.Ref, From: ev.From, Label: ev.Label, LastMsg: ev.From,
			}

		case EventSeal:
			sp, ok := a.state.Open[ev.Ref]
			if !ok {
				continue // sealing an unknown span: nothing to close
			}
			if !a.lookaheadSatisfied(ev.To) {
				// Too early. Hold the span open rather than sealing a
				// topic the conversation may still be inside; the next
				// drip re-offers the seal with more evidence behind it.
				continue
			}
			label := sp.Label
			if strings.TrimSpace(ev.Label) != "" {
				label = ev.Label
			}
			sealed = append(sealed, Sealed{
				From: sp.From, To: ev.To, Label: label, Summary: ev.Summary,
			})
			a.rememberSealedRef(ev.Ref, sp.From, ev.To)
			delete(a.state.Open, ev.Ref)
			if ev.To > a.state.SealedThrough {
				a.state.SealedThrough = ev.To
			}
			a.trimTail()

		case EventReopen:
			// Only meaningful for a span already sealed; the caller
			// un-seals it durably. Represented here by putting it back
			// in the working set so subsequent seals extend it.
			if _, exists := a.state.Open[ev.Ref]; !exists {
				a.state.Open[ev.Ref] = &OpenSpan{
					Ref: ev.Ref, From: ev.From, Label: ev.Label, LastMsg: ev.From,
				}
			}

		case EventSupersede:
			// Queued rather than applied. The target is usually a span
			// sealed in an earlier drip and already durable, so
			// resolving the ref to a stored id needs the store — which
			// this package deliberately does not have. Recorded as an
			// edge, never applied as a delete: the divergence between
			// what the stream believed and what hindsight concludes is
			// the freshness metric, and deleting the loser would delete
			// the measurement.
			a.pendingSupersedes = append(a.pendingSupersedes, ev)
		}
	}
	return sealed
}

// maxSealedRefs bounds the ref->extent memory. Evicting the oldest is
// safe because a supersede that names a span this far back is a model
// revisiting a conclusion it can no longer see, which the batch pass is
// better placed to judge anyway.
const maxSealedRefs = 64

// rememberSealedRef records where a ref's span landed, evicting the
// lowest-numbered entry once the bound is reached.
func (a *Automaton) rememberSealedRef(ref string, from, to int) {
	if len(a.state.SealedRefs) >= maxSealedRefs {
		oldestRef, oldestTo := "", 1<<62
		for r, ext := range a.state.SealedRefs {
			if ext[1] < oldestTo {
				oldestRef, oldestTo = r, ext[1]
			}
		}
		delete(a.state.SealedRefs, oldestRef)
	}
	a.state.SealedRefs[ref] = [2]int{from, to}
}

// SealedExtent resolves a ref to the span extent it sealed, so a caller
// holding the store can attach a supersession edge to the right row.
func (a *Automaton) SealedExtent(ref string) (from, to int, ok bool) {
	ext, ok := a.state.SealedRefs[ref]
	if !ok {
		return 0, 0, false
	}
	return ext[0], ext[1], true
}

// Supersedes drains the supersede events accumulated since the last call.
func (a *Automaton) Supersedes() []Event {
	out := a.pendingSupersedes
	a.pendingSupersedes = nil
	return out
}

// lookaheadSatisfied reports whether enough substantive messages have
// arrived past a proposed seal point to trust it. Without this a span
// seals the moment the model senses a lull, and a conversation that
// circles back gets fragmented into two spans that should have been one.
func (a *Automaton) lookaheadSatisfied(to int) bool {
	past := 0
	for _, m := range a.state.Tail {
		if m.ID > to {
			past++
		}
	}
	return past >= a.cfg.SealLookahead
}

// trimTail drops tail messages now covered by a sealed span, keeping any
// still inside an open one. This is the shrink half of the bounded-state
// contract: without it the tail is append-only and the whole design
// collapses back to linear-in-session context per drip.
func (a *Automaton) trimTail() {
	lowestOpen := a.state.SealedThrough
	for _, sp := range a.state.Open {
		if sp.From < lowestOpen {
			lowestOpen = sp.From
		}
	}
	kept := a.state.Tail[:0]
	for _, m := range a.state.Tail {
		if m.ID > a.state.SealedThrough || m.ID >= lowestOpen {
			kept = append(kept, m)
		}
	}
	a.state.Tail = append([]segment.Message(nil), kept...)
}

// ForceSealStale closes spans the model has held open past
// MaxOpenSpanMessages, sealing each at the message where it went stale
// rather than at the head — the excess is a new topic the model failed to
// open, and handing all of it to the stale span would be wrong in the
// other direction.
//
// A backstop, not the mechanism. Good boundaries come from the prompt
// asking the model to notice stale spans; this only guarantees the tier
// degrades to a coarse span rather than to NOTHING. Under-sealing does not
// produce a vague answer, it produces silence: unsealed spans are never
// persisted.
func (a *Automaton) ForceSealStale() []Sealed {
	var refs []string
	for ref, sp := range a.state.Open {
		if sp.StaleAtID != 0 {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	sort.Strings(refs)

	out := make([]Sealed, 0, len(refs))
	for _, ref := range refs {
		sp := a.state.Open[ref]
		out = append(out, Sealed{
			From:    sp.From,
			To:      sp.StaleAtID,
			Label:   sp.Label,
			Summary: sp.Label,
		})
		a.rememberSealedRef(ref, sp.From, sp.StaleAtID)
		delete(a.state.Open, ref)
		if sp.StaleAtID > a.state.SealedThrough {
			a.state.SealedThrough = sp.StaleAtID
		}
	}
	a.trimTail()
	return out
}

// SealAllOpen closes every remaining open span at `to`, bypassing the
// seal-lookahead, and returns them for persistence.
//
// For use only when the transcript has ENDED. Lookahead exists to avoid
// sealing a topic the conversation might return to; once there is no more
// conversation there is nothing to return, so holding a span open only
// discards it. Before this existed, every span still open when a session
// stopped was silently lost — and a session's final stretch is often its
// most active, so the live tier went quiet exactly where it mattered most.
//
// Spans are sealed with whatever summary the caller supplied via a final
// seal event; those with none fall back to their label, which is thin but
// honest and still beats no span at all.
func (a *Automaton) SealAllOpen(to int) []Sealed {
	if len(a.state.Open) == 0 {
		return nil
	}
	out := make([]Sealed, 0, len(a.state.Open))
	for _, sp := range a.OpenSpans() {
		end := to
		if end < sp.From {
			end = sp.From
		}
		out = append(out, Sealed{
			From:    sp.From,
			To:      end,
			Label:   sp.Label,
			Summary: sp.Label,
		})
		a.rememberSealedRef(sp.Ref, sp.From, end)
		delete(a.state.Open, sp.Ref)
		if end > a.state.SealedThrough {
			a.state.SealedThrough = end
		}
	}
	a.trimTail()
	return out
}

// LastTailID reports the highest message id the automaton has seen, which
// is where a force-seal should land.
func (a *Automaton) LastTailID() int {
	if n := len(a.state.Tail); n > 0 {
		return a.state.Tail[n-1].ID
	}
	return a.state.SealedThrough
}

// NeedsRestart reports whether the agent's context budget is spent. The
// automaton's state is unaffected: restarting is re-seeding a fresh agent
// from the same working set, which is precisely why that set is bounded.
func (a *Automaton) NeedsRestart() bool {
	return a.state.ApproxTokens >= a.cfg.RestartTokens
}

// NoteRestarted resets the per-agent token count after a re-seed.
func (a *Automaton) NoteRestarted() {
	a.state.ApproxTokens = approxCharsToTokens(len(a.state.RollingSummary)) +
		approxTokens(a.state.Tail)
}

// SetRollingSummary replaces the summary that stands in for sealed spans,
// bounded so folding cannot become the unbounded term.
func (a *Automaton) SetRollingSummary(s string) {
	s = strings.TrimSpace(s)
	if len(s) > a.cfg.MaxSummaryChars {
		s = s[:a.cfg.MaxSummaryChars]
	}
	a.state.RollingSummary = s
}

// OpenSpans returns the open set in a stable order, for prompting.
func (a *Automaton) OpenSpans() []OpenSpan {
	out := make([]OpenSpan, 0, len(a.state.Open))
	for _, sp := range a.state.Open {
		out = append(out, *sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// approxTokens estimates tokens for a message batch. Deliberately crude:
// it decides when to restart an agent, and being wrong by a third costs
// one extra restart, not correctness.
func approxTokens(msgs []segment.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Text)
	}
	return approxCharsToTokens(n)
}

func approxCharsToTokens(chars int) int { return chars / 4 }
