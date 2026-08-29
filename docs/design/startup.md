# Startup: capabilities, supervision, and correct-at-every-point views (🎯T154)

## Why

Three production incidents in two days, all the same shape:

| Symptom | Root |
|---|---|
| 1,337 doc inserts failed for 25 minutes | `ingestDocFile` wrote `content_z` before the migration added it |
| A WAL-size assertion failed only on Windows | boot-time writes raced anything measuring the database |
| Image backfill silently skipped for a whole process lifetime | `backfillImages` read `entries_v` on the old schema, logged one warning, returned |

Each was fixed individually. That is the whack-a-mole: the fixes were
correct and the *class* was untouched, so the next facility added to
startup would produce the next incident.

An inventory found the class is large. Ten background writers had no
context and no completion signal. Four workers write at t≈0, before
their first tick. Four ordering requirements were asserted only in
comments. And 🎯T152 introduced a fourth failure mode worse than the
others: `entries_v` read the materialised columns *exclusively*, so
during the materialisation window a reader got **NULL instead of an
error** — silently wrong, not loudly broken.

## The model

Startup brings **capabilities** online. Phases provide them; consumers
require them.

```
schema-upgrade ──provides──> schema.current
                                  │ requires
                             codec ──provides──> codec.ready
                                                    │ requires
                                    entries-materialise ──> entries.materialised
```

A capability is **pending**, **available**, or **unavailable with a
reason**. The third state is what makes degraded mode declarable
instead of discovered one SQL error at a time: when the schema upgrade
fails, `schema.current` resolves unavailable, dependent phases skip, and
a consumer that declared the requirement logs one line instead of
erroring per statement.

### Four rules, each enforced rather than documented

**R1 — Consumers declare requirements.** `s.Requires(cap, activity)`
returns false and logs once per (activity, capability). It does not
block: a ticker skips this pass and comes back rather than pinning a
goroutine for the length of a migration.

**R2 — All background work is supervised.** `s.goOnce` for finite work
(covered by `AwaitStartup`, drained by `Close`), `s.goLoop` for
long-lived workers (cancelled and drained by `Close`, never awaited).
`startup_ratchet_test.go` fails the build on any other `go` statement in
the package. Exceptions live in a named allowlist with written reasons —
a heuristic would silently reclassify new code, whereas an allowlist
entry is a decision someone has to make in review.

**R3 — Quiescence is expressible.** `AwaitStartup()` is the single
answer to "is the store quiescent?". Tests that measure the database,
its WAL, or row counts call it; `newTestStore` does so for every test.

**R4 — Phase state is reported.** `doctor`'s `startup.capabilities`
shows pending and unavailable capabilities with reasons, so a stuck
phase is visible in production rather than only as a flaky test or an
empty result.

### The structural fix that beats all four

For `entries_v`, gating readers would have been the wrong answer.
Every field is now `COALESCE(materialised, generated)`, so the view is
correct at **every** point in the rollout: a compressed row has only the
`_m` value, an unmaterialised legacy row has only the generated one, a
materialised row has both and they agree. The window in which a reader
could see NULL no longer exists, for any reader, declared or not.

The general principle: where an invariant can be made to hold
structurally, do that instead of asking every consumer to check. Rules
R1–R4 are for what cannot be made structural. The cost here is that a
filter on one of those columns cannot use `idx_entries_*_m`; the one
query that needs the index reads the base table under `INDEXED BY`.

## What changed

- `internal/store/startup.go` — the graph, the supervisor, `Requires`,
  `StartupReport`.
- `store.New` declares three phases in one place. It waits for
  `codec.ready` only on a boot with no pending migration, preserving the
  guarantee that the first ingest writes packed rows without blocking an
  upgrade boot for minutes.
- Retired: `codecBoot`, `AwaitCodecBoot`, `enableCompression`,
  `materialiseEntriesAtBoot` — four ad-hoc mechanisms replaced by one.
- Supervised: image backfill, per-session image extraction, image
  sidecars, OCR / describer / embedder backfills, WAL maintenance, cost
  reconciler.
- `Close` cancels once and drains long-lived workers under a 3s grace.
- **Unrelated pre-existing bug fixed:** `startStreamSegWatcher` and
  `startStructuralRetirementBackfill` were started from the *tail of
  `startBackupWorker`*, so every early return there — backups disabled,
  a bad window, a bad quiescence value — silently skipped both. One of
  them documents itself as "a correctness requirement rather than an
  accelerator". They now start from `startWorkers`, independent of
  backup configuration.

## Residue

Named, not hidden:

- `DriveStreamReconcilers` abandons a stream that overruns its deadline;
  an abandoned pass can still write after the drive returns. Bounded by
  the pass deadline, allowlisted with that reason.
- `internal/registry` workers are on WaitGroups but do not yet declare
  capability requirements; four use the older `AwaitSchemaUpgrade`. The
  graph is the intended destination.
- `StartRateCardRefresher` is package-level with no Store; it writes a
  file, never SQLite, so it cannot race a database observer.
