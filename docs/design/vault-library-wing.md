# Vault Library Wing

*Status: design draft — 2026-05-20 (rev 2026-05-22, review pass 1).*
*Supersedes the original vault export design (PR #74) and informs the next major
version of `vault_path`.*

**Tracking.** Per project convention, this RFC is anchored to a parent
bullseye target with one sub-target per slice. The bullseye targets are
the followable unit; this document is the design source they reference.

- **🎯T64** — parent (Vault library wing redesign)
- **🎯T64.1** — indexing scope + `.mnemoignore` (consent fix)
- **🎯T64.2** — `_mnemo/` namespace + `vault_layout` config
- **🎯T64.3** — move decisions + memories under `_mnemo/`
- **🎯T64.4** — drop session/CI/PR/repo writers (closes MVP v2)
- **🎯T64.5** — PKM profile + mtime-based auto-detect
- **🎯T64.6** — bridges
- **🎯T64.7** — patterns (persisted + rendered)
- **🎯T64.8** — themes + cross-repo (clustering engine; subordinate
  design `docs/design/vault-clustering.md`)
- **🎯T64.9** — lessons + GC tool

Full target definitions in `docs/targets.md`. Status, weight, and
rework history are queryable via `mnemo_targets` and
`mnemo_rework_history`.

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

### MVP v2 boundary

The full nine-slice arc takes weeks to months even with agent assistance.
The first four slices form a coherent, independently shippable **MVP v2**:

1. Slice 1 — indexing scope + `.mnemoignore` (fixes the v1 silent
   full-vault read; the only real shipped *bug* in v1).
2. Slice 2 — `_mnemo/` namespace + layout config.
3. Slice 3 — move decisions + memories under `_mnemo/`.
4. Slice 4 — drop session/CI/PR/repo writers from `_mnemo/`.

After MVP v2 the wing is calm, scoped, and consent-correct. Themes,
patterns, lessons, bridges, and the clustering engine (Slices 5–9) are
independent follow-ups that ship when ready. The MVP boundary lets the
consent fix land promptly without waiting for the clustering engine to
prove out.

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
    ├── MIGRATION.md          # write-once when v1 layout detected; opt-in regen via mnemo_vault_migration_doc
    ├── themes/
    │   ├── _index.md
    │   ├── _archive/
    │   │   └── <theme-slug>.md            # auto-retired themes (config: retire_after)
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

**Naming note.** Collection directories are plural nouns (`themes/`,
`patterns/`, `lessons/`, `decisions/`, `memories/`) with one
deliberate exception: `cross-repo/` reads as an adjectival
qualifier ("cross-repo views over themes") rather than a noun in
its own right. Renaming to `cross-repo-themes/` was considered and
rejected for verbosity; renaming the others to a singular form
would be more disruptive than the inconsistency is worth.
Implementers should treat `cross-repo` as a derived view, not a
peer of `themes`. See `docs/design/vault-clustering.md` for the
derivation rule.

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

#### MIGRATION.md — write-once exception

`_mnemo/MIGRATION.md` is an explicit exception to the regen-above-fence
contract. It is **written once** at the moment of v1-detection on a
vault, then never touched again by mnemo. Specifically:

- The file is created with no fence. Once on disk, it is treated as
  fully user-owned content from mnemo's point of view.
- mnemo does not regenerate MIGRATION.md on subsequent syncs, even if
  v1 dirs disappear or the layout changes.
- If the user deletes `_mnemo/MIGRATION.md`, mnemo does **not**
  recreate it. The deletion is taken as "I have read this; move on."
- A user who wants the doc back can invoke
  `mnemo_vault_migration_doc(write: true)`, which idempotently writes
  the current state-of-vault snapshot to `_mnemo/MIGRATION.md`. The
  same tool with `write: false` returns the snapshot without writing,
  for users who prefer to read it via MCP rather than the filesystem.

This rule lives outside the standard fence contract because the doc's
contract is "say something once, then get out of the user's way." A
regenerating MIGRATION.md would nag indefinitely; a content-less file
would be confusing if the user re-opened the vault months later. The
write-once shape captures both intents cleanly.

The exception is local to MIGRATION.md. Every other file in `_mnemo/`
honours the standard fence contract.

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

#### Bridge edge cases

The behaviour of the bridge writer is fully specified for the awkward
cases — none of these are theoretical, all of them can happen in real
vaults.

- **Bridge configured, fence absent from anchor file.** mnemo appends
  a new `<!-- mnemo:bridge:<name> -->` block at the end of the anchor
  file (after a single blank line separator). The user's existing
  content above is untouched. On every subsequent sync the writer
  re-locates the named fence; once present, future writes happen
  in-place.
- **Bridge configured, fence present but moved by the user.** The
  writer locates the fence by its `<!-- mnemo:bridge:<name> -->`
  delimiter pair, not by line offset, so a user reordering or
  relocating the block within the file is honoured. Two competing
  blocks with the same name in one file are not allowed: on
  encountering them, mnemo logs an error, leaves the file alone, and
  reports the conflict via `mnemo_vault_status`.
- **Bridge configured, anchor file missing.** mnemo creates the file
  with just the bridge fence inside and a one-line header (e.g.
  `# Themes MOC`). Filenames containing spaces / non-ASCII work
  exactly as on the filesystem; no normalisation. Parent
  directories are created as needed.
- **Bridge configured, anchor file unwritable** (read-only, perm
  error, symlink loop, etc.). The writer logs a warn-level error
  with the path and reason, skips the bridge for this pass, and
  surfaces the failure in `mnemo_vault_status`. No retry storm.
- **Two bridges target the same anchor file.** Each bridge gets its
  own uniquely named fence (`mnemo:bridge:themes`,
  `mnemo:bridge:patterns`). Order in the file follows order of first
  appearance: if a fence does not yet exist, it is appended after
  the existing mnemo blocks. The two bridges never share a fence.
- **Bridge name in config does not match any configured entity
  collection.** A bridge maps a *collection name* (e.g. `themes`,
  `patterns`, `cross-repo`, `lessons`, `decisions`, `memories`) to an
  anchor path. Bridge names outside this enum (typos, future
  collections, custom names) are **skipped with a warning** — the
  unknown bridge is not written, the error is surfaced via
  `mnemo_vault_status` and a structured log line, and the daemon
  proceeds with the rest of the valid bridges. This is fail-soft:
  one typo does not break the others. The daemon never blocks
  startup on bridge config.
- **Bridge removed from config after blocks were written.** Next
  sync strips the corresponding fenced block but leaves the anchor
  file otherwise intact. The file is not deleted even if the bridge
  block was the entire content.

#### Per-collection bridge content

Bridge bodies differ by collection. The default renderings are:

- `themes`, `patterns`, `cross-repo`, `lessons`, `decisions` — flat
  bulleted list of wikilinks to the collection's pages, sorted by
  weight (or last-touched for decisions), capped at
  `vault_bridges.max_links` (default 50) per bridge to avoid the
  bridge becoming its own hairball.
- `memories` — grouped by source project; subsections of the form
  `### <project-name>` followed by the wikilinks to that project's
  memories. Because memories use `<project>-<name>.md` filename
  namespacing, the bridge is the place where they become navigable
  by project. Capped per project at `vault_bridges.max_links`.

Templates from `~/.mnemo/vault_templates/v2/bridge_<collection>.tmpl`
override the per-collection rendering. Standard template
error-handling rules apply (load-time validation, no silent
fallback).

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

Auto-detected at first sync from vault contents. Detection by **mere
presence** of a tool's dot-directory is brittle — multi-tool PKM users
(notably Logseq + Obsidian against the same directory) are common,
and `.obsidian/` can persist after a single exploratory open
months ago. mnemo uses recency-of-modification of the canonical signal
files instead:

| Tool      | Signal file                              |
|-----------|------------------------------------------|
| obsidian  | `<vault>/.obsidian/workspace.json`       |
| logseq    | `<vault>/logseq/config/config.edn`       |
| foam      | `<vault>/.foam/settings.json`            |

Detection rule:

1. Stat each signal file. Files that do not exist are dropped.
2. If exactly one signal file exists → that tool's profile.
3. If multiple signal files exist → pick the one whose mtime is most
   recent. Ties (≤ 1 hour apart) → prefer `obsidian` (most common),
   then `logseq`, then `foam`. Log the chosen tool + the alternative
   so the user can spot a mis-detection.
4. If none exist → `generic`.

User override via `vault_profile` config key always wins; auto-detect
only seeds the value when the user has not configured one.

The detection result is recorded in `mnemo_vault_status` alongside
the signal file path and its mtime, so the user can immediately see
why a given profile was picked.

### Templates (escape hatch)

```
~/.mnemo/vault_templates/
└── v2/
    ├── theme.tmpl
    ├── pattern.tmpl
    ├── cross-repo.tmpl
    ├── lesson.tmpl
    ├── decision.tmpl
    ├── memory.tmpl
    └── index.tmpl
```

Standard Go templates with the entity struct exposed. The shape of the
exposed entity struct is **versioned** by the parent directory (`v2/`
here). When the entity shape changes incompatibly in a future
release, mnemo will look in `v3/` (or whatever the new version is)
without silently rotting user templates: a `v2/theme.tmpl` written
today continues to render its v2 entity shape if mnemo still ships
the v2 renderer, otherwise it is reported as out-of-date and the
embedded default is used until the user upgrades it.

#### Load-time validation, not silent fallback

Behaviour when mnemo reads `~/.mnemo/vault_templates/v2/*.tmpl` at
startup and on every hot-reload:

- **File absent** → embedded default used. Quiet, expected. Logged at
  debug level.
- **File present, parses cleanly** → user template used.
- **File present, fails to parse / fails to execute against a probe
  entity** → error logged at warn level with the parse error and the
  template path. The vault sync worker for that specific entity type
  is **not** started until the template is fixed (or the file is
  removed). Other entity types continue rendering with their
  templates. The failure is surfaced in `mnemo_vault_status` so the
  user can see exactly which template is broken without grepping
  logs.

Silent fall-through to the embedded default on a malformed user
template is explicitly rejected: a user who has invested in a custom
template wants to know it broke, not have mnemo quietly render with
an unrelated shape and leave them puzzled about why their Dataview
queries stopped working.

Each template is executed once against a synthesised probe entity at
startup as part of validation. A template that parses but blows up
on execution (e.g. referencing a missing field) is caught here
rather than at first render.

Documented in `internal/vault/README.md` alongside the probe-entity
shape so users can author against a stable contract.

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

**Scope of `.mnemoignore`.** Patterns apply throughout the configured
scope, including inside `_mnemo/`. A user who wants to exclude a
specific generated page from the search index (e.g. an experimental
theme they have not curated yet) can add `_mnemo/themes/draft-*.md`
to the ignore file. The ignore file is **not** itself indexed.

Only one `.mnemoignore` is consulted (the one named by
`vault_indexing_ignore_file`, default `<vault>/.mnemoignore`). Nested
`.mnemoignore` files inside subdirectories are not honoured — keep
all patterns in the root file. This matches the gitignore-syntax
intent but not gitignore's nested-file behaviour; the simplification
is documented in `MIGRATION.md` for upgraders.

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
- Cross-repo themes get qualitatively better: user-authored notes
  in the vault are not attributed to any specific repo, so they
  function as topical magnets that pull together session content
  from multiple repos that the embedding model might otherwise
  cluster only weakly. (Vault content contributes to a cluster's
  topical centre but does not contribute to its `repos` field; the
  repo set comes from the session-attributed members.)

Slice 8's design document (`docs/design/vault-clustering.md`, to be
written) treats `vault_indexing_scope` as a first-class input.

### Information model

mnemo extracts six abstraction types. Each has a source, an extraction
recipe, and a representation in the wing.

#### 1. Themes (`themes/`)

**Source.** Decisions + compaction span summaries + persisted patterns
(Slice 7) + indexed user vault content (when scope permits), grouped by
topical similarity. Full spec lives in
`docs/design/vault-clustering.md`.

**Extraction.** Two-engine clustering pipeline. The **heuristic engine**
(TF-IDF + single-link agglomerative) is the **default and the realistic
engine** that ships with Slice 8 — fully local, no external dependencies.
An opt-in **embeddings engine** (per T63 egress posture; explicit
`vault_clustering.engine: "embeddings"` plus a provider key) swaps the
TF-IDF vectorisation for hosted-provider embeddings but reuses the
single-link clustering downstream. HDBSCAN is reserved for a follow-up
slice once a pure-Go binding meets the quality bar. Clusters of weight ≥
3 become themes. Labelling chain: user-anchored title (if a qualifying
`vault_user` member passes the quality gates) → optional LLM label
(separate egress opt-in) → bigram fallback.

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

The "Underlying decisions" heading in the example above is theme-
specific. Each entity type uses its own heading for the
provenance section:

| Entity type  | Provenance heading       |
|--------------|--------------------------|
| theme        | "Underlying decisions" (when sourced from decisions/compactions) or "Underlying entities" (mixed) |
| pattern      | "Occurrences"            |
| cross-repo   | "Per-repo evidence"      |
| lesson       | "Source decisions"       |
| decision     | "Proposal & confirmation" |
| memory       | (no provenance section — the page *is* the memory) |

Templates pick the heading by entity type; the embedded defaults
encode the table above. Custom templates can override per-entity.

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

### Worked example: post-Slice-8 vault for a real corpus

A reader cannot fully picture the design without seeing one
end-to-end output against plausible input. Concrete scenario: a
user has been running mnemo for six months across three repos
(`mnemo`, `foo`, `bar`), uses Obsidian, and has opted into
`vault_indexing_scope: "full"` and `vault_clustering.engine:
"embeddings"` with Voyage.

After the first post-Slice-8 sync, `<vault>/_mnemo/` looks like:

```
_mnemo/
├── index.md
├── README.md
├── themes/
│   ├── _index.md
│   ├── auth-middleware-redesign.md         ← user-anchored label
│   ├── schema-migration-safety.md          ← LLM label
│   ├── test-flake-investigations.md
│   └── ci-runner-tuning.md
├── patterns/
│   ├── _index.md
│   ├── direct-jsonl-reads.md               ← from Slice 7 patterns
│   └── repeated-decision-queries.md
├── cross-repo/
│   ├── _index.md
│   └── auth-middleware-redesign.md         ← same slug as themes/, mirrored
├── lessons/
│   ├── _index.md
│   ├── compliance-over-ergonomics.md
│   └── prefer-bundled-prs-for-area-refactors.md
├── decisions/
│   ├── _index.md
│   └── (12 high-signal decisions across 3 repos)
└── memories/
    ├── _index.md
    ├── mnemo-vault_library_wing.md
    ├── mnemo-user_preferences.md
    └── (10 more, one per source memory file)
```

Excerpt from `_mnemo/themes/auth-middleware-redesign.md` (the
user-anchored case — the user already had a note titled "Auth
middleware redesign" in their vault, body ≥ 200 tokens, no
daily-note filename, title shared "auth" with the cluster centroid):

```markdown
---
type: theme
tags: [mnemo, mnemo/theme]
aliases: ["Auth middleware redesign"]
weight: 11.4
first-seen: 2025-12-04
last-touched: 2026-05-18
repos: [mnemo, foo, bar]
label-source: vault_user
label-source-path: 10-areas/engineering/Auth middleware redesign.md
---

# Auth middleware redesign

<!-- mnemo:generated 2026-05-22T14:01:55Z -->

## Summary
Three repos converged on rip-and-replace pattern driven by legal review of
session-token storage. Decisions across mnemo, foo, and bar all favoured
clean rewrites over incremental patching.

## Evidence
- 2026-03-15 · mnemo · "legal flagged session-token storage in cookies"
- 2026-04-02 · foo · "compliance review demanded same change"
- 2026-04-28 · bar · "auth rewrite scheduled for Q2"
- 2026-05-09 · mnemo · "session token format finalised"
- 2026-05-14 · foo · "rip and replace landed without rollback"

## Related
- [[_mnemo/patterns/repeated-decision-queries|Repeated decision queries]]
- [[_mnemo/lessons/compliance-over-ergonomics|Compliance over ergonomics]]
- [[_mnemo/cross-repo/auth-middleware-redesign|Auth middleware redesign (cross-repo view)]]

## Underlying decisions
- mnemo · 2026-03-15 · `decision-id-abc1234`
- foo · 2026-04-02 · `decision-id-def5678`
- bar · 2026-04-28 · `decision-id-ghi9012`

<!-- /mnemo:generated -->

<!-- Your notes below this line — preserved across syncs. -->
```

Excerpt from `mnemo_vault_status` for the same session (truncated
to the relevant fields):

```json
{
  "vault_layout":   { "active": "v2", "first_seen": "2026-05-18T...", "days_in_both": 0 },
  "indexing_scope": { "active": "full", "external_files_indexed": 1284 },
  "abstractions":   { "themes": { "rendered": 4 }, "lessons": { "rendered": 2 } },
  "last_cluster_run": { "engine": "embeddings", "estimated_cost": 0.04 }
}
```

Bridge anchor file `10-areas/knowledge/Themes MOC.md` after the
sync (user content untouched above, mnemo block appended once):

```markdown
# Themes MOC
(user's own MOC notes here, unchanged)

<!-- mnemo:bridge:themes -->
- [[_mnemo/themes/auth-middleware-redesign|Auth middleware redesign]]
- [[_mnemo/themes/schema-migration-safety|Schema migration safety]]
- [[_mnemo/themes/test-flake-investigations|Test flake investigations]]
- [[_mnemo/themes/ci-runner-tuning|CI runner tuning]]
<!-- /mnemo:bridge:themes -->
```

This is what a reviewer can hold in their head while reading the
rest of the design. The numbers in this example (4 themes,
`weight: 11.4`, `external_files_indexed: 1284`) are illustrative
and do not need to match other example snippets elsewhere in the
doc — each example was crafted for its surrounding context, not
woven into a single coherent fictional corpus.

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
    "engine": "heuristic",
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
| `vault_layout.soak_warn_after` | `"720h"` (30 days; soak window before "both"-layout warning fires) | yes |
| `vault_bridges`              | `{}`                             | yes         |
| `vault_indexing_scope`       | `"_mnemo_only"` for new vaults; `"full"` for v1-populated vaults | yes |
| `vault_indexing_includes`    | `[]`                             | yes         |
| `vault_indexing_ignore_file` | `".mnemoignore"`                 | yes         |
| `vault_clustering.engine`    | `"heuristic"` (explicit opt-in to `"embeddings"`, per T63 egress posture) | yes |
| `vault_clustering.min_cluster_weight` | `3`                     | yes         |
| `vault_clustering.recompute_interval` | `"24h"`                 | yes         |
| `vault_clustering.retire_after` | `"4320h"` (180 days)            | yes         |
| `vault_clustering.max_themes` | `200`                               | yes         |
| `vault_clustering.max_cross_repo` | `50`                            | yes         |
| `vault_bridges.max_links`    | `50`                                 | yes         |
| `vault_legacy_annotations.max_file_kb` | `512` (split aggregate when exceeded) | yes |

`vault_layout` accepted values:

- `"v2"` — write to `_mnemo/` only. Stop writing v1 paths.
- `"both"` — write to both `_mnemo/` and v1 root-level dirs. Used during
  migration window.
- `"v1"` — write to v1 root-level dirs only. Available as an emergency
  escape hatch; not advertised; **removed in the release after the
  Slice 9 GC tool reaches General Availability** (i.e. once the
  user-paced migration path is fully proven). Until then, setting
  this value logs a warning each sync ("vault_layout=v1 is the
  emergency escape hatch and will be removed in release X.Y").
  The removal slice replaces the value with a hard error at config
  load and is tracked as its own bullseye sub-target.

#### Soak-time TTL for `"both"`

`"both"` is meant as a migration window, not a steady state. A user who
upgrades and forgets sits in dual-write indefinitely, doubling
mnemo's footprint inside the vault and producing two parallel
graph hairballs (one for the v1 root-level dirs, one for `_mnemo/`).

To prevent silent indefinite dual-write:

- A `vault_layout_first_seen` timestamp is written to `~/.mnemo/state.json`
  the first sync mnemo observes the active layout (separate from
  `~/.mnemo/config.json` so the user does not edit it).
- While `vault_layout == "both"`:
  - The first sync after **30 days** of soak emits a structured warning
    log: `vault_layout="both" for N days — opt into "v2" or run
    mnemo_vault_gc_legacy to finish migrating`.
  - `mnemo_vault_status` surfaces `vault_layout`, `days_in_both`, and a
    recommendation (`"opt into v2"`, `"run gc_legacy"`, `"still within
    soak"`) on every call. The user sees the state without enabling
    debug logging.
  - The warning repeats on a weekly cadence afterwards, never
    auto-promoting the layout. The user retains the timing decision.
- The soak window is configurable via `vault_layout.soak_warn_after`
  (default `"720h"` = 30 days). A user who genuinely wants long-running
  dual-write sets the value higher; the warning is suppressed
  accordingly.

The recommendation surface is one-way (warn, never auto-narrow): the v1
data may still be load-bearing for a workflow we cannot infer. The
"`both` during soak" review question raised on PR #95 is resolved by
this TTL/visibility mechanism plus the explicit `gc_legacy` tool —
there is no auto-deletion, but there is no silent forever-dual-write
either.

#### Recommendation state machine

The `mnemo_vault_status.vault_layout.recommendation` string takes
one of four values, deterministic from observable state:

| Observed state                                                                 | Recommendation       |
|--------------------------------------------------------------------------------|----------------------|
| `vault_layout == "both"` AND `hours_in_both < soak_warn_after_hours`           | `"still within soak"` |
| `vault_layout == "both"` AND `hours_in_both ≥ soak_warn_after_hours`           | `"opt into v2"`       |
| `vault_layout == "v2"` AND v1 root-level dirs still present in the vault       | `"run gc_legacy"`     |
| any other state                                                                | `""` (empty)          |

`hours_in_both` is derived from `state.json.vault_layout_first_seen.both`;
`soak_warn_after_hours` is the parsed duration of
`vault_layout.soak_warn_after` (default `"720h"`). The comparison
is done in hours throughout to avoid integer-day rounding errors
near the soak boundary. `days_in_both` exposed on the status response
is `hours_in_both ÷ 24` rounded to the nearest integer.

The empty-recommendation row covers: `vault_layout == "v1"` (no
migration owed); `vault_layout == "v2"` with no v1 leftovers
(migration complete); `vault_layout == "both"` with no v1 dirs
present yet (transient first-sync state — v1 dirs get created on
the same sync; subsequent sync flips to one of the warn rows).

Transitions are observation-driven: the recommendation is
recomputed each `mnemo_vault_status` call and each sync. There is
no persistent state machine in the daemon — the recommendation is
a pure function of the observable state. A user who runs
`gc_legacy` and removes the v1 dirs gets the empty recommendation
on the next status call, no manual reset needed.

---

## Operational invariants

A small set of invariants the implementation honours across the whole
design. Each is testable in CI.

### Daemon-managed state file (`state.json`)

`~/.mnemo/state.json` is a daemon-managed sidecar that holds
runtime-derived state that should not pollute the user-editable
`config.json` but must survive daemon restarts. It is created on
first daemon boot post-Slice 1 with empty defaults; it is read on
every boot and written atomically (`tmp + rename`) whenever a
tracked field changes.

Schema (additive only — new fields appear over time, old ones never
disappear silently):

```json
{
  "version": 1,
  "vault_path": "~/Documents/PKM",
  "vault_layout_first_seen": {
    "v1":   "2025-11-04T09:12:33Z",
    "both": "2026-05-22T14:01:55Z",
    "v2":   null
  },
  "indexing_scope_first_seen": {
    "_mnemo_only": null,
    "full":        "2025-11-04T09:12:33Z",
    "includes":    null
  },
  "embedding_fingerprint": null,
  "last_cluster_run_id": 184,
  "broken_templates": [],
  "bridge_errors":    []
}
```

`embedding_fingerprint` is `null` when the active clustering engine
is `"heuristic"`. When the user opts into `"embeddings"`, the
fingerprint object materialises on the next pass:

```json
"embedding_fingerprint": {
  "provider": "voyage",
  "model":    "voyage-3-lite",
  "version":  "2025-09",
  "last_used": "2026-05-22T14:01:55Z"
}
```

`vault_path` records the path mnemo wrote against on the most
recent sync. The "`vault_path` change semantics" rule compares the
new config-loaded `vault_path` against this recorded value to
detect a change and reset the soak-TTL counters.

`last_cluster_run_id` mirrors `cluster_runs.id` for the most
recent successful pass; the daemon uses it on boot to detect
whether a pass was in progress at last shutdown without scanning
the table.

Properties:

- Single-user. The library wing serves the local daemon's single
  user; team-mnemo (T36) maintains its own per-user sidecars in a
  different location.
- Forbidden as a configuration surface. Users edit `config.json`,
  never `state.json`. `mnemo_vault_status` exposes a read view if
  the user wants to inspect.
- Atomic writes. Implementations write `~/.mnemo/.state.json.tmp`
  on the same filesystem, `fsync`, then `rename` over
  `~/.mnemo/state.json`. The tempfile uses the same leading-dot
  convention as the in-vault rendered files. A daemon crash
  mid-write leaves at most one stale `.state.json.tmp`, swept on
  next boot before any new write begins.
- Version handling. The top-level `version` integer governs
  schema-shape changes that go beyond additive-field. Rules:
  - Daemon reads `state.json` with `version == X`. If `X` equals
    the daemon's known version, proceed normally.
  - If `X` is below the daemon's known version, the daemon
    upgrades the file in place (migration functions registered per
    version step; rewrite atomically; bump `version`).
  - If `X` is above the daemon's known version (running an older
    binary against newer state), the daemon refuses to write
    `state.json` for this boot and runs in **read-only state mode**
    — soak-TTL counters report against the recorded data but no
    new layout transitions are persisted. Logs prominent warning
    so the user can choose to upgrade the binary or revert the
    state file from backup. Refusing-to-write prevents the older
    daemon from silently dropping fields it does not understand.
- Forward compatibility within a version. Unknown top-level keys
  inside a known `version` are preserved on rewrite (read as
  opaque, written back verbatim) so a newer daemon's additions
  survive a downgrade-then-upgrade cycle.
- Concurrency. Only one daemon process per user is supported; a
  second process attempting to start with `state.json` already
  locked aborts with a clear error rather than racing.

### Sync atomicity

The renderer never leaves the vault in a torn state. Every generated
file follows the same write protocol:

1. Render the new content in memory (including the regenerated
   above-fence block and the preserved below-fence content read at
   the start of the cycle).
2. Write to a sibling tempfile `.<name>.mnemo.tmp` in the target
   directory. The leading-dot prefix is intentional: most PKM tools
   (Obsidian, Logseq, Foam) skip dotfiles when building their note
   index and graph view, so a half-written `.foo.md.mnemo.tmp` does
   not transiently appear as a broken note even if the user's PKM
   tool scans the directory during the rename window.
3. `fsync` the tempfile.
4. `rename` over the destination — atomic on every supported
   filesystem.
5. Remove any stale `.<name>.mnemo.tmp` from a prior crashed run on
   startup, before any new write begins.

The bridge writer follows the same protocol against the anchor
file with one additional rule: the anchor file is rewritten **as a
whole**, not edited in place. The writer reads the existing anchor
file, splices the new fenced block(s) into the read content
(preserving every byte outside the bridge fences), writes the
spliced result to a sibling `.<name>.mnemo.tmp`, fsyncs, and
renames. Atomic-rename gives whole-file atomicity; the in-memory
splice gives fence-boundary correctness. A reader of the anchor
file at any moment sees either the old whole or the new whole,
never a mix.

Concurrent reads from the user's PKM tool always observe a
complete file with matching fences. A daemon crash mid-sync
leaves at most one extra `.tmp` file, swept on next boot.

### `vault_path` change semantics

Hot-reload of `vault_path` is permitted (via `mnemo_config`) with
explicit semantics:

- The old `vault_path` is **not** auto-migrated to the new location.
  Files at the old path remain where they are; mnemo simply stops
  writing there.
- The new path is treated as a fresh vault: the same auto-detect
  rules run (v1 dirs present? `_mnemo/` present? profile signal
  files?). Defaults seed accordingly.
- `state.json` records the change in `vault_layout_first_seen` /
  `indexing_scope_first_seen` against the new path's detected
  layout, so soak-TTL counters reset.
- `mnemo_vault_status` reports the change at the next call. A
  warning surfaces if the old path still contains a `_mnemo/`
  wing — the user is informed but mnemo takes no action.

The "don't auto-migrate" rule is deliberate: cross-path moves can
cross filesystems / mountpoints / sync providers (Dropbox, iCloud)
where mnemo cannot guarantee write semantics. A user who wants the
content moved does so themselves before pointing `vault_path`.

### `_mnemo/` self-exclusion from ingest

The path-exclusion registry (PR #78) is updated on every
`vault_path` change to include the new `_mnemo/` location. This
prevents mnemo from re-ingesting its own output as session corpus —
a v1 footgun. The exclusion is observable via
`mnemo_vault_status.excluded_paths`.

### Back-pressure on clustering pass output

Several sections demand caps (`max_themes`, `max_cross_repo`,
`vault_bridges.max_links`). The implementation enforces them
**before** the expensive work:

- Embedding pass cost is bounded by the total corpus size at the
  active (provider, model, model_version) fingerprint — every
  embed call corresponds to one cache miss, never speculative
  over-embedding. Caps on cluster count do not prevent corpus
  vectorisation (every member of every cluster still needs a
  vector) — they prevent unbounded *page emission* and *LLM
  labelling*, which are the next two bullets.
- Renderer hard-caps page emission per pass: themes ranked by
  weight, excess kept in the `themes` table but not rendered. The
  capped themes are listed in `_mnemo/themes/_index.md` under
  "Below cap (not rendered)" so the user sees the cap is binding.
- LLM labelling is invoked at most `max_themes` times per pass;
  excess clusters use the bigram fallback even if LLM labelling is
  otherwise enabled.
- Bridge writer truncates lists at `vault_bridges.max_links` and
  appends a "(N more, see <collection>/_index.md)" trailing line so
  the truncation is visible.

### `mnemo_vault_status` response schema

The status tool is the single canonical surface for everything the
design demands users be able to observe. It is consulted in dozens
of places throughout this doc; the schema lives here so
implementers do not have to grep.

```json
{
  "vault_path": "~/Documents/PKM",
  "vault_path_exists": true,
  "vault_profile": {
    "active":         "obsidian",
    "source":         "auto-detect",
    "signal_file":    ".obsidian/workspace.json",
    "signal_mtime":   "2026-05-15T08:11:02Z",
    "alternatives":   [
      { "profile": "logseq", "signal_mtime": "2024-11-03T17:55:08Z" }
    ]
  },
  "vault_layout": {
    "active":          "both",
    "first_seen":      "2026-05-01T09:12:33Z",
    "days_in_both":    22,
    "recommendation":  "still within soak",
    "soak_warn_after_hours": 720
  },
  "indexing_scope": {
    "active":         "full",
    "includes":       [],
    "ignore_file":    ".mnemoignore",
    "external_files_indexed": 1284
  },
  "abstractions": {
    "themes":     { "rendered": 47,  "archived": 6, "below_cap": 0 },
    "patterns":   { "rendered": 18,  "below_cap": 0 },
    "cross-repo": { "rendered": 11 },
    "lessons":    { "rendered": 23 },
    "decisions":  { "rendered": 156 },
    "memories":   { "rendered": 84 }
  },
  "v1_leftovers": { "sessions": 412, "ci": 53, "prs": 89, "repos": 7 },
  "last_cluster_run": {
    "id":             184,
    "started_at":     "2026-05-22T13:58:30Z",
    "ended_at":       "2026-05-22T13:58:32Z",
    "engine":         "heuristic",
    "estimated_cost": 0.0,
    "trigger":        "interval"
  },
  "embedding_fingerprint": null,
  "broken_templates":  [],
  "bridge_errors":     [],
  "excluded_paths":    ["~/Documents/PKM/_mnemo", "~/Documents/PKM/.git"],
  "warnings":          []
}
```

The `embedding_fingerprint` field is `null` whenever the active
clustering engine is `"heuristic"` (matching the `state.json` rule
above). When the user opts into `"embeddings"`, the field
materialises:

```json
"embedding_fingerprint": {
  "provider":  "voyage",
  "model":     "voyage-3-lite",
  "version":   "2025-09",
  "last_used": "2026-05-22T13:58:32Z"
}
```

`abstractions.themes` reports both `rendered` (live in
`_mnemo/themes/`) and `archived` (live in `_mnemo/themes/_archive/`
per the retirement rule).

Entry shapes for the array fields:

```json
"broken_templates": [
  {
    "template_path": "~/.mnemo/vault_templates/v2/theme.tmpl",
    "entity_type":   "theme",
    "phase":         "parse" | "execute",
    "error":         "undefined field .Repos at line 14"
  }
]

"bridge_errors": [
  {
    "name":         "themes",
    "anchor_path":  "10-areas/knowledge/Themes MOC.md",
    "reason":       "unknown_collection" | "anchor_unwritable" | "duplicate_fence" | "anchor_create_failed",
    "detail":       "permission denied (ENOENT on parent)"
  }
]

"warnings": [
  {
    "code":      "embedding_model_changed",
    "message":   "embedding model changed from voyage-3-lite to voyage-3; clusters will be regenerated",
    "since":     "2026-05-22T14:01:55Z"
  }
]
```

All fields are stable across daemon restarts insofar as the
underlying state permits. New fields are additive; old fields never
disappear silently.

`warnings[]` is the bucket for one-off transient signals — broken
state, recent crash recovery, soak past the warning threshold.
Implementers should prefer surfacing structured state in the typed
fields above and reserve `warnings[]` for things that genuinely do
not fit.

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
    auto-default `vault_layout` to `"both"`; **write-once** emit
    `_mnemo/MIGRATION.md` explaining the change, what to expect, and how
    to opt fully in. The MIGRATION.md doc is never regenerated; once
    written or deleted by the user, mnemo does not touch it again.
  - If neither → new vault; default `vault_layout` to `"v2"`.
- On every startup that observes a layout transition (especially into
  `"both"`), update `~/.mnemo/state.json` with `vault_layout_first_seen`
  for that layout value, used by the "both" soak-TTL warning logic
  documented under the configuration section.
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
  - **Pagination.** If the aggregate would exceed
    `vault_legacy_annotations.max_file_kb` (default 512 KB), the
    sweep splits into numbered files
    (`legacy-annotations-001.md`, `-002.md`, …) grouped by source
    subdir (`sessions/`, `ci/`, `prs/`, …) so the per-file table
    of contents stays navigable. An index page
    `_mnemo/legacy-annotations.md` lists the parts when split.
    Files that contain zero below-fence content are omitted from
    the aggregate entirely (a v1 dir of pure generated content
    has nothing to harvest).
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
  `~/.mnemo/vault_templates/v2/` once layout v2 stabilises. No
  interaction with the migration path.
- **Archetype F (casual reader).** Opens `_mnemo/index.md`. Finds themes,
  patterns, lessons. Does nothing else.

---

## Implementation roadmap

Nine slices. Each is independently shippable, reviewable, and reversible.

Slices 1–4 form the **MVP v2** boundary — coherent and shippable on its
own. Slices 5–9 are independent follow-ups, not a single arc.

### Slice 1 — indexing scope + `.mnemoignore` (consent fix)

Introduce `vault_indexing_scope`, `vault_indexing_includes`, and
`vault_indexing_ignore_file` config keys. Refactor `IngestVaultAnnotations`
to walk only the configured scope. Implement gitignore-syntax matcher
against the `.mnemoignore` file. Wire migration defaults: new vaults
default to `"_mnemo_only"`; existing v1-populated vaults default to
`"full"` for continuity. Report active scope in `mnemo_vault_status`.

This slice lands **first**, ahead of any `_mnemo/` namespace work, because
it is the only slice in the plan that fixes a real shipped *bug* — v1's
silent full-vault read without user consent. The fix is independently
correct even before the rest of the redesign exists. New vaults get the
default `"_mnemo_only"` scope (which trivially reads nothing while
`_mnemo/` does not yet exist); existing v1 vaults keep `"full"` semantics
for continuity. Either way, the user-visible behaviour is the right one
from day one of the v2 release window.

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

### Slice 2 — `_mnemo/` namespace + layout config

Plumbs the new directory root and the `vault_layout` config key. No new
abstractions yet. v1 writers retained, gated on `vault_layout`. Adds
`_mnemo/index.md` and `_mnemo/README.md` as static-ish content.

*Value:* nothing user-visible yet, but establishes the namespace and lets
later slices write into it. Required precondition for everything below
Slice 4.

### Slice 3 — move decisions + memories under `_mnemo/`

Re-render the two existing abstractions (decisions, memories) under their
new paths inside `_mnemo/`. Above-fence content gets the new frontmatter
shape. Old v1 decision/memory files are still written when
`vault_layout="both"`.

*Value:* the two existing semantic exports become available in the new
layout. Validates the rendering shape, frontmatter, and graph topology
before more abstractions are added.

### Slice 4 — drop session/CI/PR/repo writers from `_mnemo/`

When `vault_layout="v2"`, do not write session, CI, PR, or repo-index pages
to `_mnemo/`. v1 paths continue to honour the old behaviour under
`vault_layout="both"` or `"v1"`.

*Value:* validates the principle that raw signal stays in SQLite. The user
opting into v2 immediately sees a calmer, smaller wing.

**Slices 1–4 are the MVP v2 boundary.** After Slice 4, the wing is
consent-correct, namespaced, and free of raw-signal noise. Each of
Slices 5–9 below ships independently afterwards.

### Slice 5 — PKM profile + auto-detect

Introduce the `vault_profile` config key, the per-profile renderer hooks,
and the auto-detect logic. Detection uses **mtime of the canonical
signal files** (`.obsidian/workspace.json`, `logseq/config/config.edn`,
`.foam/settings.json`), not directory presence — see "PKM profile" in
the Architecture section for the full rule. The detected profile, the
signal file path, and its mtime are recorded in `mnemo_vault_status`.

*Value:* Logseq/Foam/generic users get a usable wing. Multi-tool PKM
users (Obsidian + Logseq on the same directory) are detected by
recent-activity, not first-open accident.

### Slice 6 — bridges

`vault_bridges` config key. Fenced-block writer with the safety constraints
described above. New MCP tool: `mnemo_vault_bridge_list` to inspect active
bridges.

*Value:* Archetype B (heavy PKM users) get their main need met. Without
bridges, mnemo content is segregated; with bridges, it surfaces in user
MOCs without invading.

### Slice 7 — patterns (persisted + rendered)

Promote `mnemo_discover_patterns` from live-queried to persisted. New
`patterns` table (additive, append-only per schema policy):

```sql
CREATE TABLE patterns (
  id              TEXT PRIMARY KEY,         -- pattern_<sha1(type+canonical_signature)[:8]>
  pattern_type    TEXT NOT NULL,            -- "direct_jsonl_read" | "repeated_query_shape" | ...
  signature       TEXT NOT NULL,            -- canonicalised input snippet that defines the pattern
  occurrence_count INTEGER NOT NULL,
  session_count   INTEGER NOT NULL,         -- distinct sessions where pattern observed
  repos           TEXT NOT NULL,            -- JSON array of repo names
  first_seen      TEXT NOT NULL,
  last_seen       TEXT NOT NULL,
  representative_excerpts TEXT NOT NULL,    -- JSON array of excerpt strings
  computed_at     TEXT NOT NULL
);

CREATE VIRTUAL TABLE patterns_fts USING fts5(
  pattern_type, signature, representative_excerpts,
  content='patterns', content_rowid='rowid'
);
```

Renderer emits `_mnemo/patterns/` pages for patterns meeting the
`occurrence ≥ 3` and `session_count ≥ 2` threshold (carried over
from the live `mnemo_discover_patterns` filter).

*Value:* first new abstraction. Patterns are the cheapest abstraction to
extract (purely heuristic) and the highest signal per unit effort.
Slice 8's clustering corpus (`vault-clustering.md` Inputs §3) reads
this table directly.

### Slice 8 — themes + cross-repo (clustering engine)

The research-shaped slice. Two-engine clustering pipeline over
decisions + compaction summaries + persisted patterns (Slice 7) +
indexed user content when scope permits. The **heuristic engine**
(TF-IDF + single-link agglomerative) is the default and ships first;
the **embeddings engine** is opt-in (per T63 egress posture) and
reuses single-link clustering downstream of vectorisation. HDBSCAN
is deferred. New tables: `themes`, `theme_members`,
`cluster_embeddings`, `themes_fts`, `cluster_runs`, `theme_overrides`,
`theme_pins` (additive). Cross-repo pages derived from themes with
repo set ≥ 2 and weight ≥ 4.0.

This slice carries the most risk and is governed by the subordinate
design document `docs/design/vault-clustering.md`.

*Value:* the core promise of the redesign. Without themes, the wing is a
better-organised v1; with themes, it is genuinely new.

### Slice 9 — lessons + GC tool

Lessons extractor (feedback memories + confirmed-and-surviving decisions).
`mnemo_vault_gc_legacy` tool. `mnemo_vault_migration_doc` tool (opt-in
regen of MIGRATION.md per the fence-contract exception).
`_mnemo/legacy-annotations.md` harvester.

*Value:* completes the abstraction set and gives Archetype D users a
clean migration path off v1.

### MCP tool inventory (cross-slice)

Every new MCP tool introduced by this design, indexed by the slice
that owns it. Implementers should treat this as the canonical
checklist — an item missing from a slice's PR is a slice not done.

| Tool                                | Slice | Purpose                                                 |
|-------------------------------------|-------|---------------------------------------------------------|
| (config keys via existing `mnemo_config`) | 1 | indexing-scope keys land via the existing hot-reload  |
| `mnemo_vault_status` (extended)     | 1+    | reports active layout, profile, scope, indexed-file count outside `_mnemo/`, soak-TTL recommendation, broken-template list, bridge errors, last `cluster_runs` row. Single canonical surface — see "`mnemo_vault_status` response schema" below. |
| `mnemo_vault_bridge_list`           | 6     | inspect active bridges and their anchor files           |
| `mnemo_vault_recluster`             | 8     | trigger an immediate clustering pass. Params: `engine` optional override (`"heuristic"`/`"embeddings"`); `force_reembed: bool` (default false) to invalidate the active-fingerprint cache rows and re-embed cleanly — useful when a provider silently revises a model under the same version string. Returns the new `cluster_runs` row. |
| `mnemo_vault_themes_inspect`        | 8     | full member list + distances + centroid + labelling source for a theme slug or ID |
| `mnemo_vault_themes_split`          | 8+    | mark theme for split on next pass; persisted in `theme_overrides` |
| `mnemo_vault_themes_merge`          | 8+    | inverse of split                                        |
| `mnemo_vault_themes_pin`            | 8+    | mark theme as never-auto-retire; persisted in `theme_pins` |
| `mnemo_vault_gc_legacy`             | 9     | user-initiated deletion of v1 root-level dirs after annotation-safety check |
| `mnemo_vault_migration_doc`         | 9     | opt-in regen of `_mnemo/MIGRATION.md` (write-once exception to the fence contract) |

### Templates (cross-cutting)

Shipped between Slice 4 and Slice 5 as a refactor: the renderer is
restructured to load templates from `~/.mnemo/vault_templates/v2/`
with embedded defaults. Validation runs at startup; broken
user templates surface in `mnemo_vault_status` rather than silently
falling back. No user-facing change at landing; unblocks Archetype E
once layout v2 is stable.

---

## Risks and open questions

### Indexing-scope consent migration (Slice 1)

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

**Mitigation:** Slice 8 ships with the heuristic engine
(`vault_clustering.engine="heuristic"`) as the **default and the
realistic engine**, not a fallback. The embeddings engine is an opt-in
addition. The heuristic engine is explicitly designed to be non-toy
(see `docs/design/vault-clustering.md` Engine B). Document the known
failure modes. Allow `min_cluster_weight` tuning. Provide
`mnemo_vault_themes_inspect` for users to see cluster membership and
flag misclusters.

### Privacy of cross-repo content

Themes can surface text from one repo's sessions in pages cited from
another repo's view. For solo users this is fine. For multi-tenant
deployments (team-mnemo, T36), the cross-repo surface area becomes a
policy question.

**Resolution:** out of scope here. team-mnemo's access model governs that
case. For single-user vaults, all content is the same user's — but
that does not mean it is all of the same *domain*. A solo user's
vault commonly mixes work, personal, and side-project content; a
theme that surfaces a cluster spanning work decisions and personal
journal notes can be unwelcome even if technically owned by the
same user. `.mnemoignore` is the user's only knob for this case:
add `personal/`, `journal/`, or similar to keep them out of the
clustering corpus. `mnemo_vault_status.indexing_scope.external_files_indexed`
exposes how many user notes are reaching the corpus so the user
can audit the surface area. Per-note `mnemo: ignore` (out-of-scope
follow-up) will be the finer-grained knob.

### Embedding cost

Computing embeddings for every decision summary and compaction span at
each clustering pass is bounded but not free. A user with 1000 sessions ×
several decisions each is plausible.

**Resolution:** cache embeddings keyed by `(doc_kind, entity_id,
content_hash, provider, model, model_version)`. Recompute only when any
of those change; a model swap leaves the prior rows in the cache for
revert. Estimated cost for the median user is single-digit cents per
re-cluster. For users who do not opt into the embeddings engine, the
heuristic engine handles the corpus with zero external calls.

### Profile auto-detect false positives

A vault containing `.obsidian/` because the user opened it once in
Obsidian but actually uses Logseq daily would be mis-detected by a
mere-presence check.

**Resolution:** detection is by **mtime of the canonical signal file**,
not directory presence (see "PKM profile" above). The
most-recently-modified signal file wins; ties tip towards Obsidian.
Result is recorded in `mnemo_vault_status` with the signal file path
and mtime so the user can audit. `vault_profile` config key always
overrides regardless of auto-detect.

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

**Resolution:** two orthogonal keys, two defaults:

- `vault_layout` — defaults to `"both"` until the user explicitly sets
  it. Continued dual-write is the safe choice; the soak-TTL warning
  kicks in after 30 days.
- `vault_indexing_scope` — if unset, defaults to `"full"` (continuity
  with the v1 vault's effective read scope).

These resolve independently because they govern different surfaces
(write vs. read). `MIGRATION.md` documents both states and shows the
one-line `mnemo_config` calls to commit either way.

---

## Success metrics

These determine whether the design delivered its promised value once
shipped. All are **[metric]** rather than gates — reviewable in the
post-launch retro, not blocking on slice landing.

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
- **Vault-as-input override layer.** Slice 1 makes the vault readable as
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
following are true. Each item is marked **[gate]** (ship-blocking,
CI-testable) or **[metric]** (post-launch signal, validated
qualitatively or via survey). A slice does not ship without its
gates passing; metrics are reviewed in the post-launch retro.

1. **[gate]** `<vault>/_mnemo/` is the only directory written by mnemo
   for new vault configurations.
2. **[gate]** Existing v1 vaults can be migrated to v2 with zero
   annotation loss and user-paced timing.
3. **[gate]** Themes, patterns, cross-repo, lessons, decisions
   (filtered), and memories are all materialised under `_mnemo/`.
4. **[gate]** The wing renders correctly in Obsidian, Logseq, Foam, and
   a plain Markdown editor without per-tool configuration. Verified by
   golden-file tests per profile.
5. **[gate]** Bridges work end-to-end against at least Obsidian and
   Logseq.
6. **[gate]** Template overrides work for at least one entity type
   end-to-end.
7. **[gate]** `mnemo_vault_status` returns the full schema documented in
   "Operational invariants → `mnemo_vault_status` response schema",
   including layout, profile, indexing scope, abstraction counts,
   `external_files_indexed`, `v1_leftovers`, `embedding_fingerprint`,
   `broken_templates`, `bridge_errors`.
8. **[gate]** `mnemo_vault_gc_legacy` removes v1 leftovers after
   annotation safety check.
9. **[gate]** Indexing scope defaults to `"_mnemo_only"` on new vaults
   and `"full"` on v1-populated vaults at upgrade; both behaviours
   covered by tests.
10. **[gate]** `.mnemoignore` (gitignore syntax) is honoured at vault
    root in `"full"` and `"includes"` scopes; verified by tests
    including nested-pattern cases and patterns inside `_mnemo/`.
11. **[gate]** MVP v2 (Slices 1–4) is independently shippable and
    reviewable as a coherent release. Slice 1 (indexing-scope consent
    fix) lands ahead of any `_mnemo/` namespace work.
12. **[gate]** `vault_layout="both"` triggers a structured warning +
    status surface after the configured soak window (default 30 days).
    The warning never auto-promotes or auto-deletes; the user owns the
    timing.
13. **[gate]** PKM profile auto-detect uses mtime of signal files
    (`.obsidian/workspace.json`, `logseq/config/config.edn`,
    `.foam/settings.json`), not directory presence. Tied vaults log
    the alternative.
14. **[gate]** `_mnemo/MIGRATION.md` is write-once and not regenerated.
    `mnemo_vault_migration_doc` (Slice 9) exists for opt-in
    regeneration.
15. **[gate]** Custom templates live in `~/.mnemo/vault_templates/v2/`
    and are validated at startup. A malformed user template surfaces
    in `mnemo_vault_status.broken_templates` and does not silently
    fall through to the embedded default.
16. **[gate]** Bridge edge cases (missing fence, two bridges per
    anchor, unknown bridge name, unwritable anchor) all have specified
    behaviour matching the doc; verified by tests.
17. **[gate]** Sync atomicity protocol holds: a daemon crashed
    mid-write leaves no torn `<file>.md` and at most one
    `.<name>.mnemo.tmp` per affected directory, cleaned on next boot.
18. **[gate]** `state.json` survives daemon restart, supports forward-
    and backward-compatible field set, and is never read or written
    by user-facing config flows.
19. **[gate]** Hot-reload of `vault_path` does not auto-migrate the old
    location, seeds fresh defaults for the new path, and surfaces a
    warning if a `_mnemo/` wing remains at the old path.
20. **[gate]** Back-pressure: `max_themes`, `max_cross_repo`,
    `vault_bridges.max_links` short-circuit the expensive paths
    (embedding, LLM labelling, rendering) and not just the final
    output stage.
21. **[gate]** `_mnemo/` is registered with the path-exclusion
    registry (PR #78) on every `vault_path` change, observable via
    `mnemo_vault_status.excluded_paths`.
22. **[gate]** Themes whose most recent member `ts` exceeds
    `vault_clustering.retire_after` (default 180 days) move to
    `_mnemo/themes/_archive/` on the next clustering pass. Themes
    listed in `theme_pins` are never auto-retired regardless of age.
    `mnemo_vault_themes_pin` adds and removes pins.

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
  scope-as-trees model in Slice 1 reuses these tables. Each scope
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
