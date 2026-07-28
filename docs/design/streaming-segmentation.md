# Streaming topic segmentation

🎯T132. Companion to [compaction-tokenless-sessions.md](compaction-tokenless-sessions.md)
(backfill over history) and the 🎯T64.10/🎯T64.11 segmentation work this
replaces one tier of.

Status: **design**. Recorded before implementation so the decomposition and
the cost argument can be reviewed independently of the code.

## What is wrong today

Three span methods exist, ranked `llm > compaction > structural`
(`SegmentMethodRank`, `internal/store/segments_compaction.go`).

The **batch** tier (`compaction`, `llm`) is good but late and window-bound.
Its windows are cut by token budget and never re-summarised, so:

- a topic straddling a window boundary can never be one span, and
- no window ever sees another, which makes cross-window semantics —
  supersession edges, callbacks to an earlier topic — *structurally*
  impossible, not merely unimplemented.

The **structural** tier is the provisional live-tail layer. Its cadence
heuristics (idle gaps, turn rhythm) were never a segmenter; they are a
proxy for one. It produces spans with no real labels, which is why
`DefaultSegmentExpand` is still `none` — segment expansion is off by
default pending a quality bar that the structural layer cannot clear.

## The shape of the fix

A long-running, low-effort summariser session watches a live transcript and
emits span events as the conversation happens. It replaces the
**structural** tier, not the batch tier. Batch finalisation at session close
still gets the last word, redrawing spans over the sealed region with
hindsight.

Two things follow from being live rather than retrospective:

- It closes a span when the *conversation* closes the topic, not when a
  token budget happens to run out.
- Its rolling state lets it say "this overturns span X" at the moment the
  overturning happens — the cross-window semantics batch cannot express.

## The critical design commitment: bounded state

The naive design accumulates the transcript in the watcher's context and
re-reads it each drip. That is **quadratic** in session length, and prompt
caching does not save it: a 0.1x cache-read on a *growing* prefix, paid once
per drip, still sums quadratically. For a 2,591-entry session that is the
difference between viable and absurd.

So the watcher is a bounded-state streaming automaton:

- **State**: rolling summary + currently-open spans + the unsealed tail.
- **Eviction**: a sealed span leaves the context; it is already durable in
  `topic_segments`.
- **Restart**: when the context budget fills, the watcher restarts *from
  its own emitted state* rather than from the transcript.

That makes cost linear in transcript length. It is the property to test
first, because if it does not hold nothing else matters.

### Premature sealing

Live segmentation cannot see the future, so it will sometimes seal a span
the conversation then returns to. Mitigated by a **seal-lookahead lag**: K
substantive messages on a different topic before a span seals. This is the
structural layer's lookahead rule (`DefaultSealLookahead = 3`) finally
attached to a decider that understands what the messages *mean* rather than
how far apart they arrived.

The residue is handled by supersession and by batch finalisation, not by
pretending the seal was right.

## Supersession is a genuine schema gap

`topic_segments.parent_id` is a **hierarchy** pointer (fine span → enclosing
coarse span). It is not lineage. Nothing in the schema expresses "span B
overturns span A" — no `superseded_by`, no generation counter, no lineage
table.

That has to be added, under the append-only schema policy: a **nullable**
`ADD COLUMN` is permitted under sqlift `AllowNone`, so a nullable
`superseded_by TEXT` referencing `topic_segments.id` is expressible without
any gate being relaxed. Retrieval then ranks or flags superseded spans
rather than deleting them — consistent with how the existing method ranking
leaves weaker structural coverage in place underneath.

## Free quality metric

The stream-vs-finalisation diff is not overhead, it is the measurement.
Where hindsight routinely redraws a streaming boundary, that gap **is** the
cost of freshness, per session, for nothing.

`Pk` and `WindowDiff` already exist in `internal/segment/structural.go`,
fully implemented and tested, with no production caller — a scoring harness
with nothing to score. The effort/model sweep gives them their job:
a grid over model tier x effort x drip size x seal-lookahead K, boundaries
scored against high-effort hindsight gold, labels and summaries judged
separately.

The prior is that **low effort wins**: summarisation here is extraction, not
hard thinking. The sweep exists to falsify that, not to confirm it.

## Why this is expressible today

mnemo pins `claudia v0.12.0`, which already ships **Session mode** —
`Start`, `Agent.Send`, `Agent.SubscribeEvents`, `Agent.WaitForResponse`,
backed by a persistent process in a tmux window that survives the consumer
dying. mnemo has never used it: `internal/compact/claudia.go` drives
one-shot `Task` mode exclusively, deliberately ("sessions are not reused
across calls so the summariser stays stateless and trivially terminable").

So a long-lived watcher needs no dependency bump. It is a new integration
surface against an already-vendored API, which is a materially smaller risk
than it first appears.

Which sessions to follow is likewise solved: `Store.LiveSessions()` (🎯T9.5.1)
returns session-ID→PID via `lsof` on the transcript files, already TTL-cached.
Poll it, diff against the watcher's active set, start and stop per session.

## Decomposition

This target is too large to land in one change. Proposed sub-targets, in
dependency order — each independently shippable and independently useful:

1. **Data model.** `method="stream"` in the rank, and a nullable
   `superseded_by` edge with retrieval that flags rather than hides
   superseded spans. Ships with nothing writing stream spans yet.
2. **The watcher.** Bounded-state automaton over claudia Session mode,
   driven by `LiveSessions()`, emitting idempotent open/seal/reopen/supersede
   events, crash-recoverable from last sealed state. The bounded-state
   contract is the acceptance, measured — linear, not quadratic.
3. **Finalisation.** Batch redraws over the sealed region with hindsight and
   supersedes divergent stream spans; the boundary diff is recorded per
   session as the freshness-cost metric.
4. **The sweep.** Grid over model x effort x drip x K, scored with the
   existing Pk/WindowDiff against hindsight gold; chosen operating point
   documented with measured cost per active-session-day. Structural spans
   retire from retrieval only once this clears a stated bar — the same bar
   that has kept `DefaultSegmentExpand` at `none`.

Retiring the structural tier is deliberately last. It is the current
provisional layer, and removing it before stream spans have demonstrably
beaten it would trade a weak signal for no signal.
