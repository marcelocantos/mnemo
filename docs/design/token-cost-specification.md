# Computing token cost from agent transcripts

A specification for turning locally-stored agent transcripts into a cost
figure that reconciles with a provider invoice.

Written for 🎯T135. It is a behavioural specification, derived from
observed inputs and outputs and from published pricing, not from anyone's
implementation — the reference tool used for validation ships as a
compiled binary, so there is no source to have been influenced by.
Everything below is either a fact about the transcript format, a published
price, or an inference stated as such.

---

## 1. Scope

Compute, for a set of local transcript files, the monetary cost of the
model calls they record, aggregated over an arbitrary grouping (day,
session, model, project).

The cost is what a provider would charge at published rates. On a
subscription plan that figure is notional — it measures consumption, not
spend — but it is the only figure comparable to an invoice, so it is the
one to compute.

## 2. Data source

Read only assistant-role records that carry a usage block. Everything else
in a transcript — user turns, tool results, metadata — has no billable
quantity attached and must be ignored rather than estimated.

Each usage block reports **four independently-priced quantities**:

| quantity | meaning |
|---|---|
| input | tokens sent that were **not** served from cache |
| cache creation | tokens written into the prompt cache |
| cache read | tokens served from the prompt cache |
| output | tokens generated |

The first is not the size of the prompt. With prompt caching active it is
routinely two or three tokens against hundreds of thousands of cache
reads. Any model of cost that treats "input" as "the prompt" is wrong by
orders of magnitude in the ordinary case.

### 2.1 Cache creation is two priced tiers, not one

Cache writes carry a time-to-live and the tiers price differently — the
longer TTL costs more. Transcripts report them as separate fields under
the usage block, one per TTL, and a record may carry both.

This is not an edge case. Across one real corpus:

| tier | turns | tokens |
|---|---:|---:|
| 5-minute | 334,218 | 1.39 B |
| 1-hour | 515,700 | 3.82 B |

**73% of cache-write volume was the longer tier.** Flattening the two into
a single total — as the aggregate field in the same usage block invites —
systematically under-prices the majority of cache writes.

### 2.2 Long context is a second pricing dimension

Above a per-model context threshold (200k tokens on current models),
**every class reprices**. Combined with §2.1 this makes cache writes a 2x2
matrix, not a scalar:

| | context <= threshold | context > threshold |
|---|---|---|
| cache write, short TTL | base | above-200k |
| cache write, long TTL | above-1hr | above-1hr-above-200k |

Input, output and cache read each gain their own above-threshold rate.
Given that cache reads here routinely run past 800k tokens in a single
request, the threshold is crossed as a matter of course rather than
exceptionally.

Two further quantities exist in published rate data and are out of scope
until something needs them: a minimum cacheable prompt size, below which
caching does not engage, and a per-query charge for provider-side web
search.

## 3. Per-record cost

For one assistant record:

```
cost = input        × rate.input
     + cacheWrite5m × rate.cacheWrite5m
     + cacheWrite1h × rate.cacheWrite1h
     + cacheRead    × rate.cacheRead
     + output       × rate.output
```

Rates are per token, per model. Nothing is rounded until presentation:
these are sums of millionths over millions of tokens, and rounding early
loses cents per record and dollars per day.

**Verified against a reference implementation.** A single-model day
reporting 8,660 input / 478,062 cache-write / 5,920,470 cache-read / 220
output tokens on a mid-tier model priced at $3.00 / $3.75 / $0.30 / $15.00
per million:

```
  8,660 × 3.00/M  = 0.02598
478,062 × 3.75/M  = 1.79273
5,920,470 × 0.30/M = 1.77614
    220 × 15.00/M = 0.00330
                    ───────
                    3.59815      reference tool: 3.5982
```

Agreement to the fifth decimal.

**A second case proves the tier split.** A day on a different model —
894 input / 372,322 cache-write / 8,377,691 cache-read / 259 output — fits
NO single cache-write rate. Priced with that model's base rates it comes
to $6.527 against a reported $7.9230. The gap closes exactly when the
cache writes are priced at the long-TTL rate:

```
372,322 × $10.00/M = 3.72322      (long TTL, not the $6.25/M base)
    894 ×  $5.00/M = 0.00447
8,377,691 × $0.50/M = 4.18885
    259 × $25.00/M = 0.00648
                     ───────
                     7.92302      reference tool: 7.9230
```

That day's cache writes were entirely long-TTL. A flat cache-write rate
cannot produce this number by any choice of constant, which is what makes
§2.1 a requirement rather than a refinement.

Note also that this model's input rate is $5/M where an intuition from
its product tier would suggest $15/M. Rates must be read from data, never
inferred from a model's name or position in a lineup.

## 4. The rate card

Rates are **per model** and **external**. They are not constants to embed:
models are added, renamed and repriced, and a hardcoded table silently
mis-prices every model added after it was written.

They are also **the only hard part**. Everything else in this document is
arithmetic over data already in hand; the rate card is the piece that has
to come from somewhere and stay current.

Fortunately it is data, not code. A public, community-maintained rate file
covering thousands of models across providers is one HTTP request — 1.6 MB,
2,984 models at the time of writing — and carries every field §2 and §3
require, including the long-context and TTL variants. Fetching that is a
far lighter dependency than adopting a tool, and it is the same upstream
the reference implementation itself falls back to.

Requirements:

- **Keyed by the exact model identifier** the transcript records, including
  any date suffix. Two identifiers differing only by suffix may price
  differently.
- **Cached locally with its fetch date recorded.** The cache is not merely
  an optimisation: a dated snapshot is what makes contemporaneous pricing
  possible at all (§6.1).
- **Fetched behind an explicit opt-in**, like any other outbound call, and
  degrading to the last cached copy rather than to silence.
- **An explicit unpriced state.** A model with no rate must be reported as
  unpriced, with its token counts intact.

That last point is not hypothetical, and the reference implementation gets
it wrong: an unknown model was reported at **$0.00 for 52,879 tokens** —
free rather than unknown. The consequence is worst exactly when it matters
most, because a newly released model is precisely the one missing from a
rate card, and precisely the one whose spend you want to watch. An
implementation must therefore treat `tokens > 0 AND cost == 0` as a
condition to surface, not a result to report.

## 5. Deduplication

**This is the largest single error available, and it is silent.**

The same billable model call appears in transcripts many times over. Not
occasionally — as the dominant shape. Measured over one real corpus,
grouping assistant records by their message identifier:

| records sharing one message id | groups |
|---:|---:|
| 1 | 168,167 |
| **2** | **192,442** |
| 3 | 54,690 |
| 4 | 18,653 |
| 5 or more | ~10,000 |

More identifiers appear twice than once. Within a group the usage block is
**identical, repeated verbatim** — one observed group carried
`input=5, output=1, cacheRead=18,529` six times. It is one call, recorded
six times, and a naive sum charges for six.

Measured inflation from summing without deduplication, same corpus, one
provider:

| class | naive | deduplicated | inflation |
|---|---:|---:|---:|
| input | 79.3 M | 31.3 M | **2.54x** |
| output | 413.4 M | 146.1 M | **2.83x** |
| cache read | 151.8 B | 78.0 B | **1.95x** |
| cache write | 5.21 B | 2.27 B | **2.30x** |

Two things follow.

**Deduplicate per record, before summing.** The inflation differs by class
(1.95x to 2.83x), so it cannot be corrected by scaling a total. Only
collapsing records reproduces the right answer.

**The key must identify the billable call**, not its position in a file:
the message identifier combined with the request identifier where both are
present. A record missing either is counted once and flagged, never
silently dropped.

### 5.1 The key is environment-dependent and must be validated

Do not assume a key that works against a direct provider API works behind
a gateway. Reported from production on a Bedrock-plus-Portkey path,
duplicate message-id groups produced substantial discrepancies against the
platform's own billing.

A proxy or gateway sits between the client and the model and is free to
retry, coalesce, or reissue identifiers. Either failure is available:

- **Ids reissued per attempt** — one logical call appears under several
  ids, deduplication collapses nothing, and the local figure over-counts.
- **Ids replayed across distinct billable calls** — deduplication collapses
  calls the platform charged for separately, and the local figure
  under-counts.

The direction of the divergence identifies which one you have, which makes
reconciliation diagnostic rather than merely reassuring. So:

- The dedup key is **configurable**, not hardcoded.
- Its validity is **established by reconciliation against the billing
  source** for each serving path, and re-established when that path
  changes.
- The count of records collapsed is **reported**, because a dedup step
  that silently removes half the corpus and a dedup step that removes
  nothing look identical in the output otherwise.

## 6. Time bucketing

Each record carries a timestamp. Bucketing to a day requires a stated
timezone, and the choice changes the answer for every record near a
boundary.

- The timezone must be **explicit and configurable** (IANA identifier),
  not implicitly the host's locale, or the same data yields different
  reports on different machines.
- A budget period ("calendar month, resetting on the 1st") is defined in
  that timezone.
- Range filters are inclusive of both endpoints, since a user asking for
  "the 3rd to the 5th" means three days.

### 6.1 Price contemporaneously, and freeze

Rate cards describe the present. Applying today's card to last January's
tokens is an error in the opposite direction from staleness — a later
artifact back-projected into an earlier record, which is a *prochronism*
rather than a stale value. Both are wrong; the correct value sits between
them.

Two obligations follow:

- **Record which card version priced each figure.** A cached card with a
  fetch date supplies this for free.
- **Freeze settled periods.** Recompute only what can still change — a
  session with recent activity — and leave older figures alone. A blanket
  recompute silently reprices history every time it runs, and a genuine
  price revision then propagates backwards with nothing recording that it
  happened.

In practice per-model prices are stable after release, so a recompute is
usually *approximately* right. That is an argument for freezing rather than
against it: the error is small, silent, and therefore never noticed.

## 7. Aggregation

Sum per-record costs into the requested grouping. Two rules:

- **Group by the raw model identifier**, then map to display labels only
  at presentation. Collapsing variants before summing loses the ability to
  explain a total.
- **Carry token counts alongside cost, per class.** A cost with no
  breakdown cannot be checked, and checking is the point: the breakdown is
  what reveals a mis-priced class.

## 8. Reporting obligations

A figure that cannot be audited is not useful for budgeting:

- Report **per-class token totals** next to every cost.
- Report **which rate-card version** produced it, and whether it was the
  offline fallback.
- Report **what was excluded** — unpriced models, records without usage,
  duplicates removed. Silent exclusion is how a total becomes confidently
  wrong.
- Where the provider exposes authoritative billing, reconcile against it
  and report the divergence. The invoice is the oracle; a self-computed
  number that grades itself is the failure mode this whole specification
  exists to avoid.

## 9. Known failure modes

Each of these was observed in a real implementation, not imagined:

1. **Summing per-request input across a conversation.** Where caching is
   absent, each request's input includes the whole prefix, so summing
   re-counts the conversation quadratically. One session reported 58.9 B
   input tokens against a 335 K peak context.

2. **Pricing one provider's tokens with another's rate card.** A corpus
   spanning three providers reported 27.2 B input tokens for a day, of
   which 99.99997% came from the provider with no cache accounting and no
   rate card — priced as though it were the provider that had both.

3. **Flattening the cache-write tiers.** §2.1.

4. **Skipping deduplication.** §5.

5. **Treating a subscription's notional cost as spend.** It measures
   consumption. Saying so is part of the specification, not a caveat.

## 10. Validation

An implementation is correct when, over the same file set, the same
timezone and the same rate-card version, its per-day per-model figures
match an independent implementation within rounding. That comparison
should be a **regression test**, not a one-off: the failure modes above
are all silent, and every one of them produces a plausible number.
