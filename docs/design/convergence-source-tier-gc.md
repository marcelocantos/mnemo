# Source Tiers, Lossy-Source Reconcile, and Orphan GC

*Status: design note — 2026-05-29. Subordinate to
`docs/design/convergence-data-plane.md`; anchors 🎯T68.6.*

**Tracking.** This note is the design source for 🎯T68.6. It is
deliberately written *before* implementation because the slice
**deletes derived data**, and a destructive reconcile needs its safety
argument on record first.

---

## TL;DR

mnemo's ingest cursor only understands *append-grow*: a transcript
file that grows past its recorded offset is re-read; nothing else is.
Three real situations fall outside that model, and 🎯T68.6 makes the
data plane honest about all three:

1. **A source is pruned or rewritten.** Claude Code prunes old
   transcript JSONL. The current cursor (`offset >= size`,
   `store.go`) *skips* a file once it's fully ingested — so a file
   that later shrinks or is rewritten below the offset is never
   reconciled, and a pruned file's rows simply persist.
2. **A source vanishes entirely.** Its derived rows (messages,
   entries, decisions, images, …) are now **orphaned** — the index is
   the only copy, which is fine, but nothing marks that the source is
   gone, and re-deriving from source is impossible.
3. **A derived artifact's source is deleted but the artifact lingers.**
   A vault note for an entity that no longer exists; a `git_commits`
   row for a repo that was removed. These accumulate as silent cruft.

The unifying move is to **name the tiers explicitly** and define which
tier is authoritative when they disagree, then add a *verify-before-
delete* GC that removes genuinely-orphaned derived state without ever
destroying the last durable copy of anything.

---

## The tier model

```
SOURCE  (lossy)         transcript JSONL, git repos, GitHub API, vault files
   │  ingest / mirror reconcile (🎯T68.1–T68.5)
   ▼
INDEX   (durable)       mnemo.db — entries, messages, compactions, mirrors, …
   │  derive / render
   ▼
DERIVED (regenerable)   FTS rows, vault notes, divergence views, patterns
```

- **Source is lossy.** It can be pruned (Claude Code), rewritten, or
  deleted out from under mnemo. mnemo does **not** control its
  lifetime and must not assume it is still present.
- **Index is the durable tier.** Once content is ingested, the index
  is its authoritative home. This is already true in practice — the
  schema policy forbids destructive migrations precisely because
  "some users' source JSONL has been pruned, so a wipe is permanent
  data loss." 🎯T68.6 makes it *designed*: the durable tier is backed
  up (🎯T61) and is the reconcile-from-cold path; it is never GC'd on
  the grounds that "the source is gone."
- **Derived is regenerable** from the index. It is the *only* tier GC
  may delete, because deleting it loses nothing — it can be rebuilt.

The rule that falls out: **GC only ever deletes DERIVED state, and
only after verifying the INDEX (the durable tier) still holds the
content the derived artifact represents.** Source presence/absence
never licenses deleting index rows.

---

## Detection

### A. Pruned or rewritten source (ingest cursor)

The cursor must distinguish "fully ingested, unchanged" from "changed
below the offset." Today only `offset` is stored (`ingest_state`).
Add, alongside it, the file's `size` and `mtime` (or a cheap content
fingerprint of the first N bytes) at the moment the offset was
recorded. On scan:

- `size == recorded_size && mtime == recorded_mtime` → unchanged; skip
  (today's fast path).
- `size > recorded_size` and the prefix is unchanged → append-grow;
  ingest the tail (today's behaviour).
- `size < recorded_size`, or the prefix changed → **rewrite/prune
  detected**. The file's identity is no longer what we indexed. Policy
  (below) decides; default is *retain indexed rows, stop advancing the
  offset, flag it* — never silently drop the rows, because the index
  is the durable copy.

`ingest_state` gains `recorded_size`/`recorded_mtime` columns
(additive, nullable — old rows simply fall back to the size-only
check until next ingest re-stamps them).

### B. Orphaned derived rows (index-internal)

A derived row is orphaned when the index row it derives from is gone,
detected by an exact-key query — e.g. a `messages_fts` row with no
backing `messages` row. No filesystem and no slug parsing.

Two cautions:

- The *index* tables (entries, messages, compactions, git_commits,
  github_prs, …) are the **durable tier**, never orphan-GC targets.
  Only genuinely regenerable derived state qualifies. In particular
  `git_commits`/`github_prs` rows are NOT deleted because a repo is
  currently absent from `knownRepoRoots()` — absence is ambiguous
  (an unmounted checkout, a temporarily-unavailable workspace root)
  and "source gone" never licenses deleting durable-tier rows.
- Orphan detection is a read-only query run by the GC pass, never by
  ingest.

### C. Orphaned vault notes

A note under `<vault>/_mnemo/…` whose backing entity no longer exists
in the index. (Below-fence human annotations are sacred — see Safety.)

**Detection must be forward, not reverse.** A note's path is a lossy
slug of its entity (`names.go`: lowercased, non-alphanumerics
collapsed, capped at 60 chars). Reconstructing the entity *from* the
filename to ask "does it still exist?" is therefore ambiguous — two
entities can slug to the same name, and the slug isn't a key. In a
**delete** path, that ambiguity is unacceptable: a false "orphan"
deletes a live note. So reverse-mapping is rejected.

Instead the exporter maintains an **output manifest** — a
`vault_outputs(entity_kind, entity_id, note_path, content_hash,
written_at)` table written transactionally as each note is rendered.
Orphan detection is then two exact set-differences, no slug parsing:

1. **Manifest entry whose entity is gone** — `vault_outputs` row whose
   `(entity_kind, entity_id)` no longer resolves in the index. Its
   `note_path` is a deletion candidate.
2. **On-disk note absent from the manifest** — a `*.md` under the wing
   that no manifest row claims (e.g. left by an older layout, or a
   path the current exporter no longer produces). Candidate, but
   treated more conservatively (it may predate manifest tracking):
   require the standard mnemo fence with no below-fence content before
   it is eligible, else only report.

The manifest also makes the GC's verify-before-delete cheap: the
recorded `content_hash` lets the pass confirm the on-disk note is still
the generated artifact it wrote (not something a human replaced at that
path) before removing it.

This adds a small forward-path change to the exporter (write the
manifest row alongside each note, in the same transaction) — landing
that is the first step of the GC increment, ahead of the delete logic.

**Sequencing constraint — coordinate with 🎯T64.** The vault exporter
is being actively redesigned under the vault-library-wing work
(🎯T64: the v2 `<vault>/_mnemo/` layout). The `vault_outputs` manifest
must be written by whatever exporter produces the notes, and the
note-path scheme it records is exactly what T64 is changing. Adding
the manifest to the v1 exporter now would both collide with that
in-flight work and bake in a path scheme that's about to change.
Therefore the manifest (and the vault-note GC built on it) should land
**with or after** the T64 v2 exporter, not against the v1 code. The
non-vault pieces of T68.6 — `SourceDrift` (shipped) and the
index-internal exact-key orphan queries (§B) — have no such coupling
and can proceed independently.

---

## GC policy

A single `mnemo gc` product operation (MCP tool + optional scheduled
pass), **not** a schema migration (sqlift has no hook for application-
side verification — this mirrors the messages-dedupe GC framing in
🎯T51 and `CLAUDE.md`).

Invariants:

- **Verify before delete.** Each candidate is deleted only after
  confirming the durable tier still represents its content (or that
  the candidate is pure regenerable derived state). If verification
  fails, skip and report — never delete on doubt.
- **Derived-only.** GC never deletes from the durable index tier. It
  nullifies/removes regenerable derived rows and orphaned vault notes.
- **Idempotent.** Re-running with no new orphans is a no-op.
- **Opt-in / dry-run by default.** `dry_run: true` default; a real
  delete needs `confirm: true`, scoped (`include: [...]`). Mirrors the
  `mnemo_vault_gc_legacy` shape from the vault design.
- **Observable.** The orphan backlog is a stream in the 🎯T68.4
  divergence surface (gap = orphaned derived artifacts); a GC run
  reports what it removed.
- **Reversible where it matters.** A GC pass runs against a DB that is
  backed up (🎯T61), so an over-eager deletion is recoverable from the
  most recent snapshot.

---

## Safety

The one truly irreversible thing in the data plane is **human
below-fence vault annotations** — they exist nowhere else. GC of vault
notes must:

- Never touch below-fence content. An "orphaned" generated note that
  carries human annotations is **not** deleted; it is de-fenced
  (annotations preserved, generated block removed) or left with a
  tombstone marker, per the vault fence contract. Deletion is reserved
  for notes that are *purely* generated with no human content.
- Harvest, don't destroy: align with the vault design's
  `legacy-annotations.md` harvesting so annotations survive even when
  their generated host note is removed.

---

## Acceptance gates (for 🎯T68.6)

1. A pruned/truncated source is detected (`SourceDrift`, **shipped**),
   read-only, and surfaced. Same-size in-place rewrite detection
   additionally records size+mtime (or a fingerprint) on `ingest_state`
   so the rewrite is caught, not silently skipped.
2. A pruned source's indexed rows are retained (durable tier) and the
   condition is observable; the offset stops advancing rather than
   masking the change.
3. The vault exporter writes a `vault_outputs` manifest
   (entity_kind, entity_id, note_path, content_hash) transactionally
   with each note; orphan detection is exact set-difference over the
   manifest (no lossy slug reverse-mapping), plus exact-key index
   queries for non-vault derived rows. The orphan backlog feeds a new
   stream in the 🎯T68.4 divergence surface.
4. `mnemo gc` removes verified orphaned derived state: dry-run by
   default, `confirm` required, scoped, idempotent, reports removals.
5. GC never deletes index (durable-tier) rows and never destroys
   below-fence vault annotations — covered by tests including the
   "orphaned note with annotations" and "pruned source" cases.
6. Index-as-durable-tier + backups-as-reconcile-from-cold is documented
   as the authoritative model (this note, linked from the parent).

---

## Why this unblocks the capstone (🎯T68.7)

The unified reconciler abstraction must model *removal*, not just
catch-up: a stream's "desired set" can shrink. 🎯T68.6 is where the
removal half of convergence gets its shape (verify-before-delete,
derived-only, durable-tier-protected). 🎯T68.7 then generalises both
halves — add-to-converge and remove-to-converge — into one
`(inputs, transform, predicate, cursor)` reconciler. Designing T68.7
before T68.6's removal semantics exist would be guesswork; hence the
ordering.
