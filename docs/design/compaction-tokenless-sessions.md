# Compacting sessions that carry no token metadata

🎯T131. Companion to [compactor-token-convergence.md](compactor-token-convergence.md),
which describes the token-volume model this corrects.

## The blind spot

The 🎯T72 owed-metric is

```sql
SUM(output_tokens + cache_creation_tokens)
```

over assistant entries past the compaction cursor. Both columns are
generated, extracted from `$.message.usage` in the raw JSONL.

Grok and Codex transcripts record no per-turn usage. Those columns are
therefore zero for every entry, the sum is exactly zero at any conversation
length, and the `>= budget` comparison is false forever. Such a session was
**never owed, at any size**.

Two consequences, and the second is worse than the first:

1. An entire class of sessions was never compacted, so segment search
   served confident pre-correction conclusions from eras whose corrective
   sessions had no spans.
2. `mnemo_compactor_status` reported `backlog: 0` while that class was
   invisible to the query producing the number. The metric was measuring
   its own reachable subset and reporting it as the whole — a self-owned
   gate input.

## The fix

A session with no token metadata is measured by **substantive message
count** instead, converted at `FallbackTokensPerMessage` so the same budget
threshold keeps a comparable meaning.

The discriminator is whether the **session** carries token metadata
(`sess_tokens`), never whether the past-cursor sum is zero. This distinction
is the whole correctness argument: a fully-caught-up Claude session
legitimately sums to zero past its cursor, and keying the fallback on that
would make every compacted session owed again, forever.

`FallbackTokensPerMessage` is measured, not chosen. Across the 22,319
indexed sessions that do carry usage metadata, pooled volume over
substantive message count is ~4,255 tokens/message. (Per-session mean is
13,430 and median 10,731; both are inflated by short sessions that re-cache
a large prompt, so pooled is the estimator that answers "N messages is worth
how many tokens" corpus-wide.) Rounded **down** to 4,000 — the conservative
direction, since a lower rate demands more messages before a session is
owed. At the default 50k budget the floor lands at ~13 substantive messages.

Both sites move together: the predicate inlined in
`SelectCompactionCandidatesSince`, and `AddendaTokens`, its deliberately
duplicated Go-level twin that backs `mnemo_compacted_session`. A version
where those disagreed would tell the user a session held no addenda while
the watcher was busy compacting it.

### The spend guard was the other half of the bug

The runaway backstop read:

```sql
AND (s.sess_tokens = 0 OR s.comp_tokens * 1.0 / s.sess_tokens < ?)
```

`sess_tokens = 0` short-circuits to *allow*. That was harmless only because
such sessions could never be owed anyway. Making them eligible without
touching this would have admitted precisely the sessions that have **no
spend ceiling at all**. The guard now divides by the same estimate the owed
predicate uses, so the 0.10 default ratio applies to them like everything
else.

## Cost policy for deep history

Measured over the live index at the time of writing — sessions with zero
token metadata and at least 13 substantive messages, i.e. exactly those this
change makes eligible:

| Source | Sessions | Substantive messages | Text | ≈ input tokens |
|---|---:|---:|---:|---:|
| codex | 419 | 464,294 | 855 MiB | ~224M |
| grok | 299 | 90,624 | 190 MiB | ~49M |
| claude | 12 | 3,761 | 3 MiB | ~1M |
| **total** | **730** | **558,679** | **~1 GiB** | **~274M** |

Codex is 82% of the cost. It is also the source whose sessions are largest
(~1,100 substantive messages each on average).

**Policy: full LLM summarisation for all 730, drained by the existing
watcher rather than by a bulk backfill.** Three properties make that safe
without a special-cased catch-up path:

- The cursor advances by budget-sized spans, so no session is summarised in
  one shot; a 1,100-message Codex session drains over many scans.
- The 0.10 ratio guard now applies to these sessions (see above), capping
  per-session summariser spend at a tenth of the session's estimated
  volume. This is the actual cost control, and it is enforced per session
  rather than globally.
- The watcher has no recency floor, so history drains without a separate
  backfill job — the same mechanism that handles new sessions handles old
  ones, just more slowly.

Explicitly **not** adopted: a window-projection-only tier for old eras.
It was considered and rejected — projection produces spans without the
label and decision quality the llm layer gives, and the whole reason 🎯T131
exists is that low-quality or absent spans let stale conclusions win. Paying
once for real summaries is the point.

What this does not cover, deliberately: the summariser runs against the
subscription via `claudia`, so the constraint is rate-limit pressure rather
than a dollar figure. If that changes, ~274M input tokens is the number to
price.

## Re-deriving the constant

If the corpus mix shifts materially:

```sql
WITH tok AS (
  SELECT e.session_id, SUM(e.output_tokens + e.cache_creation_tokens) AS t
  FROM entries e WHERE e.type='assistant' GROUP BY e.session_id HAVING t > 0
), msgs AS (
  SELECT session_id, COUNT(*) AS n FROM messages WHERE is_noise = 0 GROUP BY session_id
)
SELECT SUM(tok.t) * 1.0 / SUM(msgs.n) AS pooled_tokens_per_msg
FROM tok JOIN msgs USING (session_id) WHERE msgs.n > 0;
```

## Index note

The fallback counts substantive messages past a cursor, served by
`idx_messages_session_id_substantive ON messages(session_id, id) WHERE
is_noise = 0` — an index-only range scan. That index predates this change:
it was built for the pre-🎯T72 message-count owed-predicate, which is the
same shape this fallback restores for the sessions that need it.

A `LENGTH(raw)` or `LENGTH(text)` volume estimate was rejected for the
opposite reason: neither is indexed, and both force a full scan across
~31k sessions and 1.19M messages on every candidate selection.
