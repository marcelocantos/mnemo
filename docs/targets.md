# Convergence Targets

Active targets tracked by mnemo's convergence machinery. Each
target is a desired end-state with a status, a weight, and a
description. Sub-targets (`T<n>.<m>`) belong to their parent and
ship independently.

---

### 🎯T64 Vault library wing redesign (v2)

- **Status**: identified
- **Weight**: 10

Parent target for the vault library wing redesign described in
`docs/design/vault-library-wing.md`. Replaces the v1 raw-row export
(PR #74, #77, #78) with a fenced `<vault>/_mnemo/` wing carrying
second-order abstractions (themes, patterns, cross-repo,
lessons, filtered decisions, memories). Subordinate clustering
engine design lives in `docs/design/vault-clustering.md`.

Ships as nine independently reviewable slices (T64.1–T64.9). MVP
v2 boundary is T64.1–T64.4 — the wing is consent-correct,
namespaced, and free of raw-signal noise after that subset lands.
T64.5–T64.9 are independent follow-ups, not a single arc.

### 🎯T64.1 Indexing scope + .mnemoignore (consent fix)

- **Status**: identified
- **Weight**: 3

Introduce `vault_indexing_scope` (`"_mnemo_only"` | `"full"` |
`"includes"`), `vault_indexing_includes`, and
`vault_indexing_ignore_file` config keys. Refactor
`IngestVaultAnnotations` to walk only the configured scope.
Implement gitignore-syntax matcher.

Lands ahead of any `_mnemo/` namespace work because it is the only
slice in the plan that fixes a real shipped *bug* — v1's silent
full-vault read without user consent. New vaults default
`"_mnemo_only"`; existing v1 vaults default `"full"` for
continuity. Reuses T53 `trees_of_interest` + `doc_tree_refs`.
Reports active scope via `mnemo_vault_status`.

### 🎯T64.2 `_mnemo/` namespace + vault_layout config

- **Status**: identified
- **Weight**: 2

Plumb the new directory root and the `vault_layout` config key
(`"v2"` | `"both"` | `"v1"`). v1 writers retained, gated on
`vault_layout`. Add `_mnemo/index.md`, `_mnemo/README.md`, and the
write-once `_mnemo/MIGRATION.md` exception for v1-detected vaults.
Track `vault_layout_first_seen` in `~/.mnemo/state.json` for the
soak-TTL warning. Surface `vault_layout`, `days_in_both`, and
recommendation state in `mnemo_vault_status`.

### 🎯T64.3 Move decisions + memories under _mnemo/

- **Status**: identified
- **Weight**: 2

Re-render the two existing v1 abstractions (decisions, memories)
under `_mnemo/decisions/` and `_mnemo/memories/`. Above-fence
content gets the new frontmatter shape per the design's page
shape. Old v1 decision/memory files still written when
`vault_layout="both"`. Validates the rendering shape, frontmatter,
graph topology, and atomic write protocol before more abstractions
land.

### 🎯T64.4 Drop session/CI/PR/repo writers from _mnemo/

- **Status**: identified
- **Weight**: 1

When `vault_layout="v2"`, do not write session, CI, PR, or
repo-index pages to `_mnemo/`. v1 paths continue under
`vault_layout="both"` or `"v1"`. Validates the principle that raw
signal stays in SQLite and is queried on demand. Marks the **MVP
v2 boundary** — the wing is now calm, scoped, and
consent-correct.

### 🎯T64.5 PKM profile + auto-detect (mtime)

- **Status**: identified
- **Weight**: 2

Introduce `vault_profile` config key (`"obsidian"` | `"logseq"` |
`"foam"` | `"generic"`), per-profile renderer hooks, and
auto-detect logic. Detection uses **mtime of the canonical signal
files** (`.obsidian/workspace.json`, `logseq/config/config.edn`,
`.foam/settings.json`), not directory presence — addresses
multi-tool PKM users (Obsidian + Logseq on the same directory).
Record detected profile + signal file + mtime in
`mnemo_vault_status`.

### 🎯T64.6 Bridges

- **Status**: identified
- **Weight**: 2

`vault_bridges` config key mapping collection name → anchor-file
path. Whole-file atomic write of bridge fences
(`<!-- mnemo:bridge:<name> -->` … `<!-- /mnemo:bridge:<name> -->`)
into named anchor files. Fail-soft on unknown collection names,
duplicate fences, unwritable anchors, missing anchors (created on
demand), bridge removal (strip block, leave file). Per-collection
default renderings; templates can override per
`bridge_<collection>.tmpl`. New MCP tool: `mnemo_vault_bridge_list`.

### 🎯T64.7 Patterns (persisted + rendered)

- **Status**: identified
- **Weight**: 3

Promote `mnemo_discover_patterns` from live-queried to persisted.
New `patterns` + `patterns_fts` tables (additive, append-only per
T49 schema policy). Renderer emits `_mnemo/patterns/` pages for
patterns meeting `occurrence ≥ 3` AND `session_count ≥ 2`. First
new abstraction; cheapest to extract; prerequisite for T64.8
(patterns are one of four clustering inputs).

### 🎯T64.8 Themes + cross-repo (clustering engine)

- **Status**: identified
- **Weight**: 5

Two-engine clustering pipeline per `docs/design/vault-clustering.md`.
Heuristic engine (TF-IDF + single-link agglomerative, fully local)
is the default. Embeddings engine (Voyage AI vectors + same
single-link downstream) is opt-in per T63 egress posture; HDBSCAN
deferred until a pure-Go binding meets the correctness bar.

New tables: `themes`, `theme_members`, `cluster_embeddings`,
`themes_fts`, `cluster_runs`, `theme_overrides`, `theme_pins`.
Cross-repo pages derived from themes with `len(repos) ≥ 2` AND
`weight ≥ 4.0`. Label quality gates (centroid-closest, ≥200-token
body, daily-note filename exclusion, title-content coherence).
Theme retirement at `retire_after` (180 days default); pin via
`mnemo_vault_themes_pin`. Two-key egress matrix
(embeddings + LLM labelling independent opt-ins). New MCP tools:
`mnemo_vault_recluster` (with `force_reembed`),
`mnemo_vault_themes_inspect`, `mnemo_vault_themes_pin`. Stubs for
`themes_split` / `themes_merge`.

The research-shaped slice; carries the most risk. Subordinate
design document is the canonical spec.

### 🎯T64.9 Lessons + GC tool

- **Status**: identified
- **Weight**: 3

Lessons extractor (feedback-type auto-memories + confirmed
decisions surviving ≥ 2 weeks; clustered via the same engine as
T64.8). New MCP tools: `mnemo_vault_gc_legacy` (user-initiated
deletion of v1 root-level dirs after annotation-safety check),
`mnemo_vault_migration_doc` (opt-in regen of write-once
MIGRATION.md). `_mnemo/legacy-annotations.md` harvester with
`max_file_kb` pagination. Completes the abstraction set and gives
existing v1-vault users a clean off-ramp.
