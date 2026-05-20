# Vault Clustering Engine

*Status: design draft — 2026-05-20.*
*Subordinate to `docs/design/vault-library-wing.md` (Slice 8).*
*Prerequisite: Slice 7 (patterns persisted) must land first.*

---

## TL;DR

The library wing's most valuable abstractions — themes, cross-repo
similarities, lessons — all rely on grouping content by topical
similarity. This document specifies how that grouping is computed,
stored, kept fresh, and rendered. Two engines are specified: a
heuristic default that runs entirely locally, and an opt-in
embeddings engine that calls a hosted provider. Per T63's egress
posture (PR #92), the embeddings engine is **off by default** —
users opt in explicitly, API key presence alone is not enough.

The engine treats four input sources as equal first-class signal:
decision summaries, compaction span summaries, persisted patterns
(Slice 7), and — under permitted indexing scopes — user-authored
vault content. Output is a `themes` table of cluster rows plus a
`theme_members` table joining clusters to source entities. Themes
above a configurable weight threshold are materialised as
`_mnemo/themes/<slug>.md` pages; themes spanning ≥ 2 repos are
additionally surfaced as `_mnemo/cross-repo/<slug>.md` views.

Clustering runs on a long cadence (default 24h) because the
output is noisy on short windows and the cost is non-trivial. A
content-hash cache means recomputation only touches entities whose
text changed.

---

## Goals

1. **Stable, human-legible clusters.** Cluster labels do not drift
   on every pass. A theme that was meaningful yesterday is still
   recognisable today.
2. **Default works fully offline.** The heuristic engine is the
   default and ships first. It produces serviceable themes for every
   user without any opt-in or external dependency. The embeddings
   engine improves on it for users who opt in; the heuristic engine
   is not a toy.
3. **Bounded cost.** Single-digit cents per re-cluster for a
   median user. Order-of-magnitude predictable for any user.
4. **User content as ground truth.** When `vault_indexing_scope`
   is `"full"` or `"includes"`, user-authored notes anchor and
   label clusters rather than competing with auto-extracted text.
5. **Transparent.** A user can inspect cluster membership, see why
   a session was grouped a particular way, and flag misclassifications.

---

## Non-goals

- **Real-time clustering.** Themes update on a long cadence.
  Sub-minute freshness is out of scope.
- **Cross-user clustering.** Each daemon clusters its own user's
  corpus. Team-mnemo (T36) governs any multi-user case.
- **Topic modelling as a service.** Mnemo does not expose a generic
  clustering API. The engine serves the vault renderer; it is not
  a standalone tool surface.
- **Hierarchical themes.** Themes are flat. A future iteration may
  add parent/child relationships if cluster labels prove they
  warrant the structure.
- **Replacing existing search.** FTS5 over message bodies remains
  the search backbone. Clustering produces grouping, not retrieval.

---

## Inputs

The engine consumes four signal types. Each contributes "documents"
to a unified corpus from which clusters are derived.

### 1. Decision summaries

Source: existing `decisions` table. Use the `summary` column where
present; fall back to the concatenation of `proposal` + `confirmation`
texts trimmed to a representative window (first 500 tokens).

Filtering: high-signal decisions only — confirmed (not just proposed),
≥ 1-paragraph rationale, ≥ 1 week since first observation. Same
filter applied by Slice 9 lessons extractor.

Weight: 1.0 (baseline reference).

### 2. Compaction span summaries

Source: existing `compactions` table. Use the `prose_summary` column.

Filtering: spans with non-empty `targets_active` or
`targets_progressed` — i.e. spans where actual work was tracked. Empty
spans (a single message followed by `/clear`) contribute noise without
signal and are excluded.

Weight: 0.8. Compaction summaries are LLM-distilled and slightly
less reliable than decision summaries, which sit closer to source
material.

### 3. Patterns

Source: `patterns` table (introduced in Slice 7).

Filtering: patterns with occurrence ≥ 3 across ≥ 2 sessions. Single-
occurrence patterns do not yet warrant cross-cluster placement.

Weight: 1.2. Patterns are explicit recurrences, the highest-signal
input.

### 4. User-authored vault content

Source: `docs` table where `kind = 'vault'` and the entity was
ingested via `IngestVaultAnnotations` from a path outside `_mnemo/`.
Only present when `vault_indexing_scope` is `"full"` or `"includes"`.

Filtering: notes with ≥ 100 tokens of content (excluding frontmatter).
Below-fence annotations on mnemo-generated pages are excluded from
this stream — they are attributed to their parent entity, not treated
as standalone documents.

Weight: 1.5. User-authored content is the strongest available signal
of human-meaningful structure and serves as anchor labels.

### Stream merge

All four streams are merged into a `corpus` view at clustering time:

```
corpus(doc_id, kind, entity_id, repo, text, ts, weight)
  kind ∈ {decision, compaction, pattern, vault_user}
```

`doc_id` is `<kind>:<entity_id>`. `repo` is normalised to the
short-project-name form already used in v1.

---

## Outbound API posture

The embeddings engine makes outbound HTTP calls to a hosted embedding
provider (Voyage AI, OpenAI, etc.). Per the precedent set by **T63
(PR #92, cost reconciliation opt-in)**, every mnemo feature that
performs unsolicited outbound traffic to hosted APIs defaults **off**
and requires explicit opt-in via config — even when the relevant API
key is present in the environment for other reasons.

This rule applies to the embeddings engine. Specifically:

- `vault_clustering.engine` defaults to `"heuristic"`. The heuristic
  engine has no network dependencies.
- A user opts into the embeddings engine by writing
  `vault_clustering.engine: "embeddings"` (live via `mnemo_config`
  or by editing `~/.mnemo/config.json`). Mere presence of a Voyage
  API key in the environment is not enough.
- If the user sets `engine: "embeddings"` but no provider key is
  configured, the daemon logs a warning, falls back to the heuristic
  engine for that pass, and reports the degraded state via
  `mnemo_vault_status`.
- The opt-in posture is documented in CLAUDE.md alongside the
  other egress features (GitHub backfill, federation, cost
  reconciliation) when this slice ships.

This deliberately gives a worse default to new users than the
embeddings engine would. Justification: the heuristic engine is
explicitly designed to be non-toy (see Engine B below); the user can
opt in to better quality with a one-line config change; respecting
egress posture matters more than maximising default theme quality.

## Engine A — embeddings (opt-in)

### Vectorisation

For each `corpus` row, compute a 1024-dimensional embedding via the
configured provider. Default provider: Voyage AI `voyage-3-lite`
(cheap, multilingual, 1024d output). Alternative: OpenAI
`text-embedding-3-small` (1536d, truncated to 1024 via PCA). Provider
chosen by `vault_clustering.embedding_provider`; falls back to
heuristic engine if no key is set.

Embeddings cached in a new `cluster_embeddings` table keyed by
`(kind, entity_id, content_hash)`. Recompute only when the
`content_hash` changes. Median user has on the order of 10³ docs;
re-embedding cost is single-digit cents at provider rates.

### Cluster algorithm

HDBSCAN over the embedding matrix. Parameters:

| Parameter              | Default | Rationale                                    |
|------------------------|---------|----------------------------------------------|
| `min_cluster_size`     | 3       | matches `min_cluster_weight` config          |
| `min_samples`          | 2       | tolerant of slightly noisy clusters          |
| `cluster_selection_epsilon` | 0.0 | strict — prefer many small clusters over megaclusters |
| `metric`               | cosine  | standard for text embeddings                 |

HDBSCAN chosen over k-means because:
- No fixed cluster count assumption (the corpus has unknown topic
  count).
- Tolerates noise points (decisions/spans that don't belong in any
  theme).
- Density-based, so cluster labels are stable across runs given
  stable inputs.

### Cluster labelling

Within each cluster, identify representative documents by inverse
distance to cluster centroid. Take the top-5.

Then:

1. **If the cluster contains at least one `vault_user` document**
   (scope `"full"` or `"includes"` enabled, user wrote a thematic
   note that landed in this cluster), use the user document's title
   as the cluster label. This is the ground-truth path: the user
   wrote "Auth middleware redesign" once and now owns that label.

2. **Otherwise**, generate a label via Claude Haiku 4.5 with the
   prompt: "These are excerpts grouped by topical similarity. Label
   the topic in ≤6 words, all lowercase, no punctuation:" followed
   by the top-5 representative excerpts. Cap at 6 words, lowercase,
   kebab-case for slug derivation.

3. **If LLM labelling fails** (no API key, rate limit, etc.), fall
   back to the most-frequent non-stopword bigram across the cluster's
   top-5 excerpts. Crude but deterministic.

### Stability

Cluster IDs are generated as `theme_<sha1(sorted_member_doc_ids)[:8]>`.
This means: if the same set of documents clusters together on the next
pass, the ID is identical. A document joining/leaving changes the ID,
which is the right semantic (a different set is a different theme).

Renamed themes (same membership, new label) retain the same ID, so
`_mnemo/themes/<slug>.md` paths churn only when membership churns.

---

## Engine B — heuristic (default)

Same input streams. No embeddings. No LLM labelling.

### Document representation

For each document, compute the term-frequency vector over its content
after:
- Lowercase, strip punctuation.
- Tokenize on whitespace.
- Drop stopwords (standard English list + project-specific noise:
  "session", "tool_use", "ok", "thanks", code-fence markers).
- Stem via Porter stemmer.

### Similarity

Cosine similarity over TF-IDF vectors. IDF computed corpus-wide; recomputed at each clustering pass.

### Cluster algorithm

Single-link agglomerative clustering with threshold `similarity ≥ 0.35`
(tuneable via `vault_clustering.heuristic_threshold`). Stop when no
pair exceeds the threshold.

This produces fewer, broader clusters than HDBSCAN — acceptable for
the fallback. Documented limitation.

### Labelling

Most-frequent non-stopword bigram across the cluster's documents,
weighted by document weight. Capitalise as title case for the page
header. Slug from the same bigram, kebab-case.

If no bigram repeats, fall back to most-frequent single non-stopword
token.

### When the heuristic engine wins

- Zero external dependencies. Clustering works on an offline machine.
- Deterministic. Same input → same output. Useful for tests.
- Cheap. Median cluster pass is sub-second for 10³ documents.

### When it loses

- Synonym blindness. "Schema migration" and "DB migration" cluster
  separately. Embeddings would merge them.
- Domain drift. A repo with heavy jargon dominates corpus IDF and
  warps clusters. Embeddings are more robust.
- Label quality. Bigrams are choppier than LLM labels.

---

## Output

### Tables (additive, append-only per T49 schema policy)

All DDL below lands as additions to `internal/store/schema.sql` (the
single source of truth post-T49, PR #82). sqlift's `AllowNone` gate
admits the changes because each is a pure `CREATE TABLE` /
`CREATE VIRTUAL TABLE`. No constraint tightening on existing tables;
no drops; no type changes. Pre-migration backup (T61 Phase 1)
automatically captures the prior state.

```sql
CREATE TABLE themes (
  id              TEXT PRIMARY KEY,            -- theme_<sha1[:8]>
  label           TEXT NOT NULL,
  slug            TEXT NOT NULL UNIQUE,
  source_engine   TEXT NOT NULL,               -- "embeddings" | "heuristic"
  weight          REAL NOT NULL,               -- sum of member doc weights
  member_count    INTEGER NOT NULL,
  repos           TEXT NOT NULL,               -- JSON array of repo names
  first_seen      TEXT NOT NULL,               -- ISO timestamp
  last_computed   TEXT NOT NULL,
  centroid_text   TEXT                         -- representative excerpt
);

CREATE TABLE theme_members (
  theme_id        TEXT NOT NULL REFERENCES themes(id),
  doc_kind        TEXT NOT NULL,               -- decision | compaction | pattern | vault_user
  entity_id       TEXT NOT NULL,
  repo            TEXT,
  ts              TEXT,
  distance        REAL,                        -- distance to centroid; for representative selection
  PRIMARY KEY (theme_id, doc_kind, entity_id)
);

CREATE TABLE cluster_embeddings (
  doc_kind        TEXT NOT NULL,
  entity_id       TEXT NOT NULL,
  content_hash    TEXT NOT NULL,
  provider        TEXT NOT NULL,
  vector          BLOB NOT NULL,               -- float32 packed
  computed_at     TEXT NOT NULL,
  PRIMARY KEY (doc_kind, entity_id, content_hash)
);

CREATE VIRTUAL TABLE themes_fts USING fts5(
  label, slug, centroid_text,
  content='themes', content_rowid='rowid'
);
```

All four are append-only (no destructive drops, no constraint
tightening). `themes` and `theme_members` are rewritten in full each
clustering pass; `cluster_embeddings` accumulates and is GC'd by a
separate pass when the underlying entity disappears.

### Renderer

`internal/vault/themes.go` (new) emits one Markdown file per row in
`themes` where `weight ≥ vault_clustering.min_cluster_weight`. Page
shape per the parent design:

```markdown
---
type: theme
tags: [mnemo, mnemo/theme]
aliases: ["{{ .Label }}"]
weight: {{ .Weight }}
first-seen: {{ .FirstSeen | date "2006-01-02" }}
last-touched: {{ .LastTouched | date "2006-01-02" }}
repos: {{ .Repos | toJSON }}
---

# {{ .Label }}

<!-- mnemo:generated {{ now }} -->

## Summary
{{ .CentroidText }}

## Evidence
{{ range .RepresentativeExcerpts }}
- {{ .Date }} · {{ .Repo }} · "{{ .Excerpt }}"
{{ end }}

## Related
{{ range .RelatedThemes }}- [[_mnemo/themes/{{ .Slug }}|{{ .Label }}]]
{{ end }}

## Underlying entities
{{ range .Members }}- {{ .Kind }} · {{ .Repo }} · {{ .EntityID }}
{{ end }}

<!-- /mnemo:generated -->

<!-- Your notes below this line — preserved across syncs. -->
```

### Cross-repo derivation

A theme with `len(JSON(repos)) ≥ 2` AND `weight ≥ 4.0` triggers
emission of a cross-repo page at `_mnemo/cross-repo/<slug>.md` in
addition to the theme page. The cross-repo page renders the same
theme through a different lens: groups evidence by repo, surfaces
which-repos-first metadata, and is the page bridged into multi-repo
MOCs.

No separate `cross_repos` table — cross-repo pages are a derived
view over `themes`.

---

## Cadence and triggering

### Default

`vault_clustering.recompute_interval` = `"24h"` (configured in parent
design).

Background goroutine inside `vault.Exporter` (or a sibling
`vault.Clusterer`) ticks at the interval and runs a full clustering
pass. Pass duration for a median user is on the order of seconds; for
heavy users it should remain under one minute.

### Worker lifecycle

The clustering worker follows the **T61 backup worker pattern**
(`internal/backup/worker.go`, PR #87):

- Registered under the user's WaitGroup at `Registry.ForUser` time
  via a dedicated `startClusterWorker` helper analogous to
  `startBackupWorker`.
- Inherits **T62 eager-start** (PR #89): the worker spins up at
  daemon boot for the default user, not lazily on first MCP call.
  A passive daemon with no Claude session attached still re-clusters
  on schedule.
- Hot-reload via `mnemo_config`: changes to `vault_clustering.engine`,
  `recompute_interval`, or `min_cluster_weight` stop the existing
  goroutine and restart with the new settings. Per-user `clusterCancel`
  tracked on the `userEntry` struct alongside `vaultCancel` and
  `reconcilerCancel`.
- Quiescence not required (clustering reads only, does not write to
  contended tables in the hot ingest path). Unlike the backup worker
  it can run at any time.
- Misconfiguration (bad interval, unknown engine) logs a warn and
  skips the worker. Daemon never blocks on cluster setup failures.

### Triggered by

- Time tick at the configured interval.
- Manual `mnemo_vault_recluster` MCP tool call (immediate run).
- Significant corpus change (≥ N new high-signal entities since last
  pass; default N=50) — opportunistic re-cluster outside the timer.

### Concurrency

Clustering and vault sync share the existing `syncMu` discipline from
v1: a cluster pass takes a separate `clusterMu`, periodic syncs run
during/around it without conflict. The renderer reads cluster output
via a snapshot taken under `clusterMu`, so a sync writing themes
sees a consistent view.

### Cost telemetry

Each pass records to a new `cluster_runs` table:
- start/end timestamps
- engine used
- input doc count
- output theme count
- embedding API calls and approximate cost
- failure mode if any

Surfaced via `mnemo_vault_status` and queryable directly. Lets a
user spot cost outliers without enabling DEBUG logging.

---

## User-facing controls

### Configuration keys (additions to parent design)

```json
{
  "vault_clustering": {
    "engine": "heuristic",
    "embedding_provider": "voyage",
    "min_cluster_weight": 3,
    "recompute_interval": "24h",
    "heuristic_threshold": 0.35,
    "max_themes": 200,
    "max_cross_repo": 50
  }
}
```

| Key                                | Default      | Hot-reload? |
|------------------------------------|--------------|-------------|
| `vault_clustering.engine`          | `"heuristic"` (explicit opt-in to `"embeddings"`) | yes |
| `vault_clustering.embedding_provider` | `"voyage"` | yes         |
| `vault_clustering.min_cluster_weight` | `3`       | yes         |
| `vault_clustering.recompute_interval` | `"24h"`   | yes         |
| `vault_clustering.heuristic_threshold` | `0.35`   | yes         |
| `vault_clustering.max_themes`      | `200`        | yes         |
| `vault_clustering.max_cross_repo`  | `50`         | yes         |

`max_themes` caps page emission to prevent a vault explosion when
clustering accidentally produces 5000 micro-clusters. Themes ranked
by weight; excess kept in the table but not rendered.

### MCP tools

- **`mnemo_vault_recluster`** — trigger an immediate clustering pass.
  Returns the run report from `cluster_runs`. Parameters: `engine`
  optional override.
- **`mnemo_vault_themes_inspect`** — given a theme slug or ID,
  return the full member list with distances, the centroid text,
  the labelling source (user-anchored vs LLM vs bigram), and the
  related-theme links. Lets a user understand why a cluster looks
  the way it does.
- **`mnemo_vault_themes_split`** — manual override: mark a theme
  ID for splitting on the next pass. Persisted in a
  `theme_overrides` table. Next clustering pass applies the
  split-hint and reports outcomes. Inverse: `mnemo_vault_themes_merge`.

The split/merge MCP tools are not in Slice 8's MVP — they are a
follow-up after real-world cluster quality data is in hand.

### Read paths

- `mnemo_query` against `themes`, `theme_members`, `themes_fts` for
  power users.
- `_mnemo/themes/_index.md` lists all themes by weight, with
  one-line summaries.
- `mnemo_search` returns theme hits as a distinct result type
  alongside session excerpts.

---

## Failure modes and mitigations

### Mode 1 — singleton oversplitting

Same topic appears as multiple themes ("schema migration", "DB
migration", "database migration"). Symptom: `_mnemo/themes/_index.md`
has near-duplicate entries.

**Mitigation:** raise `min_cluster_size` from 3 to 5 in HDBSCAN. Or
post-process: pairwise theme similarity (cosine over centroids); merge
any pair with similarity > 0.9 in a deterministic second pass.
Documented as a tunable.

### Mode 2 — megacluster absorption

One huge cluster covers most of the corpus ("everything about Go"),
the rest are tiny. Symptom: weight distribution is bimodal with one
dominant cluster.

**Mitigation:** strict `cluster_selection_epsilon` (already 0.0).
Stopword list extended with overused project tokens. If still
occurring, partition the corpus by repo before clustering and merge
themes post-hoc.

### Mode 3 — drift in labels

A theme's auto-label changes from "auth middleware redesign" to
"session token storage" between passes despite identical membership.
Symptom: `_mnemo/themes/<slug>.md` paths churn.

**Mitigation:** label is cached per theme ID. Recompute only when
membership changes. User-anchored labels (from vault_user docs) are
sticky regardless of pass.

### Mode 4 — embedding provider outage

Embedding API down for hours. Symptom: clustering pass fails or
stalls.

**Mitigation:** pass fails gracefully, logs error, leaves prior
`themes` snapshot in place. User sees a "last computed N hours ago"
banner in `_mnemo/themes/_index.md`. Next successful pass updates.

### Mode 5 — vault content dominates spurious clusters

User has a sprawling vault with one large note tangentially mentioning
many topics. That note ends up in every cluster as a representative,
making themes look incoherent.

**Mitigation:** per-document weight cap. A single document contributes
at most weight 2.0 to any cluster regardless of configured stream
weight. A user note in three clusters thus splits its influence.

### Mode 6 — cluster IDs churn from minor edits

A user edits one decision summary slightly; its embedding shifts
marginally; the cluster it belongs to is now a different membership
set; the cluster ID changes; the theme page moves.

**Mitigation:** ID derivation uses the *sorted member set*, not the
embeddings themselves. A minor text edit doesn't change membership;
only addition/removal does. Edits to non-member documents don't
affect ID.

---

## Open questions

### Q1: How to surface user feedback into the clustering loop?

If a user reads `_mnemo/themes/auth-redesign.md` and writes below the
fence "this is actually two separate things — split into auth and
session-mgmt", how does that propagate?

Options:
- **Manual MCP tool only** (current proposal): user invokes
  `mnemo_vault_themes_split` with theme ID.
- **Below-fence directive parsing**: a `mnemo: split` frontmatter
  key or `<!-- mnemo:split -->` directive in user content is parsed
  on next ingest. Lower friction but introduces a new contract.

**Working answer:** manual tool in Slice 8 MVP. Directive parsing in
follow-up if friction is high.

### Q2: Should themes have lifecycle states (proposed/active/retired)?

Today a theme either exists (membership ≥ threshold) or doesn't.
But topics fade — a theme that was active six months ago may now be
historical context, not current work.

Options:
- **Implicit retirement**: if no member has `ts` newer than 90 days,
  theme moves to `_mnemo/themes/_archive/` automatically. Listed in
  `_index.md` under "Past themes".
- **Persistent indefinitely**: themes live forever; the user prunes
  via tooling.

**Working answer:** implicit retirement at 180 days; configurable
via `vault_clustering.retire_after`. The user can pin themes via
`mnemo_vault_themes_pin` to override.

### Q3: Patterns-only clusters versus theme membership for patterns

A pattern that recurs across 5 sessions in 3 repos: is it a
standalone `_mnemo/patterns/<slug>.md` (Slice 7), a member of a
broader theme (Slice 8), or both?

**Working answer:** both. The pattern lives in `patterns/`,
independently rendered. The theme containing it cites it under
"Related" with a wikilink. Two views of the same underlying signal.

### Q4: Translation / multilingual corpus

A non-English user (or a polyglot one) generates content in multiple
languages. Voyage AI's `voyage-3-lite` is multilingual; HDBSCAN over
cosine distance is language-agnostic; LLM labelling via Haiku is
multilingual. Heuristic engine's Porter stemmer is English-only.

**Working answer:** embeddings engine handles multilingual natively.
Heuristic engine is English-best-effort; document the limitation.
Multilingual heuristic stemming is a follow-up.

---

## Acceptance criteria

The clustering engine is considered shipped when:

1. Both engines (embeddings and heuristic) produce valid `themes`
   and `theme_members` rows for a test corpus.
2. Default engine is `"heuristic"` regardless of API key presence.
   The embeddings engine activates only on explicit
   `vault_clustering.engine: "embeddings"` config (per T63 egress
   posture).
3. Themes are rendered as `_mnemo/themes/<slug>.md` pages with the
   parent design's frontmatter and fence contract.
4. Cross-repo derivation emits `_mnemo/cross-repo/<slug>.md` for
   themes with `len(repos) ≥ 2` and `weight ≥ 4.0`.
5. Cluster IDs are stable across runs given stable membership.
6. `cluster_embeddings` cache prevents re-embedding unchanged
   content.
7. `cluster_runs` telemetry is queryable.
8. `mnemo_vault_recluster` and `mnemo_vault_themes_inspect` MCP
   tools work end-to-end.
9. User-anchored labels (vault_user documents) win over LLM
   labels when present.
10. Embedding provider outage degrades gracefully — last snapshot
    preserved.

Split/merge tools (Q1) and lifecycle states (Q2) ship in a
follow-up slice.

---

## Implementation outline

Roughly in this order. Each is reviewable independently.

1. **Schema** — add `themes`, `theme_members`, `cluster_embeddings`,
   `themes_fts`, `cluster_runs` to `internal/store/schema.sql` (single
   source of truth per T49, PR #82). sqlift `AllowNone` gate admits
   the pure additions on next daemon start. Pre-migration backup
   (T61 Phase 1) snapshots the prior state.
2. **Corpus assembly** — `internal/store/cluster_corpus.go` produces
   the `corpus` view from decisions/compactions/patterns/vault_user.
   `vault_user` rows sourced from `docs` filtered by the active
   `trees_of_interest` (T53, PR #91) for the user's vault scope.
3. **Heuristic engine** — `internal/cluster/heuristic.go`. TF-IDF +
   single-link. Deterministic. Self-tested with golden fixtures.
   **This is the default engine** — ships first, lands the slice on
   its own without any opt-in surface.
4. **Worker registration** — `internal/registry/registry.go` gains
   `startClusterWorker` modelled on `startBackupWorker` (T61, PR #87).
   Wired through `Registry.ForUser` so it benefits from T62's
   eager-start (PR #89). `clusterCancel` tracked per-user for
   hot-reload via `mnemo_config`.
5. **Embeddings engine (opt-in)** — `internal/cluster/embeddings.go`.
   Voyage client, HDBSCAN binding (port from `gonum`-compatible
   implementation), embedding cache. Documented in CLAUDE.md
   alongside other egress features per T63's posture (PR #92).
6. **Labelling** — `internal/cluster/label.go`. User-anchor → LLM →
   bigram fallback chain.
7. **Renderer** — `internal/vault/themes.go`,
   `internal/vault/cross_repo.go`. Hooks into existing `Exporter.Sync`
   loop.
8. **MCP tools** — `mnemo_vault_recluster`, `mnemo_vault_themes_inspect`.
   Wired in `internal/tools/tools.go`.
9. **Telemetry** — `cluster_runs` writes from both engines; reporting in
   `mnemo_vault_status`.
10. **Tests** — golden corpus + expected clusters for both engines;
    stability test (same input → same IDs); embedding cache hit/miss
    tests; user-anchor labelling tests; opt-in posture test
    (embeddings stays off without explicit config).

Estimated effort: 2–3 weeks of focused work. The HDBSCAN port is the
single largest sub-task; if a pure-Go HDBSCAN library that meets the
schema/test bar is not available, fall back to a hierarchical
clustering alternative (slightly worse quality, simpler to implement
from scratch).

---

## Appendix: why not delegate everything to an LLM?

A tempting shortcut: feed the entire corpus to an LLM and ask "group
these into themes and label them." Tested mentally; rejected for
these reasons:

- **Cost scales linearly with corpus.** A median user with 10³
  documents is several MB of context — multiple LLM calls per pass
  at non-trivial cost. Embedding-based clustering is O(corpus) once,
  not O(corpus × passes).
- **No stability.** LLM clustering output is not deterministic.
  Theme IDs would churn pass-to-pass even with identical input.
- **No bounded recall.** The LLM may forget documents in long
  context. Embedding clustering processes every document by
  construction.
- **Failure modes are opaque.** When LLM clustering produces a
  weird grouping, you cannot inspect why. Embeddings give you a
  centroid distance for every member.

LLM use is confined to the labelling step, where it operates on a
5-excerpt window and the failure is bounded.

---

## Appendix: relationship to existing infrastructure

- **PR #74** (vault export v1) — shares the fence contract and
  `IngestVaultAnnotations` ingest path. `vault_user` documents in the
  clustering corpus are read through the same code path.
- **PR #82 / T49** (sqlift additive-only schema) — every table in
  this design lands as a pure addition to `internal/store/schema.sql`.
  No constraint changes, no drops. sqlift's `AllowNone` gate is the
  enforcement mechanism.
- **PR #91 / T53** (trees_of_interest + doc_tree_refs) — `vault_user`
  documents in the corpus are sourced from `docs` rows linked to the
  trees registered by the parent design's Slice 4. Removing a tree
  (e.g. dropping an entry from `vault_indexing_includes`) flows
  naturally through to the corpus on the next clustering pass.
- **PR #86 / #87 / T61** (backup primitive + periodic backup worker)
  — clustering tables ride mnemo.db's standard snapshot. The
  `cluster_embeddings` table is the largest of the new additions;
  monitor its contribution to snapshot size in `cluster_runs`
  telemetry. Pre-migration backup automatically captures pre-clustering
  state when the schema additions land.
- **PR #89 / T62** (eager-start default user workers) — the
  clustering worker runs at boot for the default user. No first-MCP-
  call latency.
- **PR #92 / T63** (cost reconciliation opt-in) — the precedent for
  this design's "Outbound API posture" section. The embeddings engine
  is off by default; opt-in is explicit; the heuristic engine is
  non-toy. Documentation update to CLAUDE.md ships with the slice.
- **`docs/design/vault-library-wing.md`** (parent design) — Slice 8
  is governed by this subordinate doc. Acceptance criterion 3 of the
  parent design (themes/patterns/cross-repo/lessons/decisions/memories
  all materialised) depends on this engine.
