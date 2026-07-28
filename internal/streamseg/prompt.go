// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"fmt"
	"strings"

	"github.com/marcelocantos/mnemo/internal/segment"
)

// SystemPrompt instructs the streaming summariser.
//
// Two things it must be told that a batch summariser need not be. First,
// it is watching a conversation in progress, so it does not know how any
// topic ends — hence the instruction to hold a span open rather than
// guess. Second, it is the only tier that can see two topics at once, so
// it is explicitly asked for the supersede edge that batch segmentation
// cannot express at all.
const SystemPrompt = `You are segmenting a software engineering conversation into topic spans, live, as it happens.

You will receive the conversation in drips: a few new messages at a time, each prefixed with #<id>. You also hold a rolling summary of what is already settled and a list of spans you have open. You will NOT be shown the earlier messages again, so anything worth remembering belongs in a span summary or the rolling summary.

Reply with ONLY JSONL — one JSON object per line, no prose, no code fences.

Events:
{"event":"open","span":"<ref>","from":<msg id>,"label":"<short topic>"}
{"event":"seal","span":"<ref>","to":<msg id>,"label":"<refined topic>","summary":"<what happened and what was concluded>"}
{"event":"reopen","span":"<ref>","from":<msg id>,"label":"<topic>"}
{"event":"supersede","span":"<ref>","by":"<ref>","reason":"<what was overturned>"}

Rules:

- A span is one topic: a problem pursued, a decision reached, a bug chased. Not one message, and not a whole session.
- Open a span when a new topic starts. Use a short stable ref of your own choosing (t1, t2, ...).
- Seal a span only when the conversation has clearly moved on. If you are unsure whether a topic is finished, leave it open — you will see more. Sealing early splits one topic into two.
- A seal summary should say what was concluded, not merely what was discussed. "Tried X, it failed because Y, switched to Z" beats "discussed X and Z".
- Emit supersede when later work overturns an earlier span's conclusion — a fix that invalidates an earlier diagnosis, a decision that reverses an earlier one. This is the most valuable thing you produce: nothing else in the system can see two topics at once.
- Several spans may be open at once when topics interleave.
- If a drip contains nothing worth a span event, reply with no lines at all.`

// renderDrip builds the user message for one drip: the bounded working
// set, then the new messages.
//
// The working set is restated every drip rather than relied upon as
// conversational memory. It costs a little, and it means a restarted
// agent needs no special first prompt — re-seeding and an ordinary drip
// are the same message, which is what makes restart cheap enough to do
// whenever the budget fills.
func renderDrip(a *Automaton, fresh []segment.Message) string {
	var b strings.Builder

	if s := a.State().RollingSummary; s != "" {
		b.WriteString("Settled so far:\n")
		b.WriteString(s)
		b.WriteString("\n\n")
	}

	if open := a.OpenSpans(); len(open) > 0 {
		b.WriteString("Spans you have open:\n")
		for _, sp := range open {
			fmt.Fprintf(&b, "  %s from #%d — %s\n", sp.Ref, sp.From, sp.Label)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("No spans open.\n\n")
	}

	b.WriteString("New messages:\n")
	for _, m := range fresh {
		text := m.Text
		if len(text) > maxDripMessageChars {
			// Truncating the middle keeps both what a message opened
			// with and how it ended; a head-only truncation loses the
			// conclusion, which is the half a span summary needs.
			half := maxDripMessageChars / 2
			text = text[:half] + "\n…\n" + text[len(text)-half:]
		}
		fmt.Fprintf(&b, "#%d %s: %s\n", m.ID, m.Role, text)
	}
	return b.String()
}

// maxDripMessageChars bounds one message's contribution. A single pasted
// log can otherwise exceed the whole drip budget by itself, and the
// bounded-state argument holds only if each term is bounded.
const maxDripMessageChars = 4000
