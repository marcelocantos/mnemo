# mnemo performance baseline (🎯T165)

The benchmarks in `internal/store/perf_bench_test.go` cover the four
things agents wait on: `mnemo_search` (message search, repo-filtered
search, and the default cross-corpus search), `mnemo_recent_activity`,
`mnemo_usage` in each of its five groupings, and ingest of one new
transcript.

## Running them

```bash
make bench        # run them; raw output lands in bin/bench.txt
make bench-gate   # run them and compare against docs/perf/baseline.txt
make bench-lock   # run them and make the result the new baseline
```

The gate fails in **both** directions. A regression needs a fix; an
improvement needs `make bench-lock` in the same commit as the change
that earned it, because a baseline nobody re-locks stops describing the
code and the next regression only has to stay under the stale number.

## Two kinds of number, and only one of them is honest here

The corpus is synthetic and fixed (see `perf_corpus_test.go`), so some
of what the benchmarks report is a pure function of the corpus and the
code:

| Metric | Meaning |
|---|---|
| `selects/op` | SELECT statements issued on the read pool. The N+1 detector. |
| `payload-bytes` | Size of the indented-JSON answer a tool would return. What an agent's context pays for. |
| `rows/op`, `hits/op` | How much the call returned, so a speed-up that came from returning less is visible as such. |

Those are identical on any machine, and `TestPerfRatchet` locks a
subset of them exactly, in both directions, in every `go test ./...`.
`make bench-gate` locks all of them against `docs/perf/baseline.txt`.

`ns/op`, `B/op` and `allocs/op` are not machine-independent, so
`make bench-gate` holds them only against a baseline recorded on the
same machine, and CI compares with `-scope exact` — allocation counts
differ between operating systems because the code paths under them do.

Timing is nonetheless gated, because **benchgate reduces a benchmark's
samples to their floor rather than their middle**. Competing load can
only ever make a run slower, so the fastest of ten samples is the
closest thing to an uncontended measurement, and a same-machine
comparison should be held to the floor. The evidence for that choice:
two ten-sample runs of this package, one taken under load average ~100
and one under 5 to 67, agreed on the floor within 5% for eight of the
twelve benchmarks and within 25% for ten of them, while their means
differed by up to 66%.

What the floor does not survive is a change in the machine's *sustained*
state. Two experiments, both with `-scope all`:

- Two ten-sample runs taken back to back under the same load: **every
  metric within tolerance**, timings included.
- A baseline taken under load 5-to-67 against a run taken hours later
  under sustained heavy load: **six of twelve floors 26% to 67% high**.

So the timing lock is real but conditional, and the gate's default scope
is therefore the machine-independent metrics.
`BENCH_GATE_FLAGS="-scope all"` compares timings and is worth running
when the machine is in the same state the baseline was recorded in.
Give any gate run as many samples as the baseline has — a floor over two
samples sits well above a floor over ten, which is why `BENCH_COUNT`
defaults to ten for the gate as well as the lock.

## Reference machine

| | |
|---|---|
| Machine | Apple M4 Max, macOS 26.6.2, arm64 |
| Go | go1.26.4 |
| Recorded | 2026-09-07, `-count=10 -benchtime=1s` |
| Load during the run | load average 60 to 90 throughout — a large agent fan-out. The floors are what the gate compares, and they held (see above); the means are not usable. |

## Locked numbers

`docs/perf/baseline.txt` holds the raw `go test -bench` output. The
timings it locks, as floors over ten samples:

| Benchmark | ns/op floor |
|---|---|
| `Search/messages` | 35.12 ms |
| `Search/messages_repo_filter` | 31.75 ms |
| `Search/unified_default` | 39.81 ms |
| `Search/unified_all` | 38.89 ms |
| `RecentActivity/7d` | 1.23 ms |
| `RecentActivity/30d` | 1.23 ms |
| `Usage/day` | 189.54 ms |
| `Usage/model` | 175.34 ms |
| `Usage/repo` | 182.85 ms |
| `Usage/session` | 186.17 ms |
| `Usage/block` | 160.52 ms |
| `IngestTranscript` | 11.10 ms |

And the machine-independent metrics:

| Benchmark | selects/op | payload-bytes | rows or hits |
|---|---|---|---|
| `Search/messages` | 19 | — | 20 hits |
| `Search/messages_repo_filter` | 20 | — | 8 hits |
| `Search/unified_default` | 62 | — | 20 hits |
| `Search/unified_all` | 74 | — | — |
| `RecentActivity/7d` | 1 | 162,391 | 300 rows |
| `RecentActivity/30d` | 1 | 162,391 | 300 rows |
| `Usage/day` | 5 | — | — |
| `Usage/model` | 5 | 850 | 1 row |
| `Usage/repo` | 5 | 86,013 | 300 rows |
| `Usage/session` | 5 | 168,537 | 500 rows |
| `Usage/block` | 3 | — | — |

`Usage/session` shows the row cap doing its job: the 1,200-session
corpus produces 1,200 groups and the answer carries 500 of them plus
`omitted_total`, which took its payload from 403,029 bytes to 168,537.
`RecentActivity` is under the topic cap at this corpus shape, so its
payload is unchanged; the caps themselves are pinned by
`TestRecentActivityCapsPerRepoLists` and
`TestCapUsageRowsSummarisesTheTail`, which construct the shapes that
trigger them.

Three benchmarks deliberately report no `payload-bytes`, because theirs
is not stable between runs and a gate must not lock a number that moves
on its own:

- **Search.** Message ids depend on the order sixteen ingest workers
  wrote 1,200 files, and a tie in BM25 rank then breaks differently. The
  hit count is stable and is what search locks.
- **`Usage/day` and `Usage/block`.** Their period buckets are relative
  to the wall clock, so the corpus falls into different buckets as it
  ages. That moves the row count as well as the payload — a corpus
  spanning a day boundary produces two day rows and one otherwise,
  which is exactly how the gate first caught it — so those two
  groupings report statements issued and nothing else.

## What moved, and by what mechanism

| Path | Before | After | Mechanism |
|---|---|---|---|
| `Search/messages` selects | 129 | 19 | Eligibility as one batched EXISTS query; one batched fetch; one batched context query per direction. |
| `Search/messages_repo_filter` selects | 841 | 20 | Same. The repo filter used to run a `COUNT(*)` per over-fetched FTS hit. |
| `Search/unified_default` selects | 364 | 62 | Same, through the unified search path. |
| `mnemo_usage` field reads | 4 `json_extract` over `entries.raw` per assistant row | materialised twins via `entries_v` | 🎯T152's twin pattern extended to message id, request id and the two cache-write counters; historical rows filled by the `entries.usage` pass. |
| `mnemo_recent_activity` payload | one topic per session per repo, unbounded | ≤10 topics and work types per repo, omissions counted | Per-repo list cap with disclosure. |
| `mnemo_usage` payload | 403,029 bytes over 1,200 session rows | 168,537 bytes over 500 rows plus `omitted_total` | Row cap with disclosure; `total` still covers every row. |

`ns/op` for search did not move, and honestly so: the FTS scan is 91% of
a search (`pprof -list` on `Store.Search` attributes 1.01s of 1.11s to
`ftsRows.Next`), because an OR-relaxed query scores most of the corpus
before the LIMIT applies. The statements were never the time — they
were the read pool's lock, taken 841 times by one tool call.

## Known residue

- `mnemo_usage` is ~200 ms on this corpus and the profile puts
  essentially all of it inside SQLite executing one statement, not in
  Go. `EXPLAIN QUERY PLAN` shows the billable CTE scanning every
  assistant row via `idx_entries_type` and building a temp B-tree for
  the dedup GROUP BY. A covering index over the twins would make the
  CTE index-only, but it would be a very wide index on the largest
  table in a multi-gigabyte database, and that trade was not taken.
- The machine never went idle; every run behind this baseline was taken
  with a large agent fan-out running. Gating on floors makes that
  survivable within one machine state but not across two, which is the
  measured limit above. A re-lock on a genuinely idle machine would
  lower every floor here and is worth doing before anyone treats these
  absolute timings as mnemo's true cost.
