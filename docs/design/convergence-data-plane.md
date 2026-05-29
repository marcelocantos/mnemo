# Convergence-Driven Data Plane

*Status: design note — 2026-05-29.*
*Anchors 🎯T68 (parent) and its sub-targets, starting with 🎯T68.1
(compaction reconciler).*

**Tracking.** Per project convention this note is anchored to a
bullseye target with one sub-target per reconcilable stream. The
targets are the followable unit; this document is the design source
they reference. See `bullseye_list(cwd)` for live status.

---

## TL;DR

Seen as a dataflow engine, mnemo is a set of **derived artifacts**
(an FTS index, compaction spans, a vault export, git/PR/CI mirrors,
image descriptions, patterns) computed from **sources** (transcript
JSONL, git repos, the GitHub API, memory files, the vault).

A *convergence model* defines each derived artifact as a pure
function of its sources and runs a **reconciler** that drives actual
state toward desired state idempotently — regardless of how a gap
arose (new input, a missed event, a crash, a partial write, a
restored backup). Backfill is not a special mode; it is the
steady-state reconciler meeting a cold cursor.

Today mnemo is **about half convergent.** Its cheap, local,
append-only streams (text index, schema) are convergence-shaped
because the cursor model falls out naturally. Its expensive or
external streams — especially the LLM-derived ones — are where
convergence was traded away for cost control, and the trade was made
by **filtering the work set** (recency windows, boot-only backfill)
rather than by **prioritising a reconciler over the full desired
set.** That filtering is what leaves old state permanently divergent.

This note assesses the current state stream-by-stream, names the
gaps, and proposes the unifying abstraction. The highest-leverage
single fix — making compaction predicate-driven rather than
recency-windowed — is carved out as 🎯T68.1 and is independently
shippable.

---

## The convergence model

A convergence (reconciliation) engine has five properties. For each
derived artifact:

1. **Declared desired state** — the artifact is a pure function of
   declared sources. "The index should contain exactly the entries
   implied by the transcripts on disk."
2. **Observable actual state** — the engine can read what is
   currently materialised, and the *gap* between actual and desired
   is queryable.
3. **Diff + repair** — the engine drives actual → desired
   idempotently, independent of the cause of the gap.
4. **Quiescence at the fixed point** — re-running with no input
   change is a no-op.
5. **Self-healing** — missed events, crashes, out-of-band edits, and
   cold starts are all just "actual diverged from desired" and are
   repaired on the next pass. Backfill is not a distinct concept.

The litmus test for any mnemo worker: *if it has not run for a month
and the daemon restarts, does the system reach the same state it
would have reached had it run continuously?* For ingest and schema,
yes. For compaction, no.

---

## Current state, stream by stream

| Stream | Trigger | Cursor / freshness | Convergent? |
|--------|---------|--------------------|-------------|
| Transcript → FTS index | fsnotify + boot re-scan | per-file byte offset (`s.offsets`) | **Yes** (append-grow only) |
| Schema (sqlift) | boot | declared `schema.sql` vs DB | **Yes** (purest) |
| Vault export | 5-min `Sync` | recorded `entity_ts` per note | **Mostly** (no orphan GC) |
| Decisions / images (intra-ingest) | each `IngestAll` | backfill pass over un-scanned rows | **Yes** |
| Git commits | each `IngestAll` (boot) | upsert by sha | Boot-only |
| GitHub PRs/issues | boot (goroutine) | upsert by id, `lastUpdated` | Boot-only |
| CI runs | 5-min poll | upsert by run id | Poll-only |
| **Compaction** | 1-min scan | **recency window (24h)** | **No** |
| CLAUDE.md review | scan | entry-count gate | Poll-only |
| Image descriptions | background worker post-ingest | per-image presence | Partial |

### Convergence-shaped today

**Transcript → index** (`internal/store/store.go:2612` `IngestAll`,
`:3004` `Watch`). The strongest runtime example. A persisted
per-file byte cursor (`s.offsets[path]`) records how far each JSONL
has been ingested; files where `offset >= size` are skipped
(`:2643`), the rest parse forward from the cursor. Boot re-scans
every project dir, so downtime gaps self-heal. Crucially each
`IngestAll` also runs `backfillDecisions`, `backfillImages`,
`backfillGitCommits` (`:2706`–`:2714`) — so when a *new derived
table* is added, old sessions are reconciled into it. Properties 1–5
hold for this stream: the offset is the "how far has actual caught
up" cursor, reconcile-on-boot is the repair pass, and backfill is
catch-up, not a mode.

**Schema (sqlift, T49).** The purest case. `internal/store/schema.sql`
is declared desired state; `mnemo.db` is actual; sqlift diffs and
applies additive DDL under `AllowNone`. The append-only schema
policy is exactly what keeps the diff always-applicable.

**Vault exporter** (`internal/vault/vault.go`, `needsUpdate` /
`writeNote`). The 5-minute `Sync` iterates *all* entities and
rewrites only those whose recorded `entity_ts` is stale — a
reconcile toward "vault reflects index." It even self-heals
out-of-band deletion (file absent → regenerate). Gaps below.

### Not convergent today

These are driven by **time** (poll interval, recency window) or by
**lifecycle** (boot), not by the gap between actual and desired.

---

## The gaps

### Gap 1 — Compaction is poll-windowed, not predicate-driven → 🎯T68.1

The headline gap. The watcher selects candidates with
`WHERE ss.last_msg > now − RecencyWindow`
(`internal/store/compactions.go:279`, default 24h). The doc comment
at `internal/compact/watcher.go:48` is explicit that the window
exists so "long-dead sessions don't keep flowing through the SQL."

That promotes a **scheduling concern (recency)** into a
**correctness boundary (a permanent filter).** The desired state
"every session matching the compaction predicate has a current
compaction span" is never declared and never reconciled; historical
sessions are permanently divergent. The comment at
`watcher.go:138` records that the pre-T59 model produced "zero
compactions over the entire month of May" — that corpus will never
be compacted.

**The fix is not "add a backfill job."** It is to make compaction a
reconciler over a predicate: a session is *owed* a compaction when it
has substantive messages newer than its latest compaction's
`entry_id_to` (or has none). Recency degrades from a `WHERE` filter
to an `ORDER BY` priority. The only legitimate bounds on convergence
are the per-session token budget (already enforced) and scan
throughput (needs a per-scan cap so a cold corpus does not flood the
LLM in one pass). The backlog — owed-but-uncompacted sessions —
becomes observable via `mnemo_compactor_status` rather than silently
abandoned. Full acceptance in 🎯T68.1.

### Gap 2 — External mirrors reconcile on boot or fixed poll, not on divergence

`backfillGitCommits` runs at `IngestAll` (boot); PRs backfill once at
boot (`store.go:2718`, goroutine); CI polls every 5 minutes
(`registry.go:314`). Upsert-by-id makes them idempotent (good), but
the trigger is temporal, not gap-driven — nothing *notices* that a
repo's commits are stale and repairs them. Between triggers, drift is
bounded only by restart cadence and poll interval.

### Gap 3 — No general divergence-detection surface

`Store.BackfillStatuses()` (`store.go:1103`) is a partial step — it
exposes backfill progress. But there is no uniform "what derived
state is currently stale relative to its sources?" query. Without
observability of the gap (property 2), you cannot have a reconciler
that closes it; you can only have workers that hope they ran recently
enough.

### Gap 4 — Asymmetric source handling breaks "derived = f(source)"

The ingest cursor handles only **append-grow.** A pruned/rewritten
JSONL (`offset >= size` on a shrunk file → skipped at `:2643`), a
deleted source (orphaned index rows, never GC'd), and a deleted
entity (orphaned vault note, never GC'd) do not reconcile.

Worse, the schema policy notes that Claude Code prunes transcripts,
so **the index is now authoritative for data whose source has
vanished.** That is a real architectural fork: a pure convergence
model cannot assume "reconcile derived from source" when the source
tier is lossy. The model has to formalise the index itself as a
durable source tier and backups (T61) as its reconcile-from-cold
path — which is *almost* what happens, but implicitly, not by design.
Orphan/deletion GC must be specified rather than absent.

### Gap 5 — Three trigger paradigms instead of one reconciler abstraction

The meta-gap that explains the others. Today:

- **event-driven** — fsnotify transcript + vault watchers
- **poll-driven** — compactor, CI, connection sweeper, backup, review
- **boot-driven** — git/PR backfill

There is no shared notion of a *stream* =
`(inputs, transform, freshness predicate, cursor)` driven by one
scheduler. Each worker reinvents its cadence and its own (often
missing) catch-up logic. That is why the gaps above are scattered
rather than fixed once and for all.

---

## Target shape

"Fundamentally convergence-driven" means one reconciler abstraction
where:

- Each derived stream **declares** its inputs, transform, freshness
  predicate, and cursor.
- A single scheduler drives every stream toward its fixed point.
- Cost/recency caps are **scheduling priority**, never a filter that
  permanently excludes work from the desired set.
- **Backfill disappears** as a distinct concept — a cold cursor and
  steady-state catch-up are the same code path.
- **Divergence is queryable** — per stream, "how far is actual from
  desired?" is a first-class read (extends `BackfillStatuses`).
- The **index is an explicit durable tier**: when source is lossy
  (pruned JSONL), the reconciler's "source of truth" for that data is
  the index + backups, and this is designed, not incidental.
- **Deletion/orphan GC** is part of every stream's contract, not an
  afterthought.

This is the same philosophy the project already applies to its own
*development* — bullseye declares desired states (targets), `/cv`
computes the frontier gap, work reconciles toward it. T68 turns that
philosophy inward on mnemo's runtime data plane. It also matches the
project design principle of *general facilities + feedback loops*: a
uniform reconciler is exactly such a facility.

---

## Roadmap (leaf-first)

1. **🎯T68.1 — compaction reconciler.** Predicate, not recency
   window. Independently shippable; highest leverage; the concrete
   probe that surfaced this note. **Achieved 2026-05-29.** Recency
   floor removed from the candidate predicate (`recency` is now
   `ORDER BY` only); per-scan compaction cap bounds backlog drain;
   `mnemo_compactor_status` surfaces the owed-but-uncompacted backlog.
1a. **🎯T68.2 — compaction advances by a cursor.** **Achieved
   2026-05-29.** `Compact` now reads `Store.ReadSessionAfter` (messages
   past the prior span's `messages.id` cursor) instead of
   `ReadSession(offset 0)`, so a long session drains window-by-window
   to a fixed point instead of stalling on its first 500 messages.
1b. **🎯T68.3 — unify the owed-predicate / Compact cursor key space.**
   **Achieved 2026-05-29.** Verified that `compactions.entry_id_to/from`
   hold `messages.id` (misnamed); the owed-predicate now compares
   `m.id` (not `m.entry_id`), so "owed" ⟺ "Compact yields a span." The
   compaction stream is now fully convergent.

**Compaction-arc boundary.** T68.1–T68.3 together make the compaction
stream a complete, self-healing reconciler — a coherent shippable
increment of T68 (analogous to an MVP boundary). Slices 2–5 below are
independent follow-ups, each its own subsystem and PR.
2. **🎯T68.4 — divergence-detection surface.** **Achieved 2026-05-29.**
   `Store.StreamDivergences()` + the `mnemo_divergence` tool: a uniform
   per-stream actual-vs-desired gap report via a gatherer registry.
   Compaction / transcript-index / doc streams report real gaps;
   not-yet-instrumented streams report `unknown` honestly.
3. **🎯T68.5 — external-mirror reconcilers.** **Achieved 2026-05-29.**
   All three mirror streams (ci, github, commits) move from boot/poll
   to a per-repo reconcile cursor (`mirror_status`) + staleness
   predicate via the `mirrorReconcilers` registry. The 5-min `PollCI`
   ticker and the boot `backfillGitCommits`/`backfillGitHubActivity`
   are replaced by one divergence-driven reconcile worker; the dead
   `PollCI`/`PollGitHubActivity` wrappers are retired. Gap surfaced via
   the `github_mirrors` divergence row.
4. **🎯T68.6 — source-loss + orphan GC** (future; needs its own design
   note). Formalise the index as a durable tier; GC orphaned derived
   rows and vault notes; reconcile pruned/rewritten sources.
5. **🎯T68.7 — unified reconciler abstraction** (capstone). Collapse
   the trigger paradigms into one `(inputs, transform, predicate,
   cursor)` scheduler. Extracted from T68.4–T68.6 once their shapes are
   known, not designed up front.

Each slice is independently shippable and reversible. The capstone
(T68.7) is deliberately last.
