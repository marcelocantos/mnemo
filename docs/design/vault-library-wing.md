# Vault Library Wing

*Status: design draft — 2026-05-20.*
*Supersedes the original vault export design (PR #74) and informs the next major
version of `vault_path`.*

---

## TL;DR

The current vault export (shipped in #74, #77, #78) materialises rows from
mnemo's SQLite tables as Markdown — one note per session, per CI run, per PR.
That projection is faithful to mnemo's internal schema but offers little to a
human reading their PKM graph: it is wide, repetitive, and indistinguishable
from a raw database dump.

This design replaces the existing layout with a **well-fenced library wing**
under `<vault>/_mnemo/`. The wing carries only second-order, human-meaningful
abstractions — themes, patterns, cross-repo similarities, lessons — distilled
from the underlying session corpus. Raw event data stays in SQLite, reachable
on demand via the existing `mnemo_*` query tools.

Three audiences are first-class: new users picking up mnemo today, existing
users with no vault configured, and existing users who already have a vault
populated by the v1 layout. All three converge on the same end state without
forced data loss.

---

## Problem statement

The v1 vault has four structural problems for a human consumer of a PKM graph.

1. **Wrong abstraction layer.** A note like `sessions/abc-1234.md` containing
   a 6000-token transcript is not knowledge — it is raw signal. Knowledge is
   the recurring theme that surfaces *across* fifteen such sessions. The v1
   vault writes the signal and leaves the knowledge extraction to the reader.

2. **Vault footprint invades the user's namespace.** v1 writes
   `<vault>/sessions/`, `<vault>/decisions/`, `<vault>/ci/`, etc. directly at
   the vault root. For users with existing folder taxonomies (PARA, Johnny
   Decimal, MOC-driven Obsidian setups) this collides with their structure.

3. **One PKM shape assumed.** v1 emits Obsidian-flavoured wikilinks, YAML
   frontmatter, and a flat per-entity folder layout. Logseq users get
   semi-broken page references; users of plain-Markdown editors see foreign
   syntax; Roam/Tana users get nothing useful.

4. **Graph view is a hairball.** Every session note links to every decision
   it surfaced, every PR it referenced, every CI run that followed. In
   Obsidian graph view this produces a dense mesh with no readable structure
   and no obvious entrypoint.

Net: a user opening a vault populated by v1 cannot easily see what their
graph "knows" — they see only what their sessions *contained*.

---

## Goals

1. **Library wing, not roommate.** mnemo writes to exactly one subtree
   (`<vault>/_mnemo/`). Nothing else in the vault is touched, ever.
2. **Distilled, not dumped.** What the vault contains is the *output* of
   semantic processing — themes, patterns, cross-repo similarities, lessons,
   high-signal decisions, indexed memories. Not transcripts, not CI rows,
   not PR feeds.
3. **PKM-tool-agnostic.** Default rendering works in Obsidian, Logseq, Foam,
   and plain-Markdown editors. A profile system handles tool-specific syntax
   without forcing user configuration.
4. **Readable graph.** The mnemo subgraph forms a tight hub-and-spoke
   cluster with a single entrypoint, distinct tag namespace, and optional
   user-anchored bridges into existing MOCs.
5. **Zero forced migration.** Existing vault users do not lose annotations,
   do not have files moved out from under them, and do not face a flag day.
6. **Reversible.** Uninstall = delete `_mnemo/`. No mnemo metadata in the
   user's own notes.

---

## Non-goals

- **Bidirectional editing of mnemo-generated content.** Above-fence content
  remains mnemo-owned and is regenerated on each sync. Below-fence content
  is the human contract.
- **Replacing the user's PKM tool.** mnemo does not become a PKM. It writes
  a wing that a PKM consumes.
- **Block-level fidelity.** Logseq block refs, Roam block refs, Tana fields,
  and similar proprietary outliner constructs are not modelled. mnemo writes
  notes; it does not pretend to be a native outliner.
- **Parsing structured edits back into mnemo's relational graph.** User
  content from the vault — below-fence annotations and (under opt-in scopes)
  the user's broader notes — is full-text indexed for search and clustering
  signal. Treating that content as structured edits to specific themes,
  patterns, or decision rows (e.g. parsing `outcome:: shipped` as a field
  update) is a future direction, out of scope here. See "Indexing scope"
  below for what is read versus what is interpreted.
- **Per-page access control.** Vault content reflects the local user's
  sessions. Multi-user team scenarios are the domain of `team-mnemo` (T36),
  not this design.

---

## User archetypes

Every design decision below is justified against at least one of these.

### A. New user, no PKM tool yet

Installs mnemo, eventually wants somewhere to read its outputs. Has no
existing vault. Configures `vault_path` to a fresh directory.

**Needs:** a vault that is meaningful out of the box, with no configuration
beyond the path. Opening the directory in Obsidian should "just work" —
links resolve, graph view shows useful structure, tags are sensible.

### B. New user, established PKM workflow

Heavy Obsidian / Logseq / Foam / Roam user. Has hundreds or thousands of
existing notes in their preferred taxonomy. Considers their vault their
"second brain" and is allergic to tools that scribble in it.

**Needs:** strict isolation (a single subtree they can ignore or quarantine),
zero collisions with their existing folder/tag conventions, ability to opt
specific mnemo content into their existing graph via bridges, easy removal.

### C. Existing mnemo user, no vault configured

Has been running mnemo for months purely as an MCP search backend. Never
turned on vault export. Sees this announcement.

**Needs:** continued zero-friction. Nothing must change for them unless they
choose to opt in. If they later opt in, they get the v2 layout from day one
with no migration to think about.

### D. Existing mnemo user, vault enabled (v1 layout)

Already configured `vault_path` against a vault populated by `<vault>/sessions/`,
`<vault>/decisions/`, etc. May have hundreds of below-fence annotations
across those files.

**Needs:** their annotations survive. They are not forced to migrate on a
specific day. They can see what is changing, why, and what to do at their
own pace. The eventual cleanup is their decision, not mnemo's.

### E. Power user

Wants to override how mnemo renders entities — different folder layout
inside `_mnemo/`, different frontmatter keys to integrate with their
Dataview queries, different link syntax for an obscure PKM tool.

**Needs:** template override mechanism that does not require recompiling
mnemo or sending a PR.

### F. Casual reader

Looks at the vault once a week, scans high-level pages, occasionally clicks
through to evidence. Will not learn YAML, will not configure profiles, will
not write Dataview queries.

**Needs:** the vault's most valuable content (themes, patterns, lessons) is
visible from the root `index.md` with at most one click. Frontmatter and
fences do not interfere with readability.

---

## Architecture

### Layout

```
<vault>/
├── (user notes — untouched, owned by user)
└── _mnemo/
    ├── index.md
    ├── README.md
    ├── MIGRATION.md          # written once when v1 layout detected
    ├── themes/
    │   ├── _index.md
    │   └── <theme-slug>.md
    ├── patterns/
    │   ├── _index.md
    │   └── <pattern-slug>.md
    ├── cross-repo/
    │   ├── _index.md
    │   └── <topic-slug>.md
    ├── lessons/
    │   ├── _index.md
    │   └── <lesson-slug>.md
    ├── decisions/
    │   ├── _index.md
    │   └── <decision-slug>.md         # high-signal only, not raw dump
    ├── memories/
    │   ├── _index.md
    │   └── <memory-slug>.md           # indexed across projects
    └── legacy-annotations.md          # only if v1 annotations harvested
```

There is no `sessions/`, no `ci/`, no `prs/`, no `repos/`, no `commits/`,
no `images/`. Those tables remain in SQLite and are queried on demand.

### Tag namespace

Every mnemo-generated note carries:

```yaml
tags: [mnemo, mnemo/<type>]
```

Where `<type>` is one of `theme`, `pattern`, `cross-repo`, `lesson`,
`decision`, `memory`. This lets the user:

- Colour the mnemo subgraph distinct from their own notes in graph view.
- Filter all mnemo content out of search results (`-tag:mnemo`).
- Build Dataview queries scoped to mnemo content
  (`FROM "_mnemo" WHERE contains(tags, "mnemo/pattern")`).
- Remove all mnemo content from any tag-based MOC by excluding `mnemo`.

### Fence contract

```markdown
<!-- mnemo:generated 2026-05-19T10:23Z -->
…mnemo-owned content above the fence, regenerated on each sync…
<!-- /mnemo:generated -->

…user annotations below this line, preserved indefinitely…
```

The fence is line-anchored (matches the fix from PR #74 sixth-pass review).
A file with no fence is treated as user-owned and never overwritten (also
preserved from v1).

### Bridges

Optional. Configured per user.

```json
{
  "vault_bridges": {
    "themes":   "10-areas/knowledge/Themes MOC.md",
    "patterns": "10-areas/knowledge/Patterns.md"
  }
}
```

For each bridge, mnemo appends a fenced block to the named anchor file:

```markdown
<!-- mnemo:bridge:themes -->
- [[_mnemo/themes/auth-middleware-redesign|Auth middleware redesign]]
- [[_mnemo/themes/schema-migration-safety|Schema migration safety]]
<!-- /mnemo:bridge:themes -->
```

Properties:

- Above and below the bridge fence is user content, never touched.
- Removing the bridge from config strips the fenced block on next sync; the
  anchor file is otherwise left intact.
- The anchor file may live anywhere in the vault; mnemo creates it if absent
  but only when explicitly named in `vault_bridges` (no silent file creation).

### PKM profile

```json
{ "vault_profile": "obsidian" }   // obsidian | logseq | foam | generic
```

Profile controls:

| Concern              | obsidian       | logseq          | foam           | generic        |
|----------------------|----------------|-----------------|----------------|----------------|
| Link syntax          | `[[X\|alias]]` | `[[X]]`         | `[[X]]`        | `[X](X.md)`    |
| Frontmatter format   | YAML           | YAML            | YAML           | YAML           |
| Properties           | YAML           | YAML + `prop::` | YAML           | YAML           |
| Journal awareness    | no             | yes (read-only) | no             | no             |
| Filename casing      | kebab          | kebab           | kebab          | kebab          |

Auto-detected at first sync from vault contents:

- `<vault>/.obsidian/` present → `obsidian`
- `<vault>/logseq/` present → `logseq`
- `<vault>/.foam/` present → `foam`
- Otherwise → `generic`

User override via `vault_profile` config key always wins.

### Templates (escape hatch)

```
~/.mnemo/vault_templates/
├── theme.tmpl
├── pattern.tmpl
├── cross-repo.tmpl
├── lesson.tmpl
├── decision.tmpl
├── memory.tmpl
└── index.tmpl
```

Standard Go templates with the entity struct exposed. Missing template
files fall back to the binary's embedded defaults. Documented in
`internal/vault/README.md`.

### Indexing scope

`vault_path` describes where mnemo *writes*. This section governs what
mnemo *reads* from inside that path. The two are decoupled by design: a
user can give mnemo write access to a tiny output wing while keeping
their broader knowledge base off-limits, or grant read access to their
whole graph for richer clustering. The default is the stricter of the
two and the user opts into more.

#### Why this is its own concern

The v1 implementation silently read everything under `vault_path`:
below-fence annotations on mnemo-generated files *plus* fence-less
standalone Markdown files anywhere in the vault. That was an acceptable
shortcut when the vault was small and mnemo-owned, but it becomes a
surprise consent issue once a user points `vault_path` at an existing
PKM containing years of personal content. Configuring an output
directory is not the same as granting full-corpus index rights.

The library wing redesign is the right moment to make the read surface
explicit, configurable, and reviewable.

#### Scopes

```json
{
  "vault_indexing_scope": "_mnemo_only",
  "vault_indexing_includes": [],
  "vault_indexing_ignore_file": ".mnemoignore"
}
```

Accepted values for `vault_indexing_scope`:

- `"_mnemo_only"` (default) — mnemo reads only `<vault>/_mnemo/`. This
  covers below-fence annotations on generated pages plus any user
  Markdown the user has placed inside the wing. Nothing outside the
  wing is touched.
- `"full"` — mnemo walks the entire `<vault>` tree (minus the standard
  hidden-dir exclusions: `.obsidian/`, `.logseq/`, `.git/`, `.trash/`,
  etc., already implemented in v1). User content surfaces in
  `mnemo_search`, contributes to theme clustering, and acts as
  ground-truth labels for clusters.
- `"includes"` — mnemo walks `<vault>/_mnemo/` plus each path listed
  in `vault_indexing_includes` (vault-relative, e.g.
  `["areas/knowledge", "projects"]`). Surgical opt-in for users who
  want to share specific subtrees without exposing the whole vault.

`vault_indexing_ignore_file` is a vault-root-relative path to a
gitignore-syntax file. Patterns matched relative to vault root are
excluded regardless of scope. Default name `.mnemoignore`. Absent file
means no extra exclusions.

#### What "read" means

Reading content from the vault has three effects:

1. **`mnemo_search` results.** Indexed content appears as full-text
   search hits alongside session excerpts. Visible to any Claude Code
   session connected to the daemon.
2. **Theme clustering input.** Content contributes vectors/tokens to
   the clustering pipeline. Human-written theme notes can anchor and
   label clusters that would otherwise be LLM-labelled.
3. **Bridge back-references (future).** Once user content is indexed,
   bridges can show "this anchor MOC references _mnemo/themes/X" both
   ways.

Reading does *not* mean:

- Sending content to any external service unless the user has
  explicitly enabled an embedding provider with an API key.
- Modifying user files. Mnemo's write surface remains `<vault>/_mnemo/`
  plus configured bridge files only.
- Cross-account exposure. Index lives in the local daemon's SQLite;
  team-mnemo's sharing model governs any multi-user case.

#### Comparison of the three scopes

| Capability                                 | `_mnemo_only` | `includes`     | `full`           |
|--------------------------------------------|---------------|----------------|------------------|
| Below-fence annotations indexed            | yes           | yes            | yes              |
| Standalone notes inside `_mnemo/` indexed  | yes           | yes            | yes              |
| User's broader vault notes in `mnemo_search` | no          | within includes | yes             |
| User content informs theme clustering      | no            | within includes | yes              |
| Configuration burden                       | none          | low (1 key)    | none             |
| Surprise-consent risk                      | none          | low            | non-trivial      |
| Storage / FTS index size                   | bounded       | proportional   | scales w/ vault  |
| Suitability for vaults with private content | safest       | safe with care | requires `.mnemoignore` |

#### Behaviour at v1 → v2 migration

The v1 implementation effectively ran in `"full"` mode (without the
opt-in). For existing v1-vault users, we cannot silently narrow scope:
they may rely on `mnemo_search` returning hits from notes they
deliberately placed in their vault.

Migration rule:

- **New vaults (`_mnemo/` did not exist at upgrade time).** Default
  `vault_indexing_scope = "_mnemo_only"`. Strictest by default.
- **Existing v1 vaults (`_mnemo/` absent, root-level mnemo dirs
  present).** Default `vault_indexing_scope = "full"` for continuity.
  `_mnemo/MIGRATION.md` describes the indexing scope explicitly and
  documents how to narrow it via `mnemo_config`.
- **Existing v2 vaults (both `_mnemo/` and root-level dirs from a
  prior partial migration).** Honour the existing config; if unset,
  default to `"full"` for safety.

In all cases, `mnemo_vault_status` reports the active scope and, when
scope is `"full"` or `"includes"`, the number of indexed user files
outside `_mnemo/` so the user can audit what mnemo can see.

#### Per-note opt-out alternative (deferred)

A YAML frontmatter key like `mnemo: ignore` on individual notes would
provide note-level granularity without forcing folder reorganization.
Implementable on top of `.mnemoignore` (frontmatter parser already
runs during ingest classification). Deferred to a follow-up — the
folder-level mechanism handles the 90% case and ships first.

#### Implications for clustering

If the user enables `"full"` (or scoped `"includes"`), the clustering
engine in Slice 8 gains a strong signal:

- A user-written note titled "Auth middleware redesign" with body text
  describing the architecture is a near-perfect cluster label.
- Cluster IDs become stable across runs because the label is anchored
  in human content, not in LLM-generated text that drifts pass to
  pass.
- Cross-repo themes get qualitatively better: the user's own
  high-level notes already span repos by topic, so they bridge
  sessions that the embedding model might cluster only weakly.

Slice 8's design document (`docs/design/vault-clustering.md`, to be
written) treats `vault_indexing_scope` as a first-class input.

### Information model

mnemo extracts six abstraction types. Each has a source, an extraction
recipe, and a representation in the wing.

#### 1. Themes (`themes/`)

**Source.** Decisions + compaction span summaries + targets, grouped by
topical similarity.

**Extraction.** Embedding-based clustering. For each decision summary and
each compaction prose summary, compute a vector embedding (via the
already-present image embedder infrastructure repurposed for text, or an
external embedding service when `ANTHROPIC_API_KEY` is set). Run a streaming
clustering algorithm (HDBSCAN or online k-means) over the corpus at sync
time. Clusters of weight ≥ 3 (i.e. at least three contributing sessions or
spans) become themes. Each cluster gets an auto-generated label by feeding
the top-N representative excerpts to an LLM with a "label this theme in
≤6 words" prompt.

**Update.** Recomputed on a long cadence (daily, default) because clustering
is order-dependent and noisy on short windows.

#### 2. Patterns (`patterns/`)

**Source.** Output of `mnemo_discover_patterns` (already implemented but
currently live-queried only), persisted to a new `patterns` table.

**Extraction.** Each pattern row records: pattern type (e.g.
`direct_jsonl_read`, `repeated_query_shape`), occurrence count, repos
affected, first/last seen timestamps, representative excerpts. Threshold for
emission: occurrence ≥ 3 across ≥ 2 sessions.

**Update.** Recomputed on a medium cadence (hourly) — pattern detection is
cheap and benefits from freshness.

#### 3. Cross-repo (`cross-repo/`)

**Source.** Themes whose repo set has size ≥ 2.

**Extraction.** Derived view over themes. Cross-repo page is auto-generated
when a theme's `repos` field has length ≥ 2 and `weight` ≥ 4 (i.e. the
similarity is not a one-off).

**Update.** Follows the theme update cadence.

#### 4. Lessons (`lessons/`)

**Source.** Feedback-type auto-memories + confirmed decisions where the
confirmation paragraph contains validation language ("yes exactly", "that
worked", "perfect, keep doing that") or the decision was followed by a
durable code change that survived ≥ 2 weeks.

**Extraction.** Heuristic: filter by signal, then cluster by topic (reuses
the theme clustering engine). Cluster label = "<one-sentence lesson>".

**Update.** Long cadence (daily).

#### 5. Decisions (`decisions/`, filtered)

**Source.** Existing `decisions` table.

**Extraction.** Not all decisions become vault pages. Filter to high-signal
decisions: those that are confirmed (not just proposed), have a rationale
of ≥ 1 paragraph, and survived ≥ 1 week without reversal. Low-signal
decisions remain queryable via `mnemo_decisions` but do not pollute the
vault.

**Update.** Per-decision lazy emission as new decisions cross the
high-signal threshold.

#### 6. Memories (`memories/`)

**Source.** Auto-memory files from `~/.claude/projects/*/memory/`.

**Extraction.** Direct projection (already in v1, kept). Filename
disambiguated with `<project>-<name>.md` to avoid Obsidian title collisions
(already in v1, kept).

**Update.** File-watch-triggered (already in v1, kept).

### Page shape

Every entity page follows this skeleton:

```markdown
---
type: theme
tags: [mnemo, mnemo/theme]
aliases: ["Auth middleware redesign"]
weight: 7
first-seen: 2025-09-12
last-touched: 2026-05-18
repos: [mnemo, foo, bar]
---

# Auth middleware redesign

<!-- mnemo:generated 2026-05-19T10:23Z -->

## Summary
Three repos converged on rip-and-replace pattern driven by legal review of
session-token storage. Decisions across mnemo, foo, and bar all favoured
clean rewrites over incremental patching.

## Evidence
- 2026-03-15 · mnemo · "legal flagged session-token storage in cookies"
- 2026-04-02 · foo · "compliance review demanded same change"
- 2026-04-28 · bar · "auth rewrite scheduled for Q2"

## Related
- [[_mnemo/patterns/legal-driven-rewrites|Legal-driven rewrites]]
- [[_mnemo/lessons/compliance-over-ergonomics|Compliance over ergonomics]]

## Underlying decisions
- mnemo · 2026-03-15 · `decision-id-abc1234`
- foo · 2026-04-02 · `decision-id-def5678`

<!-- /mnemo:generated -->

<!-- Your notes below this line — preserved across syncs. -->
```

Session-level evidence is rendered as inline quoted excerpts with `date ·
repo · "quote"` rather than a wikilink to a session page. Session pages no
longer exist. The session UUID is retrievable via `mnemo_chain` if the
reader wants drill-down.

### Graph topology

```
USER GRAPH                  BRIDGES                 _MNEMO WING

Daily/2026-05-19 ─┐                                  _mnemo/index.md
Projects/work ────┼── Themes MOC ──(bridge)──→  themes/_index
Reading/papers ───┤                                  ├→ theme-A ──┐
Areas/career ─────┘                                  ├→ theme-B   │ (theme→pattern)
                                                     └→ theme-C   ▼
                                                              patterns/_index
                                                                 ├→ pattern-1
                                                                 └→ pattern-2 ──→ cross-repo/topic-X
                                                                                  └→ lessons/lesson-Y
```

Properties:

- One entrypoint into the wing: `_mnemo/index.md`. Removable in one delete.
- Internal links form hub-spoke. The user's graph view never shows a
  hairball cluster of mnemo notes.
- Bridges are the only edges crossing from the user's subgraph into the
  mnemo subgraph. Each is opt-in and configurable.
- `#mnemo` tag is the user's filter handle for the entire wing.

---

## Configuration surface

```json
{
  "vault_path": "~/Documents/PKM",
  "vault_profile": "obsidian",
  "vault_layout": "v2",
  "vault_bridges": {
    "themes":   "10-areas/knowledge/Themes MOC.md",
    "patterns": "10-areas/knowledge/Patterns.md"
  },
  "vault_indexing_scope": "_mnemo_only",
  "vault_indexing_includes": [],
  "vault_indexing_ignore_file": ".mnemoignore",
  "vault_clustering": {
    "engine": "embeddings",
    "min_cluster_weight": 3,
    "recompute_interval": "24h"
  }
}
```

| Key                          | Default                          | Hot-reload? |
|------------------------------|----------------------------------|-------------|
| `vault_path`                 | `""` (disabled)                  | yes (#77)   |
| `vault_profile`              | auto-detect, fall back to obsidian | yes      |
| `vault_layout`               | `"v2"` for new vaults; `"both"` for v1-populated vaults | yes |
| `vault_bridges`              | `{}`                             | yes         |
| `vault_indexing_scope`       | `"_mnemo_only"` for new vaults; `"full"` for v1-populated vaults | yes |
| `vault_indexing_includes`    | `[]`                             | yes         |
| `vault_indexing_ignore_file` | `".mnemoignore"`                 | yes         |
| `vault_clustering.engine`    | `"heuristic"` (explicit opt-in to `"embeddings"`, per T63 egress posture) | yes |
| `vault_clustering.min_cluster_weight` | `3`                     | yes         |
| `vault_clustering.recompute_interval` | `"24h"`                 | yes         |

`vault_layout` accepted values:

- `"v2"` — write to `_mnemo/` only. Stop writing v1 paths.
- `"both"` — write to both `_mnemo/` and v1 root-level dirs. Used during
  migration window.
- `"v1"` — write to v1 root-level dirs only. Available as an emergency
  escape hatch; not advertised; removed in a later release.

---

## Migration story

Follows the three-phase deprecation pattern from `CLAUDE.md` (schema policy
section), adapted for filesystem rather than schema.

### Phase 1 — additive (this release)

- `_mnemo/` wing introduced. New abstraction extractors land.
- `vault_layout` config key added.
- On startup, for each user with a configured `vault_path`:
  - If `_mnemo/` exists and any v1 root-level dirs (`sessions/`, `decisions/`,
    etc.) exist → assume in-progress migration; honour configured `vault_layout`.
  - If v1 dirs exist and `_mnemo/` does not → new release on existing vault;
    auto-default `vault_layout` to `"both"`; emit `_mnemo/MIGRATION.md`
    explaining the change, what to expect, and how to opt fully in.
  - If neither → new vault; default `vault_layout` to `"v2"`.
- v1 paths continue to be written when `vault_layout` is `"both"` or `"v1"`.
  Above-fence content stays fresh. Below-fence annotations are preserved
  exactly as in v1.
- `mnemo_vault_status` reports the active layout, the location of each
  abstraction subdir, and the count of v1 leftovers if any.

### Phase 2 — soak (several releases)

- User opts into pure v2 via `mnemo_config(op=write, patch={"vault_layout": "v2"})`.
- v1 paths stop being written. Existing v1 files are not deleted.
- A new sweep runs at the moment of opt-in: walk all v1 root-level dirs,
  harvest below-fence content from each file, and aggregate into
  `_mnemo/legacy-annotations.md` (one section per source file, with the
  original path and harvested annotation text). The user's annotations
  remain reachable through search even if they later `rm -rf <vault>/sessions/`
  manually.
- `mnemo_vault_status` reports "v1 dirs present but unused; safe to delete
  after verifying legacy-annotations.md".

### Phase 3 — garbage collection (user-initiated)

- New MCP tool: `mnemo_vault_gc_legacy`.
- Parameters: `confirm: bool`, `dry_run: bool` (default true), optional
  scope filters (`include: ["sessions","ci"]`).
- Effect when run with `confirm=true, dry_run=false`: deletes the named v1
  root-level dirs after a final pre-flight that verifies
  `legacy-annotations.md` covers every below-fence annotation it would
  destroy.
- Idempotent. Reports what was removed.

No automatic deletion. The user owns the timing.

### Per-archetype migration experience

- **Archetype A (new user).** Never sees v1. `vault_layout` defaults to `"v2"`.
- **Archetype B (new user, established PKM).** Same as A. The new
  `_mnemo/` isolation makes their existing graph safe by construction.
- **Archetype C (existing mnemo user, no vault).** No change. If they later
  configure `vault_path`, they land on `"v2"` immediately.
- **Archetype D (existing mnemo user, v1 vault).** Phase 1 release: sees
  `MIGRATION.md` appear at next sync. Continues to use v1 dirs by default
  until they choose. Phase 2: opts into `"v2"` at their pace; annotations
  harvested. Phase 3: optionally runs `mnemo_vault_gc_legacy` to clean up.
- **Archetype E (power user).** Drops templates into
  `~/.mnemo/vault_templates/` once layout v2 stabilises. No interaction
  with the migration path.
- **Archetype F (casual reader).** Opens `_mnemo/index.md`. Finds themes,
  patterns, lessons. Does nothing else.

---

## Implementation roadmap

Nine slices. Each is independently shippable, reviewable, and reversible.

### Slice 1 — `_mnemo/` namespace + layout config

Plumbs the new directory root and the `vault_layout` config key. No new
abstractions yet. v1 writers retained, gated on `vault_layout`. Adds
`_mnemo/index.md` and `_mnemo/README.md` as static-ish content.

*Value:* nothing user-visible yet, but establishes the namespace and lets
later slices write into it. Required precondition for everything else.

### Slice 2 — move decisions + memories under `_mnemo/`

Re-render the two existing abstractions (decisions, memories) under their
new paths inside `_mnemo/`. Above-fence content gets the new frontmatter
shape. Old v1 decision/memory files are still written when
`vault_layout="both"`.

*Value:* the two existing semantic exports become available in the new
layout. Validates the rendering shape, frontmatter, and graph topology
before more abstractions are added.

### Slice 3 — drop session/CI/PR/repo writers from `_mnemo/`

When `vault_layout="v2"`, do not write session, CI, PR, or repo-index pages
to `_mnemo/`. v1 paths continue to honour the old behaviour under
`vault_layout="both"` or `"v1"`.

*Value:* validates the principle that raw signal stays in SQLite. The user
opting into v2 immediately sees a calmer, smaller wing.

### Slice 4 — indexing scope + `.mnemoignore`

Introduce `vault_indexing_scope`, `vault_indexing_includes`, and
`vault_indexing_ignore_file` config keys. Refactor `IngestVaultAnnotations`
to walk only the configured scope. Implement gitignore-syntax matcher
against the `.mnemoignore` file. Wire migration defaults: new vaults
default to `"_mnemo_only"`; existing v1-populated vaults default to
`"full"` for continuity. Report active scope in `mnemo_vault_status`.

Reuse `trees_of_interest` + `doc_tree_refs` (landed via T53, PR #91)
for the multi-tree case. Each configured scope produces one or more
tree roots:

- `"_mnemo_only"` registers exactly one tree at `<vault>/_mnemo/`.
- `"full"` registers one tree at `<vault>` with the hidden-dir
  exclusions already enforced by the walker.
- `"includes"` registers `<vault>/_mnemo/` plus one tree per
  `vault_indexing_includes` entry. Trees may overlap (a configured
  include path that is an ancestor of `_mnemo/`); the existing T53
  semantics share content rows and dedupe naturally.

Removing a path from `vault_indexing_includes` removes its tree
references; orphaned content rows are GC'd by the same mechanism
that handles other tree removals.

*Value:* fixes the silent-permissive-read behaviour of v1. Archetype B
(heavy PKM) gets the isolation guarantee that the wing's name implies.
Power users and engaged readers (A, F) opt into `"full"` for the richer
feedback loop. Lands before Slice 6 (bridges) and Slice 8 (clustering),
which both depend on a well-defined read surface. Building on T53
instead of inventing a parallel mechanism keeps the data model lean
and reuses tested code.

### Slice 5 — PKM profile + auto-detect

Introduce the `vault_profile` config key, the per-profile renderer hooks,
and the auto-detect-from-vault-contents logic.

*Value:* Logseq/Foam/generic users get a usable wing.

### Slice 6 — bridges

`vault_bridges` config key. Fenced-block writer with the safety constraints
described above. New MCP tool: `mnemo_vault_bridge_list` to inspect active
bridges.

*Value:* Archetype B (heavy PKM users) get their main need met. Without
bridges, mnemo content is segregated; with bridges, it surfaces in user
MOCs without invading.

### Slice 7 — patterns (persisted + rendered)

Promote `mnemo_discover_patterns` from live-queried to persisted. New
`patterns` table (additive, append-only per schema policy). Renderer
emits `_mnemo/patterns/` pages.

*Value:* first new abstraction. Patterns are the cheapest abstraction to
extract (purely heuristic) and the highest signal per unit effort.

### Slice 8 — themes + cross-repo (clustering engine)

The research-shaped slice. Embedding-based clustering over decisions +
compaction summaries, plus indexed user content when scope permits. New
`themes` table (additive). Cross-repo pages derived from themes with
repo set ≥ 2.

This slice carries the most risk and warrants its own subordinate design
document (`docs/design/vault-clustering.md`) once Slice 7 lands.

*Value:* the core promise of the redesign. Without themes, the wing is a
better-organised v1; with themes, it is genuinely new.

### Slice 9 — lessons + GC tool

Lessons extractor (feedback memories + confirmed-and-surviving decisions).
`mnemo_vault_gc_legacy` tool. `_mnemo/legacy-annotations.md` harvester.

*Value:* completes the abstraction set and gives Archetype D users a
clean migration path off v1.

### Templates (cross-cutting)

Shipped between Slice 3 and Slice 5 as a refactor: the renderer is
restructured to load templates from `~/.mnemo/vault_templates/` with
embedded defaults. No user-facing change at landing; unblocks Archetype E
once layout v2 is stable.

---

## Risks and open questions

### Indexing-scope consent migration (Slice 4)

v1 effectively shipped in `"full"` scope without an opt-in. Some users
may rely on `mnemo_search` returning hits from notes they deliberately
authored in their vault. Silently narrowing scope on upgrade would
break their workflow without warning. Conversely, leaving scope wide
open by default for existing vaults perpetuates the consent issue we
are trying to fix.

**Resolution:** the migration rule documented under "Indexing scope"
above splits the default by vault state. New vaults default
`"_mnemo_only"`; existing v1 vaults default `"full"`. `MIGRATION.md`
explicitly states the scope and shows the one-line `mnemo_config`
call to narrow it. `mnemo_vault_status` always reports active scope
and indexed-file count outside `_mnemo/`, so the user can audit at
any time. No silent narrowing on existing vaults.

### Sensitive content in `"full"` scope

A user enables `"full"` scope on a vault that contains private
journals, financial notes, or other sensitive content. That content
becomes returnable from `mnemo_search` invoked by any Claude Code
session connected to the daemon.

**Resolution:** the consent burden lives at the config layer.
`.mnemoignore` provides folder-level excludes; per-note opt-out via
frontmatter is the documented follow-up. `mnemo_vault_status` reports
which subtrees are currently indexed so the user can review. The
broader principle: the local daemon serves the local user; mnemo
does not transmit indexed content off-machine unless the user has
configured a federation peer (T36) or enabled an embedding provider.

### Clustering quality risk (Slice 8)

Themes are the riskiest abstraction. Bad clusters look like:

- Singletons that should have merged ("schema migration" and "DB migration"
  as two themes).
- Mega-clusters absorbing unrelated content ("everything about Go" as one
  theme).
- Labels that are vague or misleading ("various topics").

**Mitigation:** ship Slice 8 behind a `vault_clustering.engine=heuristic`
fallback (TF-IDF + jaccard over n-grams, no embeddings). Document the
known failure modes. Allow `min_cluster_weight` tuning. Provide
`mnemo_vault_themes_inspect` for users to see cluster membership and
flag misclusters.

### Privacy of cross-repo content

Themes can surface text from one repo's sessions in pages cited from
another repo's view. For solo users this is fine. For multi-tenant
deployments (team-mnemo, T36), the cross-repo surface area becomes a
policy question.

**Resolution:** out of scope here. team-mnemo's access model governs that
case. For single-user vaults, all content is the same user's, so the
question does not arise.

### Embedding cost

Computing embeddings for every decision summary and compaction span at
each clustering pass is bounded but not free. A user with 1000 sessions ×
several decisions each is plausible.

**Resolution:** cache embeddings keyed by (entity_kind, entity_id,
content_hash). Recompute only on content change. Estimated cost for the
median user is single-digit cents per re-cluster. For users without an
embedding API key, the heuristic engine handles the corpus.

### Profile auto-detect false positives

A vault containing `.obsidian/` because the user opened it once in Obsidian
but actually uses Logseq daily would be mis-detected.

**Resolution:** `vault_profile` config key always wins. Profile detection
result is logged at startup. Future enhancement: detect more strongly by
looking for tool-specific *content* signatures (Logseq `journals/` with
recent edits, Obsidian `.obsidian/workspace.json` with recent activity).

### `_mnemo/` collides with user's own folder

A user with an existing folder literally named `_mnemo/` (vanishingly
unlikely but possible).

**Resolution:** on startup, if `<vault>/_mnemo/` exists and contains no
mnemo-generated files (i.e. no `index.md` with the mnemo fence), abort
with an error. Require the user to rename or move their folder, or point
`vault_path` elsewhere. Never overwrite.

### Migration ambiguity

A user has both `_mnemo/` (from a previous run) and v1 root-level dirs
(from an even earlier run). Which is current?

**Resolution:** treat as `vault_layout="both"` until the user explicitly
sets it. `MIGRATION.md` documents the state and asks them to choose.

---

## Success metrics

These determine whether the design delivered its promised value once
shipped. Reviewable post-launch.

- **For new users:** ≥ 90% of new vault configurations land on `v2` without
  any `vault_*` config beyond `vault_path`. Validated by surveying
  `mnemo_config` calls in the first 30 days post-release.
- **For existing v1 users:** 0 reports of lost annotations during Phase 1
  migration. Validated by post-release issue tracker scan.
- **Graph readability:** Archetype B users (heavy PKM) report that the
  `_mnemo/` wing is "isolatable" and "doesn't pollute my graph" in user
  feedback. Validated qualitatively.
- **Abstraction value:** Archetype F users (casual readers) report opening
  the vault at least weekly and finding non-trivial insights in themes or
  lessons. Validated qualitatively.
- **Template usage:** at least one community-contributed template lands in
  the wild within 6 months. Validates the escape hatch was real.
- **Migration adoption:** ≥ 70% of existing v1 users have opted into
  `vault_layout="v2"` within 6 months of Phase 1 release.

---

## Out of scope / future directions

- **Structured edit ingestion.** Currently below-fence annotations are
  full-text indexed. A future iteration could parse structured fields
  (e.g. `outcome:: shipped`) and feed them back into mnemo's graph as
  human-verified labels.
- **Bidirectional bridge content.** Bridges are one-way (mnemo → anchor
  file). Allowing a bridge to also surface back-references from mnemo
  content to the anchor would close the loop, at the cost of complexity.
- **Team vault.** Multi-user wings governed by team-mnemo's access model.
  Out of scope for this design.
- **Tana / Reflect / Heptabase profiles.** Proprietary formats. Possible
  via the template mechanism once core profiles ship.
- **Block-level outliner support.** Logseq/Roam block refs. Distinct
  representational model; not pursued.
- **Vault-as-input override layer.** Slice 4 makes the vault readable as
  clustering signal under `"full"` or `"includes"` scopes. A stronger
  step — treating specific curated user notes (e.g. hand-written
  `_mnemo/themes/foo.md` augmentations or user-promoted MOC pages) as
  *authoritative overrides* that pin cluster labels, merge clusters, or
  veto auto-extracted themes — is a follow-up to Slice 8. Requires a
  shape for "user assertion" markers in user content (frontmatter keys
  or fenced directives) and conflict resolution rules.
- **Per-note `mnemo: ignore` frontmatter.** Folder-level `.mnemoignore`
  ships first; per-note opt-out lands as a follow-up once a clear
  shape is settled.

---

## Acceptance criteria

The work covered by this design is considered complete when all of the
following are true:

1. `<vault>/_mnemo/` is the only directory written by mnemo for new vault
   configurations.
2. Existing v1 vaults can be migrated to v2 with zero annotation loss and
   user-paced timing.
3. Themes, patterns, cross-repo, lessons, decisions (filtered), and
   memories are all materialised under `_mnemo/`.
4. The wing renders correctly in Obsidian, Logseq, Foam, and a plain
   Markdown editor without per-tool configuration.
5. Bridges work end-to-end against at least Obsidian and Logseq.
6. Template overrides work for at least one entity type end-to-end.
7. `mnemo_vault_status` reports layout, profile, indexing scope,
   abstraction counts, count of indexed user files outside `_mnemo/`,
   and any v1 leftovers.
8. `mnemo_vault_gc_legacy` removes v1 leftovers after annotation safety
   check.
9. Indexing scope defaults to `"_mnemo_only"` on new vaults and `"full"`
   on v1-populated vaults at upgrade; both behaviours covered by tests.
10. `.mnemoignore` (gitignore syntax) is honoured at vault root in
    `"full"` and `"includes"` scopes; verified by tests including
    nested-pattern cases.

---

## Appendix: relationship to existing infrastructure

- **PR #74** (vault export) — supersedes the layout but reuses the fence
  contract, the file-watch ingest, the path-canonicalisation logic, and
  the rendering primitives. Slice 2 above is a refactor of #74's renderers,
  not a rewrite.
- **PR #77** (`mnemo_config` hot-reload) — every config key introduced
  here uses the existing hot-reload machinery. `vault_path`, `vault_profile`,
  `vault_layout`, `vault_bridges`, `vault_clustering.*` are all eligible
  for live reconfiguration.
- **PR #78** (path-exclusion registry) — `_mnemo/` registers itself as a
  path exclusion at user init, exactly as `vault_path` does today. No
  re-ingest of mnemo's own output.
- **PR #82 / T49** (sqlift-mediated additive-only schema) — every new
  table in this design (and in the subordinate clustering design) ships
  via append-only DDL only. `internal/store/schema.sql` is the single
  source of truth; sqlift's `AllowNone` gate is the enforcement
  mechanism.
- **PR #91 / T53** (trees_of_interest + doc_tree_refs) — the
  scope-as-trees model in Slice 4 reuses these tables. Each scope
  becomes one or more tree roots; overlapping trees share content rows
  via doc_tree_refs.
- **PR #86 / #87 / T61** (backup primitive + periodic backup worker) —
  clustering tables (`themes`, `theme_members`, `cluster_embeddings`,
  `cluster_runs`) and indexing-scope tables ride the standard mnemo.db
  snapshot. No separate backup mechanism required. Periodic backups
  enforce retention.
- **PR #89 / T62** (eager-start default user workers at daemon boot) —
  the clustering background worker and any other vault-library workers
  inherit boot-time startup automatically when registered under the
  user's WaitGroup. No lazy-on-first-MCP-call surprises.
- **PR #92 / T63** (cost reconciliation opt-in via explicit config) —
  establishes the precedent for outbound-API features: default off,
  explicit opt-in. The clustering design honours this for the
  embeddings engine. See `docs/design/vault-clustering.md`.
- **`docs/design/team-mnemo.md`** (T36) — orthogonal. The library wing
  describes single-user vault output; team-mnemo describes multi-user
  shared indices. The two can coexist; a team-mnemo deployment can expose
  a `_mnemo/` wing per user against a shared backend.
