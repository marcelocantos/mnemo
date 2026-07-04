# Topic Segmentation — the message-level extent axis

*Status: design draft — 2026-07-01.*
*Extends `docs/design/vault-clustering.md` (Slice 8, T64.8).*
*Depends on the clustering engine (themes / theme_members / cluster_embeddings) shipping first.*

---

## TL;DR

Today a search hit returns the matching message plus ±N neighbours.
The analogy that motivates this design: in code search you do not
want three lines above and below a match — you want the *enclosing
function*, and above it the class, the file, the module. That
structure is not computed per query; the AST already exists and
search walks up it.

This design gives transcripts the same thing: a **precomputed
topic-AST over each session's message stream**, so a hit can be
expanded to its smallest enclosing topic-coherent span, and a theme
can be resolved to every span that is about it, across sessions.

It is deliberately built as a **second extent axis of the existing
clustering engine (T64.8)**, not a parallel system:

- The clustering engine's corpus is document-level today —
  `corpus(doc_id, kind, …)` with `kind ∈ {decision, compaction,
  pattern, vault_user}`. This design adds a **new corpus kind,
  `segment`**, produced by a segmentation stage that runs upstream
  of clustering. Segments then cluster into themes through the
  *same* Engine A / Engine B pipeline. One engine, two extents.
- **Two hierarchies, kept distinct.** *Extent* hierarchy (a big
  span contains smaller spans) comes from multi-scale boundary
  detection and lives in a new `topic_segments` table. *Theme*
  taxonomy (a broad topic contains narrower ones) comes from
  **retaining the agglomerative dendrogram** instead of cutting it
  at one height — which reverses vault-clustering.md's "themes are
  flat" non-goal (`vault-clustering.md:81`).
- **Overlap is native, not bolted on.** Segments are arbitrary
  intervals over the message sequence, so they nest and overlap for
  free. A segment maps to one *primary* theme (its agglomerative
  cluster) and any number of *secondary* themes (centroid-similarity
  above a threshold). The user-facing "non-empty intersection
  between segments covering two themes" is then a plain interval-
  intersect + join, computable in SQL.

Nothing here is destructive to the schema: one new table, one new
FTS table, one watermark table, two new nullable columns on existing
tables, and message vectors riding the existing `cluster_embeddings`
store. All admitted by sqlift `AllowNone` (T49 policy).

---

## Goals

1. **Enclosing-span retrieval.** A search hit resolves to the
   smallest topic-coherent span that contains it, and to any coarser
   span up the extent tree — the "enclosing function → class → file"
   experience, precomputed.
2. **Cross-session theme drill-down.** "That bug we fixed last week"
   resolves a *theme* to every segment about it, across sessions and
   repos, time-ordered — because segments feed the same cross-session
   clustering that already produces themes.
3. **Hierarchy from structure the engine already computes.** Extent
   hierarchy from multi-scale boundary depth; theme hierarchy from
   the agglomerative dendrogram. Neither tree is hand-authored.
4. **Overlap for free.** Arbitrary intervals + soft (primary +
   secondary) theme membership. No bespoke overlap machinery.
5. **Graceful degradation, matching the parent's egress posture.**
   Local structural + lexical boundary detection is the always-on
   default (zero egress). Embedding-drift boundaries and LLM
   labelling are each independently opt-in, exactly as Engine A and
   `label.engine: "llm"` already are.

---

## Non-goals

- **Replacing compaction spans.** Compaction spans are window-based
  work summaries (500 msgs / 60k chars); segments are topic-coherent
  intervals. Both feed the corpus; a theme may have members of both
  kinds. Segments are additive signal, not a replacement.
- **Replacing FTS search.** As in the parent design, FTS5 over
  message bodies remains the retrieval backbone. Segmentation adds
  *expansion and grouping* over the same hits.
- **Real-time segmentation.** Segments are computed incrementally at
  ingest with a provisional tail (see "Tail instability"); they are
  not re-derived on every keystroke.
- **A generic segmentation API.** The engine serves search expansion
  and the clustering corpus; it is not a standalone topic-modelling
  surface.

---

## Data model

Two orthogonal concepts, deliberately separated — conflating them is
the trap that forces a single tree to be both nesting *and*
overlapping, which no tree can be.

### Segment = extent (an interval)

A segment is a contiguous interval `[from_msg_id, to_msg_id]` over a
session's **substantive** messages (`messages.is_noise = 0`, ordered
by `messages.id`). Because a segment is *just an interval*:

- **Nesting** is interval containment: `A ⊆ B` iff
  `A.from ≥ B.from AND A.to ≤ B.to`. Multi-scale boundary detection
  produces a forest of nested spans (level 0 = finest).
- **Overlap** is interval intersection: two spans may partially
  overlap with neither containing the other. No special
  representation — it falls out of allowing arbitrary intervals.

`parent_id` and `level` materialise the containment forest for cheap
tree walks, but they are derivable from the intervals and are a
convenience, not a source of truth.

### Theme = identity (a cross-session cluster)

A theme is a cluster label produced by T64.8's engine ("the FTS5
tokenizer bug"), reusable across sessions. Segments become
first-class members of themes via **the existing `theme_members`
table** with `doc_kind = 'segment'`.

- **Primary membership** — the segment's agglomerative cluster
  assignment (the hard cluster it lands in). One per segment.
- **Secondary membership** — any theme whose centroid similarity to
  the segment exceeds `segmentation.secondary_threshold`. Zero or
  more per segment. This is what makes a single segment "about" more
  than one theme (debugging an FTS5 bug that turns out to be a
  schema-migration issue → primary `fts5`, secondary
  `schema-migration`) without switching the engine to soft
  clustering.

Because `theme_members` is keyed `(theme_id, doc_kind, entity_id)`,
one segment already may appear under several `theme_id`s — the schema
needs no change to represent overlap; only a nullable
`membership_kind` / `similarity` column to distinguish primary from
secondary.

### Theme taxonomy = retained dendrogram

Single-link agglomerative clustering *is* a dendrogram. T64.8 cuts it
at one height and discards the tree (flat themes). This design keeps
it: `themes` gains a nullable `parent_theme_id` and `depth`, so
"SQLite issues ⊃ FTS5 bug ⊃ diacritic handling" is navigable. This
**explicitly reverses** the flat-themes non-goal; the parent doc
already anticipated it ("A future iteration may add parent/child
relationships if cluster labels prove they warrant the structure",
`vault-clustering.md:81-83`).

---

## Boundary detection (full pipeline)

Three tiers, each independently gated, mirroring the parent's
heuristic-default / embeddings-opt-in / LLM-opt-in structure.

### Tier 1 — structural priors (always on, zero egress)

Cheap signals already present in the schema:

- **Idle gaps.** Large `messages.timestamp` deltas between adjacent
  substantive messages are strong topic-switch priors.
- **User-turn cadence.** A fresh user imperative after a long
  assistant/tool run is a candidate boundary.
- **Decision boundaries.** The `decisions` table already marks
  sub-task completion (proposal→confirmation); these are boundary
  hints for free.
- **Tool/file locality.** A shift in the set of files touched
  (`tool_input` paths) or tools used marks a working-context change.
- **Compaction-span edges** as coarse pre-partition guides.

Tier 1 alone yields a usable first-pass extent tree with no vectors
and no API calls.

### Tier 2 — lexical / embedding drift (TextTiling)

Classic cohesion-based segmentation: score adjacent windows by
similarity, detect *valleys* (depth scores), place boundaries at the
deepest. Run at **multiple window widths** → nested boundary sets →
the extent hierarchy directly (multi-scale is what gives levels).

- **Default (Engine B parity): lexical.** TF-IDF / bag-of-words
  cohesion over token windows. No per-message vectors stored, no
  egress. Reuses the parent's tokeniser (lowercase, stopwords,
  Porter stem).
- **Opt-in (Engine A parity): embedding drift.** Per-message
  embeddings, cosine drift. Message vectors ride the **existing
  `cluster_embeddings` table** with `doc_kind = 'message'`,
  `entity_id = messages.id`, keyed by content hash + provider/model/
  version exactly as document vectors are. No new vector store.

### Tier 3 — LLM refine + label (opt-in)

A background Haiku pass, gated by the parent's existing
`label.engine: "llm"` opt-in, that (a) adjusts Tier-1/2 candidate
boundaries where cohesion is ambiguous and (b) emits each segment's
`summary` and `label`. With the LLM off, `summary`/`label` fall back
to the bigram/centroid-excerpt heuristic already specified for
themes. The LLM sees a bounded window (the segment's excerpts), so
the failure mode is bounded — the same argument the parent makes in
"why not delegate everything to an LLM".

### Tail instability

Topic boundaries near the *end* of a growing transcript are
unstable: a span you closed may extend when the next messages arrive.
Mitigation: only **seal** a segment when it is followed by a strong
boundary *and* ≥ `segmentation.seal_lookahead` substantive messages
of trailing context. The unsealed tail is recomputed each pass. The
`segment_scan_state` watermark advances only to the last *sealed*
message, so incremental re-segmentation never rewrites sealed spans.
This mirrors the `decision_scan_state` watermark pattern.

---

## Schema (additive, sqlift `AllowNone`)

```sql
CREATE TABLE topic_segments (
  id            TEXT PRIMARY KEY,          -- seg_<sha1(session_id|from|to|method|level)[:12]>
  session_id    TEXT NOT NULL,
  from_msg_id   INTEGER NOT NULL,          -- messages.id, inclusive
  to_msg_id     INTEGER NOT NULL,          -- messages.id, inclusive
  level         INTEGER NOT NULL,          -- 0 = finest; higher = coarser (multi-scale)
  parent_id     TEXT,                      -- enclosing segment (materialised); NULL at top level
  method        TEXT NOT NULL,             -- "structural" | "drift" | "llm"
  confidence    REAL NOT NULL DEFAULT 0,   -- boundary confidence [0,1]
  sealed        INTEGER NOT NULL DEFAULT 0,-- 1 once the trailing boundary is stable
  label         TEXT,                      -- short topic label (NULL until labelled)
  summary       TEXT,                      -- segment abstract for FTS (NULL until labelled)
  repo          TEXT,
  first_ts      TEXT,                      -- timestamp of from_msg_id
  last_ts       TEXT,                      -- timestamp of to_msg_id
  computed_at   TEXT NOT NULL
);
CREATE INDEX topic_segments_by_session ON topic_segments (session_id, from_msg_id, to_msg_id);
CREATE INDEX topic_segments_by_parent  ON topic_segments (parent_id);

CREATE VIRTUAL TABLE topic_segments_fts USING fts5(
  label, summary,
  content='topic_segments', content_rowid='rowid'
);

CREATE TABLE segment_scan_state (
  session_id           TEXT PRIMARY KEY,
  segmented_through_id INTEGER NOT NULL,   -- highest sealed messages.id
  method               TEXT NOT NULL,
  scanned_at           TEXT NOT NULL
);

-- Extensions to T64.8 tables (nullable additions only):
ALTER TABLE themes        ADD COLUMN parent_theme_id TEXT;     -- dendrogram parent; NULL at root
ALTER TABLE themes        ADD COLUMN depth           INTEGER;  -- dendrogram depth; NULL if flat
ALTER TABLE theme_members ADD COLUMN membership_kind TEXT;     -- "primary" | "secondary"; NULL = legacy/primary
ALTER TABLE theme_members ADD COLUMN similarity      REAL;     -- centroid similarity for the membership
```

Notes:

- **Segment IDs are stable** the same way theme IDs are — derived
  from the span's identifying tuple, so a re-segmentation that yields
  the same interval keeps the ID (page/reference stability).
- **`doc_kind = 'segment'`** joins segments into the existing
  `theme_members` / clustering machinery with no new join table.
  `topic_segments.id` is the `entity_id`.
- **Message vectors** need no new table — they ride
  `cluster_embeddings` under `doc_kind = 'message'`. This materially
  grows the vector store, so per T64.8's telemetry the growth shows
  up in `cluster_runs.embeddings_bytes`; the *default* (lexical
  Tier 2) stores no per-message vectors at all.
- **Corpus weight** for `segment` documents: `0.9` — between
  compaction (0.8) and decision (1.0); a topic-coherent span is
  richer signal than a fixed-window compaction but LLM-derived like
  it. Listed alongside the existing stream weights.

---

## Search integration — the payoff

### Expand a hit to its enclosing span

`mnemo_search` gains an `expand` parameter:

- `"none"` (default) — current ±N-message behaviour, unchanged.
- `"segment"` — return the smallest enclosing sealed segment for
  each hit: its `id`, `[from_msg_id, to_msg_id]`, `label`, `summary`.
- `"segment:coarse"` — walk `parent_id` to the top-level span.

The returned range hydrates through the existing session-range read
(`mnemo_read_session` over `[from_msg_id, to_msg_id]`). This is
literally "enclosing function instead of three lines."

When a hit sits inside two overlapping segments, both are returned,
ranked by `confidence` then narrowness — the resolution the parent
design defers on ambiguous membership.

### Resolve a theme to its segments across sessions

Because segments are theme members, the parent's existing
theme-hit result type in `mnemo_search` gains a drill-down: a theme
→ its `segment` members, time-ordered, across sessions and repos.
This is the path for "that bug we fixed last week": FTS on
`themes_fts` → theme `fts5-tokenizer-bug` → its segments → the one
from last week → expand.

### New tool: `mnemo_segments`

Query segments directly:

- by `session_id` (the topic-AST of one session),
- by `theme_id` (all spans about a theme),
- by `containing_msg_id` (which spans enclose this message),
- by FTS over `label` / `summary`,
- with `overlaps_theme` (two theme IDs) → segments that are members
  of both, i.e. the non-empty intersection the feature is named for.

---

## Failure modes

- **Tail instability** — addressed by seal-on-lookahead + watermark
  (above). Never rewrite a sealed span.
- **Over-segmentation** — too many tiny spans. Levers: minimum span
  size (`segmentation.min_span_msgs`), boundary-depth threshold, and
  the multi-scale hierarchy itself (fine spans roll up into coarse
  ones, so the finest level being noisy is tolerable — callers ask
  for a coarser level).
- **Vector-store blow-up** — per-message embeddings are far more
  numerous than the ~10³ documents T64.8 embeds. Mitigations: the
  default Tier 2 is lexical (no stored vectors); embedding drift is
  opt-in; when on, embed substantive messages only, and GC message
  vectors when their session ages out (reuse the parent's
  `cluster_embeddings` GC pass). Growth is visible in
  `cluster_runs.embeddings_bytes`.
- **Boundary quality worse than ±N** — the whole feature is a net
  loss if boundaries are bad. Ship behind an eval harness: a golden
  set of hand-segmented sessions, scored with a windowed boundary
  metric (Pk / WindowDiff). Do not enable `expand: "segment"` by
  default until the metric clears a bar on the golden set.
- **Dendrogram churn** — retaining the tree inherits T64.8's
  membership-set ID stability; a `parent_theme_id` edge changes only
  when membership changes. Same mitigation as Mode 6 there.

---

## Acceptance criteria

1. A segmentation pass produces `topic_segments` rows for a test
   session, with nested (`parent_id`/`level`) and, where the
   transcript warrants, overlapping intervals.
2. Tier 1 (structural) runs with zero egress and no stored vectors,
   and is the default.
3. Tier 2 lexical drift produces multi-scale boundaries; Tier 2
   embedding drift is opt-in and stores message vectors in
   `cluster_embeddings` under `doc_kind = 'message'`, never
   activating on API-key presence alone.
4. Tier 3 LLM labelling is opt-in via the parent's
   `label.engine: "llm"`; with it off, segments still get
   heuristic labels/summaries and the pass makes zero outbound
   calls.
5. Segments participate in clustering as `doc_kind = 'segment'`
   corpus documents and appear as `theme_members`.
6. A segment carries exactly one `membership_kind = 'primary'` theme
   and zero-or-more `'secondary'` themes above
   `segmentation.secondary_threshold`.
7. `themes.parent_theme_id` / `depth` are populated from the
   retained dendrogram; a theme's ancestors and descendants are
   navigable.
8. `mnemo_search expand="segment"` returns the smallest enclosing
   sealed segment; `"segment:coarse"` returns the top-level span;
   `"none"` is byte-identical to today's behaviour.
9. `mnemo_segments` answers by-session, by-theme, by-containing-
   message, by-FTS, and `overlaps_theme` (intersection) queries.
10. Incremental re-ingest never rewrites a sealed segment; the
    `segment_scan_state` watermark advances only past sealed spans.
11. Boundary quality clears the Pk/WindowDiff bar on the golden set
    before `expand="segment"` is enabled by default.
12. All schema changes pass under sqlift `AllowNone` (pure
    `CREATE` + nullable `ADD COLUMN`).

---

## Implementation outline

Roughly in order; each reviewable independently.

1. **Schema** — `topic_segments`, `topic_segments_fts`,
   `segment_scan_state`, and the four nullable columns on
   `themes` / `theme_members`, into `internal/store/schema.sql`.
2. **Tier 1 segmenter** — `internal/segment/structural.go`. Idle
   gaps + user cadence + decision edges + tool/file locality →
   candidate boundaries → sealed spans. Default, deterministic,
   golden-fixture tested.
3. **Watermark + incremental pass** — seal-on-lookahead, integrated
   into the ingest path like decision scanning.
4. **Tier 2 lexical drift** — `internal/segment/texttiling.go`.
   Multi-scale depth scoring → nested levels. Reuses the parent's
   tokeniser.
5. **Corpus wiring** — extend `internal/store/cluster_corpus.go` to
   emit `segment` documents (weight 0.9) into the clustering corpus.
6. **Dendrogram retention** — extend the agglomerative clusterer to
   record `parent_theme_id`/`depth` instead of only the cut.
7. **Secondary membership** — centroid-similarity pass writing
   `membership_kind = 'secondary'` rows above threshold.
8. **Tier 2 embedding drift (opt-in)** — message vectors via the
   existing `EmbeddingProvider` into `cluster_embeddings`.
9. **Tier 3 LLM label/refine (opt-in)** — reuse the parent's Haiku
   labelling gate for segment `label`/`summary`.
10. **Search** — `mnemo_search expand=…`, theme→segment drill-down,
    and the `mnemo_segments` tool in `internal/tools/tools.go`.
11. **Eval harness** — golden hand-segmented sessions + Pk/WindowDiff
    scoring; gates the default-on decision for `expand="segment"`.

---

## Appendix: relationship to existing infrastructure

- **`docs/design/vault-clustering.md` / T64.8** — the parent. This
  design adds one corpus kind (`segment`), reuses Engine A/B
  clustering, `theme_members`, `cluster_embeddings`, the labelling
  chain, and the worker/cadence machinery. It reverses the parent's
  flat-themes non-goal by retaining the dendrogram.
- **`decisions` / `decision_scan_state`** — decision boundaries are
  a Tier 1 signal; the incremental watermark pattern is copied
  directly.
- **`compactions`** — compaction-span edges are coarse boundary
  priors; span summaries remain an independent corpus kind.
- **T49 / sqlift `AllowNone`** — every change is a pure addition or
  a nullable column; no drops, no constraint tightening.
- **T63 egress posture** — Tier 2 embeddings and Tier 3 LLM
  labelling inherit the parent's two-key, opt-in, key-presence-is-
  not-consent discipline.
