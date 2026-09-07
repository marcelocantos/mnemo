// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package streamseg segments a live session into topic spans as the
// conversation happens (🎯T132.2).
//
// The batch segmenter cuts windows by token budget and never revisits
// them, so a topic straddling a window boundary can never be one span and
// no window ever sees another. A watcher that follows the live transcript
// dissolves both: it closes a span when the conversation closes the topic,
// and its rolling state lets it say "this overturns that" at the moment
// the overturning happens.
//
// The package is deliberately split so the interesting half has no
// dependency on claudia or on a database: events.go and automaton.go are
// pure, which is what makes the bounded-state contract testable rather
// than merely asserted.
package streamseg

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// EventKind is the verb of a span event.
type EventKind string

const (
	// EventOpen starts a span at a message. To is not yet known.
	EventOpen EventKind = "open"
	// EventSeal closes a span at a message and gives it its summary.
	// Sealing is the point a span becomes durable and leaves the
	// automaton's working set.
	EventSeal EventKind = "seal"
	// EventReopen un-seals a span the conversation returned to. The
	// seal-lookahead makes this rare, but "rare" is not "never", and a
	// segmenter that cannot admit it was early will either fragment the
	// topic or silently swallow the return.
	EventReopen EventKind = "reopen"
	// EventSupersede records that a later span overturned an earlier
	// one. This is the capability batch segmentation cannot express at
	// all: it requires seeing both spans, and batch windows never see
	// each other.
	EventSupersede EventKind = "supersede"
)

// MsgID is a message id inside an event, accepted as a JSON number or
// as a JSON string holding one.
//
// The prompt asks for `"from":<msg id>`, and a model that answers
// `"from":"3762206"` has understood the question perfectly and answered
// it in a type the parser used to discard. Silently: a quoted id failed
// json.Unmarshal into int, the line was skipped as unparseable, and the
// drip produced no span at all. Measured on the frozen replay sample,
// that one detail was the difference between 20 of 20 drips emitting
// events and 4 of 20 — the whole apparent quality gap between the two
// spawn paths was this, not comprehension.
type MsgID int

func (m *MsgID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	// A quoted id is unquoted and parsed as a number; anything else
	// (an object, an array, a non-numeric string) is still an error, so
	// a genuinely malformed event is still dropped by ParseEvents.
	// The leading '#' comes back because that is how renderDrip labels
	// every message ("#3826596 user: ..."), so a model answering
	// "from":"#3826596" is quoting the notation it was shown.
	s = strings.TrimPrefix(strings.Trim(s, `"`), "#")
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("message id %s: %w", b, err)
	}
	*m = MsgID(n)
	return nil
}

// Event is one span transition emitted by the summariser, one per line
// of JSONL.
//
// Events are idempotent by construction: Ref is derived from the span's
// own extent, so replaying an event stream converges on the same spans
// rather than duplicating them. That is what makes crash recovery a
// matter of replaying from the last sealed state rather than
// reconstructing bookkeeping.
type Event struct {
	Kind EventKind `json:"event"`
	// Ref is the summariser's handle for a span within this stream. It
	// is local to the conversation with the model, not a durable id.
	Ref string `json:"span"`
	// From and To are message ids. From is set on open, To on seal.
	From MsgID `json:"from,omitempty"`
	To   MsgID `json:"to,omitempty"`
	// Label is a short topic name, set on open and refinable on seal.
	Label string `json:"label,omitempty"`
	// Summary is the span's content, set on seal.
	Summary string `json:"summary,omitempty"`
	// By is the superseding span's ref, on supersede.
	By string `json:"by,omitempty"`
	// Reason explains a supersede — what was overturned and by what.
	Reason string `json:"reason,omitempty"`
}

// ParseEvents reads a summariser reply as JSONL span events.
//
// It is forgiving on purpose. The reply comes from a language model
// through a terminal, so it arrives with prose around it, fenced code
// blocks, and occasional blank lines. A strict parser would turn every
// such wobble into a lost drip, and a lost drip is a hole in the span
// coverage that nothing later goes back to fill. Unparseable lines are
// skipped; malformed events are dropped by validation rather than
// failing the batch.
func ParseEvents(reply string) []Event {
	var out []Event
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "```json")
		line = strings.TrimSuffix(line, "```")
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if !ev.valid() {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// valid rejects events that cannot be applied. A span with no ref has
// nothing to address; an open with no start has no extent; a seal with
// no end likewise. Validating here keeps the automaton free of
// defensive checks on data that should never have reached it.
func (e Event) valid() bool {
	if strings.TrimSpace(e.Ref) == "" {
		return false
	}
	switch e.Kind {
	case EventOpen:
		return e.From > 0
	case EventSeal:
		return e.To > 0
	case EventReopen:
		return true
	case EventSupersede:
		return strings.TrimSpace(e.By) != ""
	}
	return false
}

func (e Event) String() string {
	return fmt.Sprintf("%s(%s from=%d to=%d)", e.Kind, e.Ref, e.From, e.To)
}
