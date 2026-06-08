# Compactor token-volume convergence (🎯T72)

Status: implemented. Supersedes the message-count + idle-timeout
candidate model (🎯T59/🎯T67/🎯T68.1) for *when* a session is compacted.
The cursor-advance mechanics (🎯T68.2/🎯T68.3) are unchanged.

## The throughline

Compactions are the durable, dense layer. Everything past a session's
latest compaction cursor is **addenda** — raw entries pinned by their
position past the cursor. Search and post-analysis sit on top:
compactions win, addenda flow through. Most sessions are converged most
of the time; re-compaction is deliberate, not continuous.

## Why token volume, not message count

LLM cost scales with tokens; retrieval budgets are tokens; "does this
fit a context window" is a token question. Message count was always a
proxy. The metric is

```
SUM(output_tokens + cache_creation_tokens)   -- over assistant entries
```

`output_tokens` is non-overlapping per turn; `cache_creation_tokens` is
the uncached input the model actually processed. Neither double-counts
the way `input_tokens` does (it re-counts the whole prior conversation
each turn), so their sum is the cleanest content-volume measure.

## The unified predicate

A session is **owed** a compaction iff:

```
compactor_internal = 0
AND (ratio guard: cumulative summariser cost < maxBudgetRatio · session cost)
AND addenda_tokens(session) >= AddendaBudgetTokens     -- default 50k
```

where `addenda_tokens` sums the metric over assistant entries past the
cursor. The cursor is `MAX(compactions.entry_id_to)` — a *messages.id*
(the column name is a historical misnomer, 🎯T68.3) — mapped to its
owning `entries.id` so the sum ranges over entries:

```sql
e.id > COALESCE((SELECT m.entry_id FROM messages m WHERE m.id = cursor_msg_id), 0)
```

When no compaction exists the cursor is 0 and the sum is the whole
session. So the **size floor** ("a tiny session has nothing dense to
compress; its raw entries ARE its retrieval form") and the
**re-compaction trigger** are the same measurement over different
ranges. Backed by a covering index:

```sql
CREATE INDEX idx_entries_addenda
  ON entries(session_id, id, output_tokens, cache_creation_tokens)
  WHERE type = 'assistant';
```

There is no recency floor: recency is the `ORDER BY last_msg DESC`
priority, and the per-scan compaction cap drains any historical backlog
over successive scans without starving live work.

## The precise recursion guard

The old guard skipped any candidate whose cwd was under the mnemo repo —
which also excluded genuine dev sessions in mnemo (the reason the
backlog never drained: ~705k lifetime skips against ~200 real
compactions). The claudia summariser now prefixes every compaction
prompt with `store.CompactorMarker`, which lands as the spawned
session's first user message. Ingest detects it (or the legacy
`"You are a session compactor."` signature for historical runs) and sets
`session_meta.compactor_internal = 1`. The candidate query filters on the
flag. Surgically narrow: only sessions whose prompt literally carries the
marker are excluded.

## Search weighting

`compactions_fts` (external-content FTS5 over `summary`) is queried
alongside `messages_fts`. Matching summaries rank **above** transcript
and vault hits, and raw message hits a matched compaction covers
(`entry_id_from < msg.id <= entry_id_to`) are suppressed so the summary
represents them. Uncovered addenda hits past every cursor flow through
unchanged. Callers see strictly denser results without changing their
query shape.

## Retrieval form

`mnemo_compacted_session` / `Store.CompactedView` return the compaction
summaries (durable layer) plus the addenda tail (`ReadSessionAfter` past
the cursor), computed live — addenda are never stored separately.

## Failure ratio

Re-compaction is now rare and load-bearing, so a failure matters far
more than under the old "we'll get it next scan" model. The lifetime
failure tally (failed:compacted ≈ 2.7:1) decomposed into three causes,
all fixed:

- **NUL-byte poison session (dominant).** claudia passes the prompt as
  an argv element; a NUL byte made Go's exec reject it with EINVAL,
  re-failing one session every scan. Fixed by stripping NUL/C0 controls
  from the prompt before spawn.
- **Prose before JSON.** The model sometimes echoes the task ahead of
  the JSON object. `parsePayload` now extracts the outermost `{...}`.
- **Rate-limit / API-outage notices** echoed as prose are classified
  (only after a parse failure) as `ErrLLMUnavailable` → a distinct
  `rate_limited` outcome that triggers a watcher-wide cooldown rather
  than marching every owed session through the same wall. A genuinely
  un-summarisable session backs off per-session with capped exponential
  delay.

`mnemo_compactor_status` surfaces the `rate_limited` outcome and the
failed/compacted ratio (healthy ≤ 0.20).

## Open questions (intentionally not pinned)

- **Budget value.** 50k is a starting point tied to downstream retrieval
  budgets; the metric is fixed, the threshold may evolve.
- **Re-compaction strategy.** Differential append today (summarise the
  window past the cursor). Periodic full re-summary is a future option.
- **Chain consolidation.** Compactions stay session-scoped; a
  chain-level pass that stitches a `/clear`-bounded chain's compactions
  is downstream work (feeds the vault wing, 🎯T64.7–9).

## Schema (all additive, sqlift AllowNone)

- `session_meta.compactor_internal INTEGER NOT NULL DEFAULT 0`
- `compactions_fts` virtual table + `compactions_ai` trigger
- `idx_entries_addenda` covering index

`healCompactionsFTS` rebuilds the external-content index once for
compaction rows that predate the FTS table; idempotent thereafter.
