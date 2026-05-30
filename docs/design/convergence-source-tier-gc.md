# Source Tiers, Content + State Convergence, and Policy GC

*Status: design note — 2026-05-29 (rev 2026-05-30 to lock in the
bitemporal model). Subordinate to
`docs/design/convergence-data-plane.md`; anchors 🎯T68.6.*

**Tracking.** Design source for 🎯T68.6. Written before any
deletion-capable code so the safety argument is on record.

---

## TL;DR

mnemo's data plane has been informally treating the source (transcript
JSONL, repos, GitHub API) as canonical and the index as a cache of it.
That framing is wrong: sources are **lossy** — Claude Code prunes
JSONL; repos and PRs are mutable — so the index is what actually holds
content durably. Once you accept that, two distinct convergence laws
fall out of one mess:

1. **Content convergence (append-only).** The index converges toward
   containing **everything ever observed in any source** — never
   toward mirroring the *current* source. Source pruning never removes
   indexed content.
2. **State convergence (per-row tag).** Each source-bound row carries
   a small `source_status` attribute — `live` / `truncated_at=…` /
   `deleted_at=…` — that converges toward the current state of its
   source. The gap is rows whose tag hasn't caught up to reality.

Both laws are convergent in the strict sense: idempotent, quiescent at
the fixed point, self-healing. They describe different reconcilers
(ingest for content; a drift sweep for state) but share the same
observability surface (🎯T68.4).

This is essentially **bitemporal**: transaction time (when mnemo
observed it, durably) vs valid time (when the source claimed it was
true). The same shape as event-sourcing with tombstones-as-attributes
(Kafka log compaction, Datomic).

With this model, the `mnemo gc` scope **narrows dramatically**. Source
disappearance becomes a *tag*, not a delete. Deletion is reserved for
genuinely-policy-orphaned derived state — e.g. notes left behind when
the exporter's targeting changes — which is far smaller and safer to
specify.

---

## The tier model (revised)

```
SOURCE   (lossy, ephemeral)   transcript JSONL, git repos, GitHub API, vault files
   │  ingest / mirror reconcile (🎯T68.1–T68.5) — drives CONTENT convergence
   │  source-state drift sweep — drives STATE convergence (tag source_status)
   ▼
INDEX    (durable, canonical) mnemo.db — entries, messages, compactions, mirrors, …
   │  derive / render
   ▼
DERIVED  (regenerable)        FTS rows, vault notes, divergence views, patterns
```

- **Source is ephemeral.** mnemo does not own its lifetime. It is the
  *feed* — what to ingest next, what state to tag — never the
  authoritative copy.
- **Index is canonical.** Once content is ingested, the index holds it
  durably; the schema policy already enforces this (no destructive
  migrations). Backups (🎯T61) are the reconcile-from-cold path.
- **Derived is regenerable from the index.** Source presence/absence
  does not license deleting derived state — only the index does (and
  only when the derived state is no longer licensed by the index's
  content or the producer's policy).

Key consequence: **the convergence law for source-bound streams is
two-axis** (content + state), not single-axis. The earlier informal
"actual = f(current source)" framing was lopsided; this is the
symmetric version.

---

## Convergence laws (formal)

Two laws, two reconcilers, one surface:

**Law 1 — Content (monotone, append-only):**
> For every (source_path, content) ever observed, the index contains
> the derived row(s). The set is monotonically growing; ingest is its
> reconciler.

**Law 2 — State (per-row tag converges to current source):**
> For every source-bound row, `source_status` equals the current state
> of its source (`live` / `truncated_at=…` / `deleted_at=…`). The
> drift sweep is its reconciler; the gap (rows whose tag is stale) is
> reported in the 🎯T68.4 divergence surface.

Both are convergent in the strict sense: idempotent, quiescent,
self-healing. Both expose their gap. The reconcilers don't conflict —
one writes content (rare events, append-only); the other writes a tag
column (small, idempotent UPDATEs).

---

## Detection

### A. Source state (pruned / truncated / rewritten)

Two distinct cases, two costs:

- **Pruned (file gone) or truncated (current size < ingested offset)**
  — detectable read-only from the existing cursor (`SourceDrift`,
  **shipped**). The state reconciler reads this and updates
  `source_status` on the affected rows: `deleted_at = now()` (gone) or
  `truncated_at = now()` (shrunk).
- **Rewritten in place at the same size** — only detectable with a
  stored content fingerprint (a hash of the first N bytes).
  `ingest_state` gains `recorded_size`, `recorded_mtime`,
  `recorded_prefix_hash` (additive, nullable; old rows fall back to the
  size-only check until next ingest re-stamps them). A fingerprint
  mismatch triggers the same state-reconciler path.

Crucially, detection **does not stop ingest** and **does not remove
rows**. It tags. The append-grow path is unchanged for live sources.

### B. Source-state propagation onto rows

Source state lives at file/repo granularity, but consumers care at row
granularity. The state-reconciler propagates a tag onto each affected
row:

- `entries`, `messages` rows derived from a `source_path` get
  `source_status` and `source_state_at` columns.
- Mirror rows (`git_commits`, `github_prs`, `ci_runs`) get the same,
  scoped per-repo.

These columns are additive and nullable (default `live`). Old rows
behave identically until a drift event tags them.

### C. Derived orphans (policy-driven, narrowed)

A derived artifact is now only an orphan in the **policy** sense: the
producer no longer targets it. Examples:

- A vault note left under `<vault>/<old_layout>/…` after the exporter
  switched to a different layout (e.g. v1 → 🎯T64's `_mnemo/…`). The
  note's *entity* still exists in the index (with whatever
  `source_status`), but the exporter no longer outputs to that path.
- A `messages_fts` row with no backing `messages` row (a genuine
  internal-derived dangle from a past bug; expected to be zero in
  steady state, but verified by the same predicate).

Detection uses a forward `vault_outputs` manifest (entity_kind,
entity_id, note_path, content_hash, written_at), written
transactionally by the exporter. Orphans are then exact set-differences
over the manifest, *not* lossy slug reverse-mapping (filenames are
slugs and could collide; false orphans in a delete path would destroy
live notes). For non-vault derived rows, exact-key index queries.

**Explicitly out of scope here:** there is no "delete because source
is gone" path. That is replaced entirely by `source_status` tagging.

---

## GC policy (narrower, safer)

`mnemo gc` removes **policy-orphaned derived state only**. Not
source-orphaned content (that's tagging). Not index rows ever.

Invariants:

- **Policy-orphaned only.** Candidate ⇔ derived artifact no longer
  targeted by its producer. Verified via the forward manifest
  (`vault_outputs`) or an exact-key derived-internal query.
- **Verify before delete.** Each candidate is deleted only after
  confirming (a) the index still represents the relevant content (or
  the candidate is purely regenerable internal derived state) and
  (b) the on-disk artifact still matches the manifest's `content_hash`.
  If verification fails, skip and report.
- **Derived-only.** GC never deletes from the durable index tier.
- **Idempotent.** Re-running with no new policy-orphans is a no-op.
- **Dry-run by default.** `dry_run: true` default; a real delete needs
  `confirm: true`, scoped (`include: [...]`).
- **Observable.** The policy-orphan backlog is a stream in the 🎯T68.4
  divergence surface; a GC run reports what it removed.
- **Reversible where it matters.** Runs against a backed-up DB
  (🎯T61).

---

## Safety

The one truly irreversible thing in the data plane is **human
below-fence vault annotations** — they exist nowhere else. GC of vault
notes must:

- Never touch below-fence content. A policy-orphaned generated note
  that carries human annotations is **not** deleted; it is de-fenced
  (annotations preserved, generated block removed) or left with a
  tombstone marker.
- Harvest, don't destroy: align with the vault design's
  `legacy-annotations.md` harvesting.

---

## Acceptance gates (for 🎯T68.6)

1. Pruned/truncated sources are detected (`SourceDrift`, **shipped**).
   Same-size in-place rewrite detection adds size+mtime+prefix-hash to
   `ingest_state` (additive, nullable).
2. Affected rows are *tagged* (`source_status`, `source_state_at`),
   not removed. The append-grow ingest path is unchanged for live
   sources.
3. The state-reconciler runs as a drift sweep, propagating source
   state onto rows; its gap (rows whose tag is stale relative to
   current source state) is a stream in the 🎯T68.4 divergence
   surface.
4. The vault exporter writes a `vault_outputs` manifest
   (entity_kind, entity_id, note_path, content_hash, written_at)
   transactionally with each note. Detection of *policy* orphans is
   exact set-difference; reverse-mapping is rejected.
5. `mnemo gc` removes policy-orphaned derived state: dry-run default,
   `confirm` required, scoped, idempotent, reports removals.
6. GC never deletes index (durable-tier) rows and never destroys
   below-fence vault annotations — tests cover both, including the
   "orphaned note with annotations" and "source-deleted entity" cases
   (the latter must NOT trigger a GC candidate, only a tag).
7. The two-law model + index-as-canonical-tier +
   backups-as-reconcile-from-cold is documented as authoritative
   (this note, linked from the parent).

---

## Sequencing constraint (vault GC + 🎯T64)

The `vault_outputs` manifest is written by the vault exporter, which
🎯T64 is actively redesigning (v2 `<vault>/_mnemo/` layout). The
note-path scheme T64 changes is what the manifest records. Therefore:

- The **non-exporter** pieces of T68.6 can land independently:
  `SourceDrift` (shipped), `source_status`/`source_state_at` columns +
  the state-reconciler, the `vault_outputs` schema, orphan detection
  over the manifest, the dry-run `mnemo gc` tool, divergence streams,
  tests.
- The **exporter-side manifest-write call** (the few lines inside the
  exporter that record each note) should land with or after T64's v2
  exporter, so it isn't bolted onto code Navnita is replacing.

---

## Why this unblocks the capstone (🎯T68.7)

The earlier framing modelled only one kind of cursor per stream — "how
far have we caught up?" That left the abstraction lopsided because
*removal* didn't fit and source-loss was a special case. The two-law
model makes the shape symmetric: each stream declares

```
stream := {
  inputs            : ...,
  transform         : ...,
  content_cursor    : ...,    // law 1: monotonic catch-up
  state_predicate   : ...,    // law 2: per-row state, converges to source reality
}
```

T68.7 generalises both halves into the unified reconciler. T68.6 is
where the second half — state convergence as a tag, not a delete —
gets its shape. Designing T68.7 before T68.6's state semantics existed
would be guesswork; hence the ordering.
