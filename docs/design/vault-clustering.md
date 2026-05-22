# Vault Clustering Engine

*Status: design draft — 2026-05-20 (rev 2026-05-22, review pass 1).*
*Subordinate to `docs/design/vault-library-wing.md` (Slice 8).*
*Prerequisite: Slice 7 (patterns persisted) must land first.*

---

## TL;DR

The library wing's most valuable abstractions — themes, cross-repo
similarities, lessons — all rely on grouping content by topical
similarity. This document specifies how that grouping is computed,
stored, kept fresh, and rendered.

**Two engines are specified.** Engine B (heuristic, TF-IDF +
single-link agglomerative, fully local) is the **realistic default**
that ships first and is on by default. Engine A (hosted-provider
embeddings + single-link agglomerative) is **opt-in** per T63 egress
posture; it swaps the TF-IDF vectorisation step for hosted-provider
embeddings but reuses the rest of Engine B's pipeline. HDBSCAN is
the algorithmically right choice for Engine A's downstream clustering
but is **deferred**: no pure-Go HDBSCAN library currently meets the
correctness bar, and porting one is non-trivial. Configuration keys
for the HDBSCAN path are reserved (`vault_clustering.hdbscan.*`)
and the swap can happen in a follow-up slice without breaking
config. This keeps Slice 8 a *shippable* slice.

Per T63's egress posture (PR #92), the embeddings engine is **off by
default** — users opt in explicitly, API key presence alone is not
enough.

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
   engine improves on it specifically by coalescing synonyms and
   handling vocabulary drift better; downstream of vectorisation
   both engines run the same single-link agglomerative clustering,
   so the embeddings engine is *not* a wholesale algorithmic
   upgrade. The heuristic engine is not a toy.
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

**Two thresholds, on purpose.** Corpus admission for `vault_user`
documents is `≥ 100` tokens (this filter). Label-anchor eligibility
is `≥ 200` tokens (`vault_clustering.label.user_min_tokens`; see
"Cluster labelling"). The two are deliberately different: a
shorter note carries enough signal to *belong* to a cluster but
not enough to *name* it. Both thresholds are configurable; they
should be moved together if changed.

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

The clustering pipeline has **two independent egress surfaces** to a
hosted API, each governed by its own opt-in per T63 (PR #92, cost
reconciliation precedent):

1. **Embeddings.** Compute float vectors via a hosted embedding
   provider (Voyage AI, etc.). Required for Engine A; not used by
   Engine B.
2. **LLM labelling.** Generate human-readable cluster labels via
   Claude Haiku 4.5. Optional in both engines; falls back to the
   bigram heuristic when off.

A user can opt into either, both, or neither. The matrix:

| Embeddings | Labelling | Behaviour                                          |
|------------|-----------|----------------------------------------------------|
| off        | off       | Engine B end-to-end, bigram labels. No external calls. |
| off        | on        | Engine B clustering, LLM labelling for cluster names. One outbound surface. |
| on         | off       | Engine A clustering with Voyage embeddings, bigram labels. One outbound surface. |
| on         | on        | Engine A + LLM labels. Two outbound surfaces.       |

Each surface defaults off and requires its own explicit
configuration:

- **Embeddings opt-in.** `vault_clustering.engine: "embeddings"`
  (default `"heuristic"`). Mere presence of a Voyage API key in
  the environment is not enough.
- **Labelling opt-in.** `vault_clustering.label.engine: "llm"`
  (default `"bigram"`). Mere presence of an Anthropic API key is
  not enough.

If a user sets either to on but the corresponding API key is
absent, the daemon logs a warning, falls back to the off behaviour
for that pass, and reports the degraded state in
`mnemo_vault_status.warnings[]`. The pass does not block.

The CLAUDE.md egress section lists both surfaces alongside GitHub
backfill, federation, and cost reconciliation when this slice
ships.

The defaults are deliberately worse than what hosted APIs could
offer. Justification: the heuristic engine + bigram labelling is
explicitly designed to be non-toy (see Engine B); a one-line
config change is the user's deliberate ask; respecting egress
posture matters more than maximising default theme quality.

## Engine A — embeddings (opt-in)

### Status

Engine A is **opt-in** (per T63 egress posture) and **partially
aspirational**. The provider interface, embedding cache, model-version
pinning, and labelling pipeline are committed. The HDBSCAN
sub-component is not — see "Cluster algorithm" below for the
shipped-as-of-Slice-8 behaviour and the deferred HDBSCAN follow-up.

### Vectorisation

For each `corpus` row, compute a 1024-dimensional embedding via the
configured provider.

The provider interface is **provider-agnostic from day one**, even
though Voyage AI is the only concrete provider that ships with Slice
8. The interface lives in `internal/cluster/embedding_provider.go`:

```go
type EmbeddingProvider interface {
    // Name returned in cluster_embeddings.provider.
    Name() string
    // Model returned in cluster_embeddings.model.
    Model() string
    // ModelVersion returned in cluster_embeddings.model_version.
    // Empty string if the provider does not version models
    // (callers should treat the empty value as a stable tag).
    ModelVersion() string
    // Dimensions returned in cluster_embeddings.dimensions.
    Dimensions() int
    // Embed produces dense float32 vectors for each input string.
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

Concrete provider for Slice 8: `internal/cluster/voyage.go`
implementing `EmbeddingProvider` against Voyage AI's
`voyage-3-lite`. The provider is selected by
`vault_clustering.embedding_provider` (default `"voyage"`). OpenAI,
Cohere, local embedding servers, etc. are out of scope for Slice 8
but cost nothing to support later because the interface already
admits them.

Per the egress posture, the embeddings engine falls back to the
heuristic engine when:
- `vault_clustering.engine` is not `"embeddings"`, or
- `vault_clustering.engine` is `"embeddings"` but the configured
  provider returns an auth error / has no API key.

Embeddings cached in a new `cluster_embeddings` table keyed by
`(doc_kind, entity_id, content_hash, provider, model, model_version)`.
Recompute only when any of those change. Median user has on the
order of 10³ docs; re-embedding cost is single-digit cents at
provider rates.

#### Model-version pinning and drift

A user who switches from `voyage-3-lite` to `voyage-3`, or whose
provider versions a model in place, must not silently corrupt the
existing cluster set. The cache key includes `provider`, `model`,
and `model_version` precisely so:

- Switching models leaves the old embeddings in the cache,
  available if the user reverts.
- The next clustering pass after a switch fully re-embeds the
  corpus under the new (provider, model, model_version) — these
  rows are absent from the cache, so the pipeline regenerates them.
- Cluster IDs are derived from `sha1(sorted member set)`; the
  member-set is determined by the embeddings the active model
  produces. A model swap that re-segments the corpus produces new
  cluster IDs, and `_mnemo/themes/<slug>.md` paths churn
  accordingly. This is the **right** behaviour: a different
  embedding space is a different clustering, and pretending
  otherwise would silently corrupt cluster identity. The user is
  warned via `mnemo_vault_status` immediately after the swap and
  before the next pass runs.
- `mnemo_vault_themes_inspect` surfaces the model fingerprint the
  current theme was clustered under, so a user can distinguish
  "this theme is from before the model swap" from "this theme is
  from after."
- A `mnemo_vault_recluster(force_reembed: true)` parameter lets a
  user force a clean re-embed even when content has not changed —
  useful when the provider has silently revised a model under the
  same version string.

### Cluster algorithm

#### Shipped behaviour (Slice 8 MVP)

Embeddings (replace TF-IDF vectorisation) → cosine similarity →
single-link agglomerative clustering. The label-quality gates and
ID-derivation rules below all apply. This delivers ~80% of
embeddings' value (synonym coalescing, broader topical association)
without requiring an HDBSCAN library.

**Threshold tuning per engine.** The single-link threshold is the
same shape (`similarity ≥ X`) for both engines, but the *value*
behaves differently because the underlying vector spaces differ:
TF-IDF cosines vs. dense-embedding cosines have different
distributions. Engine A reads its threshold from
`vault_clustering.embedding_threshold` (default `0.55`, looser than
Engine B's `0.35` because embeddings produce higher pairwise
cosines on related text). Engine B continues to use
`vault_clustering.heuristic_threshold`. The two are independent
config keys — tuning one does not affect the other — and both are
listed in the config keys table.

#### Deferred — HDBSCAN follow-up

HDBSCAN is the algorithmically right choice for unknown-cluster-
count, noise-tolerant clustering and was the original Engine A
spec. Reality check:

- No pure-Go HDBSCAN library currently meets the schema/test bar
  (idiomatic Go, stable API, MIT/BSD/Apache, actively maintained,
  matches the reference scikit-learn behaviour on golden corpora).
- Porting one from scratch is non-trivial correctness risk; agent
  assistance helps with mechanical translation but does not
  eliminate the correctness risk of reimplementing density-based
  clustering.
- The slice ships shippably with single-link, not with an
  in-development HDBSCAN port.

When a HDBSCAN binding becomes available (or the port is
green-lit as its own slice), Engine A swaps `agglomerative` for
`hdbscan` behind the same interface. Configuration shape ready
for that day:

| Parameter              | Default | Rationale                                    |
|------------------------|---------|----------------------------------------------|
| `min_cluster_size`     | 3       | matches `min_cluster_weight` config          |
| `min_samples`          | 2       | tolerant of slightly noisy clusters          |
| `cluster_selection_epsilon` | 0.0 | strict — prefer many small clusters over megaclusters |
| `metric`               | cosine  | standard for text embeddings                 |

These are documented under `vault_clustering.hdbscan.*` and ignored
by the running daemon until the HDBSCAN path is wired up. The keys
are reserved so users can write them today without warnings.

HDBSCAN's properties when it lands:

- No fixed cluster count assumption (the corpus has unknown topic
  count).
- Tolerates noise points (decisions/spans that don't belong in any
  theme).
- Density-based, so cluster labels are stable across runs given
  stable inputs.

### Cluster labelling

Within each cluster, identify representative documents by inverse
distance to cluster centroid. Take the top-5.

Then, in priority order:

1. **User-anchored label, if a qualifying `vault_user` document is
   present** (scope `"full"` or `"includes"` enabled, user wrote a
   note that landed in this cluster). Naively picking any
   `vault_user` member produces broken labels — a journal entry
   titled `2026-05-12` or a stub note titled `Inbox` can outweigh
   the rest of the cluster's content. To prevent that, the
   user-anchor path applies **all** of these quality gates before
   accepting the title:

   - **Centroid-closeness.** The candidate must be the
     centroid-closest `vault_user` member of the cluster (smallest
     embedding distance / largest TF-IDF similarity, depending on
     engine). Any `vault_user` is not enough — only the one closest
     to the cluster's centre.
   - **Minimum body content.** The note must have **≥ 200 tokens** of
     body content excluding frontmatter, code fences, and the title
     line. Stub notes and dailies routinely have one-line bodies; this
     filters them out. Threshold configurable via
     `vault_clustering.label.user_min_tokens`.
   - **Filename pattern exclusion.** Daily-note filenames are
     excluded regardless of content length. The default exclusion
     regex set (`vault_clustering.label.user_filename_exclude`) is:
     ```
     ^\d{4}-\d{2}-\d{2}(\.md)?$        # YYYY-MM-DD
     ^\d{4}_\d{2}_\d{2}(\.md)?$        # YYYY_MM_DD
     ^\d{4}\.\d{2}\.\d{2}(\.md)?$      # YYYY.MM.DD
     ^(daily|journal|journals|inbox|scratch|todo|untitled)(\.md)?$
     ```
     Plus any path under `daily/`, `journals/`, `inbox/`, `scratch/`.
     User overrideable.
   - **Title-content coherence.** The candidate's title must share
     at least one non-stopword token with the cluster centroid text
     (after stemming). A note titled "Random thoughts" whose body
     happens to mention auth middleware tangentially is not an
     anchor for the auth-middleware cluster.

   If no `vault_user` member passes all four gates, the user-anchor
   path is skipped and labelling falls through to step 2. The cluster
   members that *failed* the gates remain in the cluster — they
   just do not contribute the label.

2. **LLM-generated label.** Generate a label via Claude Haiku 4.5
   with the prompt: "These are excerpts grouped by topical
   similarity. Label the topic in ≤6 words, all lowercase, no
   punctuation:" followed by the top-5 representative excerpts. Cap
   at 6 words, lowercase, kebab-case for slug derivation. **Opt-in
   per T63 egress posture** — gated on
   `vault_clustering.label.engine: "llm"` (default `"bigram"`).
   Mere presence of an Anthropic API key is not enough; a user who
   has not opted in stays on the bigram path even with a key
   available. Disabled-but-asked-for (no key, rate-limited,
   provider error) falls through to step 3 and reports the
   degraded state via `mnemo_vault_status.warnings[]`.

3. **Bigram fallback.** Most-frequent non-stopword bigram across the
   cluster's top-5 excerpts. Crude but deterministic. Used when LLM
   labelling is disabled or fails.

The `mnemo_vault_themes_inspect` tool reports which of the three
paths produced the label, including the gate that fired when an
expected user-anchor was rejected. A user who sees a cluster
labelled "various topics" instead of their note title can immediately
see why ("daily-note pattern matched", "below 200-token minimum",
"title shared no tokens with centroid") and respond — either by
renaming the note, beefing it up, or relaxing a gate via config.

### Stability

Cluster IDs are generated as `theme_<sha1(sorted_member_doc_ids)[:8]>`.
This means: if the same set of documents clusters together on the next
pass, the ID is identical. A document joining/leaving changes the ID,
which is the right semantic (a different set is a different theme).

Renamed themes (same membership, new label) retain the same ID, so
`_mnemo/themes/<slug>.md` paths churn only when membership churns.

**Stability is scoped to an embedding fingerprint, not absolute.**
The ID derivation is stable across runs *within* a fixed
(provider, model, model_version) fingerprint. A model swap (or a
provider-side silent revision; see Mode 7) re-segments the corpus
and produces a fresh set of cluster IDs. This is intentional — a
different embedding space defines different clusters — and the
churn is warned about via `mnemo_vault_status` before the next
pass commits. The heuristic engine's stability is absolute (no
external fingerprint), modulo IDF recomputation, which is
deterministic for a given corpus.

---

## Engine B — heuristic (default)

Same input streams. No embeddings required. Labelling chain (Engine
B uses the same chain as Engine A — see "Cluster labelling" above):
user-anchored → optional LLM → bigram. LLM labelling is
independently opt-in via `vault_clustering.label.engine`; when off,
Engine B runs fully offline.

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

This produces fewer, broader clusters than HDBSCAN would. Acceptable
for the **default** engine — most users land here and the broader
clusters are easier to scan in the wing than dense fine-grained
ones. The post-pass `merge_threshold` second pass (Mode 1
mitigation) keeps near-duplicates from sprawling.

### Labelling

The unified labelling chain (see "Engine A → Cluster labelling")
applies to both engines: user-anchored → optional LLM → bigram. The
bigram path used by Engine B by default (since LLM labelling
defaults off) is:

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

#### Mitigation for domain drift

The heuristic engine applies two corrections before the
single-link pass to limit one-repo IDF dominance:

1. **Per-repo IDF cap.** IDF for a token is computed corpus-wide
   but clamped to `max(idf(token), p95(idf))` so a single repo's
   esoteric vocabulary cannot push tokens to the extremes of the
   IDF distribution. The clamp value is recomputed each pass.
2. **Per-repo document quota.** When the corpus is heavily
   imbalanced (one repo contributes >50% of documents), down-sample
   the dominant repo to 2× the second-largest repo's contribution
   before TF-IDF vectorisation. Down-sampling is stratified by
   doc_kind (decision/compaction/pattern) so each stream survives
   proportionally. Down-sample is configurable via
   `vault_clustering.heuristic.balance_factor` (default 2.0;
   `0` disables).

Both corrections are run only by the heuristic engine. The
embeddings engine is robust to vocabulary imbalance by
construction and does not need them.

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
  provider        TEXT NOT NULL,                 -- e.g. "voyage"
  model           TEXT NOT NULL,                 -- e.g. "voyage-3-lite"
  model_version   TEXT NOT NULL DEFAULT '',      -- provider-reported version tag; '' if unversioned
  dimensions      INTEGER NOT NULL,              -- vector length, sanity-check on read
  vector          BLOB NOT NULL,                 -- float32 packed
  computed_at     TEXT NOT NULL,
  PRIMARY KEY (doc_kind, entity_id, content_hash, provider, model, model_version)
);

CREATE INDEX cluster_embeddings_by_fingerprint
  ON cluster_embeddings (provider, model, model_version);

CREATE VIRTUAL TABLE themes_fts USING fts5(
  label, slug, centroid_text,
  content='themes', content_rowid='rowid'
);

CREATE TABLE cluster_runs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at      TEXT NOT NULL,
  ended_at        TEXT,                          -- NULL while running
  engine          TEXT NOT NULL,                 -- "embeddings" | "heuristic"
  provider        TEXT NOT NULL DEFAULT '',      -- empty for heuristic
  model           TEXT NOT NULL DEFAULT '',
  model_version   TEXT NOT NULL DEFAULT '',
  input_docs      INTEGER NOT NULL DEFAULT 0,
  output_themes   INTEGER NOT NULL DEFAULT 0,
  embedding_calls INTEGER NOT NULL DEFAULT 0,
  estimated_cost  REAL NOT NULL DEFAULT 0,       -- USD; NULL-ish 0 for heuristic
  failure_mode    TEXT NOT NULL DEFAULT '',      -- empty on success
  trigger         TEXT NOT NULL,                 -- "interval" | "manual" | "opportunistic"
  embeddings_bytes INTEGER NOT NULL DEFAULT 0,   -- cumulative size of cluster_embeddings.vector at pass end
  embeddings_rows  INTEGER NOT NULL DEFAULT 0    -- cumulative row count of cluster_embeddings at pass end
);

CREATE INDEX cluster_runs_by_started ON cluster_runs (started_at DESC);

CREATE TABLE theme_overrides (
  theme_id        TEXT PRIMARY KEY,              -- target theme
  directive       TEXT NOT NULL,                 -- "split" | "merge" | "relabel"
  payload         TEXT NOT NULL,                 -- JSON; shape depends on directive
  created_at      TEXT NOT NULL,
  applied_at      TEXT                           -- NULL until next pass consumes
);

CREATE TABLE theme_pins (
  theme_id        TEXT PRIMARY KEY,              -- never auto-retired while pinned
  pinned_at       TEXT NOT NULL,
  reason          TEXT NOT NULL DEFAULT ''
);
```

All seven are append-only (no destructive drops, no constraint
tightening). `themes` and `theme_members` are rewritten in full each
clustering pass; `cluster_embeddings` accumulates and is GC'd by a
separate pass when the underlying entity disappears. `cluster_runs`
grows unbounded by design (telemetry log) and is trimmed by a
background sweep when its row count exceeds `max_run_history`
(default 1000). `theme_overrides` rows are marked `applied_at` rather
than deleted, so the directive history survives. `theme_pins` rows
live until the user removes them.

**Vector encoding.** `cluster_embeddings.vector` is the float32
sequence in **little-endian IEEE 754**, contiguous, length =
`dimensions × 4` bytes. The encoding is fixed regardless of host
architecture so a daemon migrated between archs (or a backup
restored on a different arch) reads vectors correctly. Sanity check
on read: reject any row whose `length(vector) != dimensions * 4`.

### Renderer

`internal/vault/themes.go` (new) emits one Markdown file per row in
`themes` where `weight ≥ vault_clustering.min_cluster_weight`. The
renderer hooks into the existing periodic vault sync loop
(`Exporter.Sync` in v1, retained under `vault_layout="v2"` with
session/CI/PR writers removed per Slice 4). Page shape per the
parent design — note the embedded template depends on a small set
of template helpers registered by the renderer (`date`, `toJSON`,
`now`); they are documented in `internal/vault/README.md` next to
the probe-entity contract:

```markdown
---
type: theme
tags: [mnemo, mnemo/theme]
aliases: ["{{ .Label }}"]
weight: {{ .Weight }}
first-seen: {{ .FirstSeen | date "2006-01-02" }}
last-touched: {{ .LastTouched | date "2006-01-02" }}
repos: {{ .Repos | toJSON }}
label-source: {{ .LabelSource }}             {{/* "vault_user" | "llm" | "bigram" */}}
embedding_fingerprint: {{ .EmbeddingFingerprint | toJSON }}  {{/* null on heuristic */}}
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
{{/* Per Q3: pattern members are also surfaced as Related links to their
     standalone pages, not just as members. They appear in both places. */}}
{{ range .RelatedPatterns }}- [[_mnemo/patterns/{{ .Slug }}|{{ .Label }}]]
{{ end }}

## {{ .ProvenanceHeading }}  {{/* "Underlying decisions" | "Underlying entities" — see parent design's per-entity heading table */}}
{{ range .Members }}- {{ .Kind }} · {{ .Repo }} · {{ .EntityID }}
{{ end }}

<!-- /mnemo:generated -->

<!-- Your notes below this line — preserved across syncs. -->
```

The `ProvenanceHeading` field is computed by the renderer per the
parent design's per-entity heading table — for a theme whose
members are exclusively decisions/compactions it is "Underlying
decisions"; for a mixed-kind theme it is "Underlying entities".
Custom templates can override.

### Cross-repo derivation

A theme with `len(JSON(repos)) ≥ 2` AND `weight ≥ 4.0` triggers
emission of a cross-repo page at `_mnemo/cross-repo/<slug>.md` in
addition to the theme page. The cross-repo page renders the same
theme through a different lens: groups evidence by repo, surfaces
which-repos-first metadata, and is the page bridged into multi-repo
MOCs.

#### Threshold worked examples

Stream weights (from the Inputs section): decisions 1.0,
compactions 0.8, patterns 1.2, vault_user 1.5. Per-doc cap is 2.0
(see Mode 5). Threshold check: theme `weight ≥ 4.0` AND `len(repos)
≥ 2`.

| Cluster shape (member kinds × repos)                         | Sum of weights | Cross-repo page? |
|---------------------------------------------------------------|----------------|-------------------|
| 2 decisions in 1 repo + 1 decision in another                | 3.0            | no (below 4.0)    |
| 3 decisions × 2 repos + 1 pattern                            | 4.2            | yes               |
| 1 decision + 2 compactions × 2 repos                         | 2.6            | no                |
| 4 decisions × 3 repos                                         | 4.0            | yes (boundary)    |
| 1 user note + 2 decisions × 2 repos                          | 3.5            | no                |
| 1 user note + 3 decisions × 2 repos                          | 4.5            | yes               |

Reading: low-member clusters spanning only 2 repos do not become
cross-repo views even if they are technically multi-repo —
intentional, to avoid every casual mention becoming a page. A
single user-authored note (weight 1.5) gives a meaningful boost
toward the threshold; one note plus ~2.5 of other signal crosses
it. The threshold is tunable via `vault_clustering.min_cluster_weight`
and a future
`vault_clustering.cross_repo_weight_threshold` (not in Slice 8;
listed here for the reviewer's mental model).

No separate `cross_repos` table — cross-repo pages are a derived
view over `themes`.

---

## Cadence and triggering

### Default

`vault_clustering.recompute_interval` = `"24h"` (configured in parent
design).

Background goroutine inside `vault.Exporter` (or a sibling
`vault.Clusterer`) ticks at the interval and runs a full clustering
pass. Pass duration for a median user:

- **Heuristic engine.** Sub-second for 10³ documents; the
  bottleneck is TF-IDF recomputation, not clustering itself. For
  heavy users (10⁴+ documents) the pass remains under ~10 seconds.
- **Embeddings engine, warm cache.** Sub-second — cache hit on
  every document; the network round-trip is skipped entirely.
- **Embeddings engine, cold cache (full re-embed).** Bounded by
  provider RTT × batch count. With Voyage AI's batch endpoint
  (128 docs per request), a 10³-doc corpus is one batch round-trip
  — typically under 10 seconds end-to-end. A 10⁴ corpus is on the
  order of one minute.

Heavy users with a full re-embed running over the interval is the
worst case (network-bound, not compute-bound); the
`opportunistic_threshold` trigger plus the `force_reembed` MCP
parameter let the user shape this trade-off explicitly.

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
- Hot-reload via `mnemo_config`: **any** change to a
  `vault_clustering.*` key stops the existing goroutine and
  restarts with the new settings. The restart is cheap (no
  re-embedding unless `embedding_provider` / `embedding_model` /
  `embedding_model_version` changed, in which case Mode 7's
  warn-then-regenerate flow kicks in on the next pass). Per-user
  `clusterCancel` tracked on the `userEntry` struct alongside
  `vaultCancel` and `reconcilerCancel`. Generalising over
  "any `vault_clustering.*`" — rather than enumerating keys —
  means new keys added in follow-up slices inherit hot-reload
  automatically without touching the worker.
- Quiescence not required (clustering reads only, does not write to
  contended tables in the hot ingest path). Unlike the backup worker
  it can run at any time.
- Misconfiguration logs a warn and skips the worker (daemon never
  blocks on cluster setup failures). Specifically:
  - `recompute_interval`: must parse via Go's `time.ParseDuration`
    AND be ≥ `"60s"`. Smaller values are clamped up to `"60s"`
    with a warning; unparseable values fall back to the default
    `"24h"` with a warning.
  - `engine`: must be one of `"heuristic"`, `"embeddings"`.
    Anything else falls back to `"heuristic"` with a warning.
  - `embedding_provider`: must be a registered provider name.
    Unknown providers fall back to disabling the embeddings engine
    (heuristic runs) with a warning.
  - Numeric thresholds (`min_cluster_weight`, `heuristic_threshold`,
    `embedding_threshold`, `merge_threshold`, `per_doc_weight_cap`):
    must be in their plausible ranges (positive; thresholds in
    `[0, 1]`). Out-of-range values fall back to defaults with a
    warning.
  Every misconfiguration warning is also surfaced in
  `mnemo_vault_status.warnings[]` so the user can see why their
  configured value did not take effect.

### Triggered by

- Time tick at the configured interval.
- Manual `mnemo_vault_recluster` MCP tool call (immediate run).
- Significant corpus change (≥ N new high-signal entities since last
  pass; default N=50) — opportunistic re-cluster outside the timer.

### Concurrency

Two layers of discipline, working together:

- **In-database snapshot.** Clustering and vault sync share the
  existing `syncMu` discipline from v1: a cluster pass takes a
  separate `clusterMu`, periodic syncs run during/around it
  without conflict. The renderer reads cluster output via a
  snapshot taken under `clusterMu`, so a sync writing themes sees
  a consistent view.
- **On-disk atomicity.** The theme / cross-repo renderer writes
  every output file using the parent design's atomic-write protocol
  (`.<name>.mnemo.tmp` sibling + `fsync` + `rename`; see
  "Operational invariants → Sync atomicity" in
  `docs/design/vault-library-wing.md`). A clustering pass that
  produces N theme pages results in N atomic renames; if the
  daemon crashes mid-pass, the user sees a mix of (old, new) full
  files but never a torn file. The `_archive/` move for retirements
  is the same `rename(2)` discipline (also atomic on the same
  filesystem) — partial retirement is impossible from a crash mid-
  pass.

### Cost telemetry

Each pass records to the `cluster_runs` table (full DDL in the
"Output → Tables" section above). Captured fields:

- `started_at` / `ended_at`
- `engine` (`"embeddings"` or `"heuristic"`)
- `provider`, `model`, `model_version` (embeddings runs; empty for heuristic)
- `input_docs` — corpus size at pass start
- `output_themes` — emitted themes (capped by `max_themes` if applicable)
- `embedding_calls`, `estimated_cost` (USD; zero for heuristic)
- `failure_mode` — empty on success; specific codes on
  graceful-degrade paths (`embedding_provider_outage_fell_back_to_heuristic`,
  `embedding_provider_outage_no_fallback`, etc.)
- `trigger` (`"interval"` / `"manual"` / `"opportunistic"`)
- `embeddings_bytes`, `embeddings_rows` — `cluster_embeddings`
  table footprint at pass end, for backup-size monitoring

Surfaced via `mnemo_vault_status.last_cluster_run` and queryable
directly. Lets a user spot cost outliers and snapshot-size growth
without enabling DEBUG logging.

---

## User-facing controls

### Configuration keys (additions to parent design)

```json
{
  "vault_clustering": {
    "engine": "heuristic",
    "embedding_provider": "voyage",
    "embedding_model": "voyage-3-lite",
    "embedding_model_version": "",
    "min_cluster_weight": 3,
    "recompute_interval": "24h",
    "heuristic_threshold": 0.35,
    "embedding_threshold": 0.55,
    "max_themes": 200,
    "max_cross_repo": 50,
    "retire_after": "4320h",
    "max_run_history": 1000,
    "label": {
      "engine": "bigram",
      "user_min_tokens": 200,
      "user_filename_exclude": []
    },
    "hdbscan": {
      "enabled": false,
      "min_cluster_size": 3,
      "min_samples": 2,
      "cluster_selection_epsilon": 0.0,
      "metric": "cosine"
    }
  }
}
```

| Key                                | Default      | Hot-reload? |
|------------------------------------|--------------|-------------|
| `vault_clustering.engine`          | `"heuristic"` (explicit opt-in to `"embeddings"`) | yes |
| `vault_clustering.embedding_provider` | `"voyage"` | yes         |
| `vault_clustering.embedding_model` | `"voyage-3-lite"` | yes      |
| `vault_clustering.embedding_model_version` | `""` (provider-reported; user override forces a specific version for reproducibility) | yes |
| `vault_clustering.min_cluster_weight` | `3`       | yes         |
| `vault_clustering.recompute_interval` | `"24h"`   | yes         |
| `vault_clustering.heuristic_threshold` | `0.35` (TF-IDF cosine) | yes |
| `vault_clustering.embedding_threshold` | `0.55` (embeddings cosine) | yes |
| `vault_clustering.max_themes`      | `200`        | yes         |
| `vault_clustering.max_cross_repo`  | `50`         | yes         |
| `vault_clustering.retire_after`    | `"4320h"` (180 days) | yes  |
| `vault_clustering.max_run_history` | `1000`       | yes         |
| `vault_clustering.label.engine`    | `"bigram"` (explicit opt-in to `"llm"`, per T63 egress posture) | yes |
| `vault_clustering.label.user_min_tokens` | `200`  | yes         |
| `vault_clustering.label.user_filename_exclude` | `[]` (extends defaults) | yes |
| `vault_clustering.hdbscan.enabled` | `false` (reserved; ignored until HDBSCAN ships) | yes |
| `vault_clustering.per_doc_weight_cap` | `2.0`     | yes         |
| `vault_clustering.merge_threshold` | `0.9` (post-pass theme-similarity merge; set to `1.0` to disable) | yes |
| `vault_clustering.opportunistic_threshold` | `50` (re-cluster outside timer once N new high-signal entities accumulate) | yes |
| `vault_clustering.embedding_retry_max` | `3`      | yes         |
| `vault_clustering.fallback_to_heuristic_on_outage` | `true` | yes |
| `vault_clustering.heuristic.balance_factor` | `2.0` (per-repo document quota; `0` disables) | yes |

`max_themes` caps page emission to prevent a vault explosion when
clustering accidentally produces 5000 micro-clusters. Themes ranked
by weight; excess kept in the table but not rendered.

### MCP tools

- **`mnemo_vault_recluster`** — trigger an immediate clustering pass.
  Returns the new `cluster_runs` row. Parameters:
  - `engine`: optional override (`"heuristic"` or `"embeddings"`).
    Honours the egress posture: an `"embeddings"` override without
    `vault_clustering.engine: "embeddings"` configured is rejected
    with an error, not silently approved.
  - `force_reembed`: bool, default `false`. When `true`, invalidates
    the cache rows matching the active
    (provider, model, model_version) fingerprint and re-embeds the
    full corpus. Useful when a provider has silently revised a model
    under the same version string. Has no effect when the active
    engine is `"heuristic"`.
- **`mnemo_vault_themes_inspect`** — given a theme slug or ID,
  return the full member list with distances, the centroid text,
  the labelling source (user-anchored vs LLM vs bigram, including
  the gate that fired when an expected user-anchor was rejected),
  the related-theme links, and the embedding fingerprint the
  cluster was computed under. Lets a user understand why a cluster
  looks the way it does.
- **`mnemo_vault_themes_pin`** — pin / unpin a theme so it is
  exempt from `vault_clustering.retire_after` auto-archival.
  Parameters: `theme_id`, `unpin` (bool, default `false`),
  `reason` (string, optional). Persists to the `theme_pins` table.
- **`mnemo_vault_themes_split`** — manual override: mark a theme
  ID for splitting on the next pass. Persisted in a
  `theme_overrides` table. Next clustering pass applies the
  split-hint and reports outcomes. Inverse:
  **`mnemo_vault_themes_merge`**.

The split/merge tools are not in Slice 8's MVP — they are a
follow-up after real-world cluster quality data is in hand.
`themes_inspect`, `recluster` (with `force_reembed`), and
`themes_pin` ship with Slice 8.

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

**Mitigation (for the single-link engine that actually ships):**

- Raise `vault_clustering.min_cluster_weight` (default 3) to require
  stronger evidence before a cluster is emitted as a theme.
- Lower `vault_clustering.heuristic_threshold` (default 0.35) so the
  agglomerative merge admits looser pairs.
- Run a deterministic second pass: pairwise theme similarity
  (cosine over centroids); merge any pair with similarity > 0.9. The
  second-pass merge is on by default in the heuristic engine and
  configurable via `vault_clustering.merge_threshold` (default 0.9;
  set to `1.0` to disable).
- HDBSCAN-specific tunables (`min_cluster_size`,
  `cluster_selection_epsilon`) are reserved under
  `vault_clustering.hdbscan.*` for the deferred follow-up and
  ignored in Slice 8.

### Mode 2 — megacluster absorption

One huge cluster covers most of the corpus ("everything about Go"),
the rest are tiny. Symptom: weight distribution is bimodal with one
dominant cluster.

**Mitigation (single-link engine):**

- Raise `vault_clustering.heuristic_threshold` (default 0.35) — a
  stricter merge threshold prevents loose chains from absorbing
  weakly-related documents.
- Extend the stopword list with overused project-specific tokens.
- The domain-drift corrections (per-repo IDF cap + per-repo
  document quota) documented under "Engine B → Mitigation for
  domain drift" already work against megaclusters that arise from
  one-repo vocabulary dominance.
- If still occurring, partition the corpus by repo before
  clustering and merge themes post-hoc via the second-pass merge
  documented in Mode 1.
- HDBSCAN's `cluster_selection_epsilon` is reserved for the
  deferred follow-up; it is not a Slice 8 lever.

### Mode 3 — drift in labels

A theme's auto-label changes from "auth middleware redesign" to
"session token storage" between passes despite identical membership.
Symptom: `_mnemo/themes/<slug>.md` paths churn.

**Mitigation:** label is cached per theme ID. Recompute only when
membership changes. User-anchored labels (from vault_user docs) are
sticky regardless of pass.

### Mode 4 — embedding provider outage

Embedding API down for hours. Symptom: clustering pass cannot
fetch fresh vectors for any new / content-changed documents.

**Mitigation (graduated, in order of severity):**

1. **Stale-vector reuse.** If the cache holds vectors at the
   active (provider, model, model_version) fingerprint for every
   active corpus document — i.e. nothing changed since last pass —
   the pass runs entirely from cache. Provider outage is invisible.
2. **Partial cache miss.** If some documents need fresh vectors,
   the pass attempts the provider call once. On 5xx / timeout /
   rate-limit, it retries with exponential backoff up to
   `vault_clustering.embedding_retry_max` (default 3). If all
   retries fail and the user has opted into the embeddings engine
   on this pass, the pass **falls back to the heuristic engine
   for this run only** (matching the vectorisation-section
   fallback rule). The next scheduled pass tries the embeddings
   provider again. The fall-back is logged and surfaced in
   `mnemo_vault_status.warnings[]` and the `cluster_runs.failure_mode`
   column for that row (`"embedding_provider_outage_fell_back_to_heuristic"`).
3. **Configured to refuse fallback.** A user who prefers to skip
   the pass rather than mix engine outputs can set
   `vault_clustering.fallback_to_heuristic_on_outage: false`
   (default `true`). In that case the pass aborts without
   touching the existing `themes` snapshot; the
   `_mnemo/themes/_index.md` shows a "last computed N hours ago"
   banner; `cluster_runs.failure_mode =
   "embedding_provider_outage_no_fallback"`.

Either path preserves user data — the prior themes snapshot is
never partially overwritten. The two paths differ only in whether
a single pass produces output or skips.

### Mode 5 — vault content dominates spurious clusters

User has a sprawling vault with one large note tangentially mentioning
many topics. That note ends up in every cluster as a representative,
making themes look incoherent.

**Mitigation:** per-document weight cap, applied to the *total* weight
a single document contributes *across all clusters it lands in*, not
to its per-cluster contribution. A document is capped at total
weight `min(stream_weight, 2.0)` — but if it lands in N clusters,
its per-cluster contribution is `min(stream_weight, 2.0) / N`. So a
vault note (stream weight 1.5, cap 1.5) appearing in 3 clusters
contributes 0.5 to each. A pattern row (stream weight 1.2) appearing
in 2 clusters contributes 0.6 to each. The cap exists to disarm
single-document mega-influence in pathological splay cases; the
per-cluster *attenuation* is what actually fixes the failure mode.

The cap of 2.0 is deliberately set just above the highest stream
weight (1.5 for vault_user) so that in the common case (a document
lands in exactly one cluster) the cap does not bind. It binds only
when a document splays into N ≥ 2 clusters, attenuating per-cluster
contribution to 1/N of its full weight.

Configurable via `vault_clustering.per_doc_weight_cap` (default
`2.0`), reserved for future tuning.

### Mode 6 — cluster IDs churn from minor edits

A user edits one decision summary slightly; its embedding shifts
marginally; the cluster it belongs to is now a different membership
set; the cluster ID changes; the theme page moves.

**Mitigation:** ID derivation uses the *sorted member set*, not the
embeddings themselves. A minor text edit doesn't change membership;
only addition/removal does. Edits to non-member documents don't
affect ID.

### Mode 7 — embedding model swap silently churns clusters

A user changes `vault_clustering.embedding_model` from
`voyage-3-lite` to `voyage-3` (or the provider versions the model
in-place). The new embedding space segments the corpus
differently. Cluster memberships shift, IDs regenerate, and every
`_mnemo/themes/<slug>.md` path churns at once.

**Mitigation:** the cache key includes
`(provider, model, model_version)`, so the swap is observable in
the cache. The next clustering pass detects that the active
(provider, model, model_version) has no cached rows for any
corpus document and **warns** before regenerating: `embedding model
changed from <old> to <new> — clusters will be regenerated and
slug paths may change`. Warning surfaces in `mnemo_vault_status`,
the daemon log, and the pass's `cluster_runs` row. The user can
abort by reverting the config; the prior themes table is left
unchanged in that case (a clustering pass runs end-to-end or not
at all). A `force_reembed: true` flag on `mnemo_vault_recluster`
lets a user deliberately request a clean re-embed when they
suspect a silent provider-side model revision.

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

**Answer (implemented).** Implicit retirement at 180 days,
configurable via `vault_clustering.retire_after`. Mechanics:

- On every clustering pass, compute `most_recent_member_ts` for
  each theme. If `now − most_recent_member_ts >
  vault_clustering.retire_after` and the theme is not present in
  `theme_pins`, move its page from `_mnemo/themes/<slug>.md` to
  `_mnemo/themes/_archive/<slug>.md`. The move is the same atomic
  protocol used elsewhere; the file is not regenerated until the
  cluster reactivates.
- `_index.md` lists active themes; an archive section at the bottom
  links to retired ones with their last-seen dates.
- A retired theme that receives new members on a later pass is
  promoted back to the live directory automatically. Cluster ID
  survives the round-trip because membership-set identity is
  preserved.
- Pinning: `mnemo_vault_themes_pin(theme_id)` writes to
  `theme_pins`. Pinned themes never auto-retire. Unpin via the same
  tool with `unpin: true`.
- Status: `mnemo_vault_status.abstractions.themes` reports both
  `rendered` and `archived` counts.

### Q3: Patterns-only clusters versus theme membership for patterns

A pattern that recurs across 5 sessions in 3 repos: is it a
standalone `_mnemo/patterns/<slug>.md` (Slice 7), a member of a
broader theme (Slice 8), or both?

**Working answer:** both. The pattern lives in `patterns/`,
independently rendered. The theme containing it cites it under
"Related" with a wikilink. Two views of the same underlying signal.

### Q4: Translation / multilingual corpus

A non-English user (or a polyglot one) generates content in multiple
languages. Voyage AI's `voyage-3-lite` is multilingual; single-link
agglomerative over cosine distance is language-agnostic; LLM
labelling via Haiku is multilingual. Heuristic engine's Porter
stemmer is English-only.

**Working answer:** embeddings engine handles multilingual natively
(via the multilingual provider model + language-agnostic
clustering). Heuristic engine is English-best-effort with one
partial mitigation that ships in Slice 8: tokens are lowercased
via `unicode.ToLower` (not just ASCII `strings.ToLower`), so
Latin-script multilingual content tokenises sensibly even though
the Porter stemmer only stems English. Cross-language synonym
coalescing (e.g. "auth" / "authentification") still requires the
embeddings engine. Per-language stemmer plug-ins are a follow-up.

---

## Acceptance criteria

The clustering engine is considered shipped when:

1. Both engines (embeddings and heuristic) produce valid `themes`
   and `theme_members` rows for a test corpus.
2. Default engine is `"heuristic"` regardless of API key presence.
   The embeddings engine activates only on explicit
   `vault_clustering.engine: "embeddings"` config (per T63 egress
   posture).
3. The embeddings engine ships with **single-link agglomerative**
   clustering reusing Engine B's pipeline downstream of
   vectorisation. HDBSCAN is **not** a Slice 8 commitment.
4. Themes are rendered as `_mnemo/themes/<slug>.md` pages with the
   parent design's frontmatter and fence contract.
5. Cross-repo derivation emits `_mnemo/cross-repo/<slug>.md` for
   themes with `len(repos) ≥ 2` and `weight ≥ 4.0`.
6. Cluster IDs are stable across runs given stable membership.
7. `cluster_embeddings` cache is keyed by
   `(doc_kind, entity_id, content_hash, provider, model, model_version)`
   and prevents re-embedding unchanged content for the active
   (provider, model, model_version).
8. Switching `vault_clustering.embedding_model` triggers a clean
   re-embed on the next pass; old rows are retained in the cache;
   `mnemo_vault_status` warns about the model change.
9. The embedding provider interface (`EmbeddingProvider`) is
   implemented by at least one concrete provider (Voyage AI) and
   wired so that a second provider could be added without changing
   the rest of the pipeline.
10. `cluster_runs` telemetry is queryable.
11. `mnemo_vault_recluster` (including `force_reembed: true` and
    `engine` override), `mnemo_vault_themes_inspect`, and
    `mnemo_vault_themes_pin` (with `unpin`) MCP tools work
    end-to-end. `themes_split` / `themes_merge` stubs land
    (config-rejecting placeholders, ship in follow-up).
12. User-anchored labels (vault_user documents) win over LLM
    labels **only** when all four label-quality gates pass
    (centroid-closest, ≥ 200-token body, filename-pattern
    exclusion, title-content token overlap). `themes_inspect`
    reports the labelling path used and the gate that fired when
    an anchor was rejected.
13. Embedding provider outage degrades gracefully per Mode 4's
    graduated mitigation: stale-vector reuse, then bounded retries,
    then heuristic fallback (or skip-pass if
    `fallback_to_heuristic_on_outage: false`). Last `themes`
    snapshot is preserved in every case.
14. Two-key egress test passes: with both API keys present in the
    environment but neither `vault_clustering.engine: "embeddings"`
    nor `vault_clustering.label.engine: "llm"` set, a clustering
    pass produces zero outbound HTTP requests. Each opt-in is
    independently testable.
15. Theme retirement test passes: a theme with no member newer than
    `vault_clustering.retire_after` moves to
    `_mnemo/themes/_archive/` on the next pass; pinning the theme
    (`mnemo_vault_themes_pin`) prevents the move; new members on a
    later pass promote it back without churning the cluster ID.
16. `cluster_runs` rows beyond `max_run_history` are trimmed by a
    background sweep; the most recent rows are preserved.
17. `vault_clustering.embedding_model_version` override pins the
    cache key; rows for other versions in the cache are ignored by
    the active pass but retained for revert.
18. Per-engine thresholds applied correctly:
    `heuristic_threshold` tunes Engine B,
    `embedding_threshold` tunes Engine A,
    and changing one does not affect the other.
19. Misconfiguration recovery: unparseable `recompute_interval`,
    unknown `engine`, unknown `embedding_provider`, and
    out-of-range numeric thresholds all fall back to documented
    defaults with a warning surfaced in
    `mnemo_vault_status.warnings[]`.

Split/merge tools (Q1) ship in a follow-up slice. HDBSCAN
clustering (deferred Engine A path) ships in a separate follow-up
slice if/when a pure-Go HDBSCAN binding meets the quality bar.

---

## Implementation outline

Roughly in this order. Each is reviewable independently.

1. **Schema** — add `themes`, `theme_members`, `cluster_embeddings`,
   `themes_fts`, `cluster_runs`, `theme_overrides`, `theme_pins` (plus
   the `cluster_embeddings_by_fingerprint` index and the
   `cluster_runs_by_started` index) to `internal/store/schema.sql`
   (single source of truth per T49, PR #82). sqlift `AllowNone` gate
   admits the pure additions on next daemon start. Pre-migration backup
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
5. **Embeddings engine (opt-in)** — `internal/cluster/embeddings.go`
   + `internal/cluster/embedding_provider.go` (interface) +
   `internal/cluster/voyage.go` (first concrete provider). The
   embedding output feeds into the same single-link agglomerative
   clustering used by Engine B — no HDBSCAN port in Slice 8.
   `cluster_embeddings` cache keyed by
   `(doc_kind, entity_id, content_hash, provider, model, model_version)`.
   Documented in CLAUDE.md alongside other egress features per
   T63's posture (PR #92).
6. **Labelling** — `internal/cluster/label.go`. User-anchor → LLM →
   bigram fallback chain. The LLM step has its own opt-in gate
   (`vault_clustering.label.engine: "llm"`, default `"bigram"`) —
   independent of the embeddings-engine opt-in, per the two-key
   egress matrix. The user-anchor quality gates (centroid-closest,
   token minimum, filename exclusion, title-content coherence) land
   here.
7. **Renderer** — `internal/vault/themes.go`,
   `internal/vault/cross_repo.go`. Hooks into existing `Exporter.Sync`
   loop.
8. **MCP tools** — `mnemo_vault_recluster` (with `engine` + `force_reembed`),
   `mnemo_vault_themes_inspect`, `mnemo_vault_themes_pin` (with
   `unpin`). Wired in `internal/tools/tools.go`. `mnemo_vault_themes_split`
   / `themes_merge` land as config-rejecting placeholder stubs;
   live implementations ship in the follow-up slice.
9. **Telemetry** — `cluster_runs` writes from both engines; reporting in
   `mnemo_vault_status`.
10. **Tests** — golden corpus + expected clusters for both engines;
    stability test (same input → same IDs within an embedding
    fingerprint); embedding cache hit/miss tests; user-anchor
    labelling tests for each of the four quality gates;
    **two-key egress test** (neither opt-in → zero outbound HTTP);
    Mode 4 graduated outage tests (stale-vector reuse, retry +
    fallback, refuse-fallback config); Mode 7 model-swap warning
    test; theme retirement test (retire after threshold, pin
    prevents, new members promote back, ID stable); `cluster_runs`
    trim test (`max_run_history` enforced); `embedding_model_version`
    pin test; atomic-write resilience test (kill mid-pass, verify
    no torn files and at most one `.tmp` left behind); per-engine
    threshold test (different defaults applied to the right engine);
    domain-drift mitigation test (per-repo IDF cap + balance_factor);
    misconfiguration recovery test (unparseable interval / unknown
    engine falls back with status warning).

Estimated effort (agent-assisted, sustained engagement): the
mechanical pieces — schema DDL, renderer, MCP tool wiring, worker
registration, golden-corpus tests — are hours of work each. Engine B
and the embeddings engine on top of single-link clustering are days
of generation + review/iteration. **HDBSCAN is explicitly out of
scope** for this slice; it is reserved for a follow-up if and when a
pure-Go HDBSCAN binding becomes available. The label-quality gates,
model-version pinning, and provider-agnostic interface are
single-day additions each, sequenced after the Engine B baseline
lands.

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
