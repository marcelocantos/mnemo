# Per-row text compression (🎯T151)

## Why

`mnemo.db` was 34 GB. `dbstat` put `entries` at 17.4 GB, `messages` at
6.7 GB, `docs` at 1.3 GB, `messages_fts_data` at 1.9 GB, and about 2 GB
of indexes on `entries`. `entries.raw` is already JSONB, which is a parse
cache rather than a compression format — on this corpus JSONB is the
same size as the text because the payload is long string values, not
structure. The compressible mass is `messages.text` (5.3 GB) and
`docs.content` (1.3 GB).

Measured on a random 20k-row sample of the live index:

| Method                                       | messages.text | docs |
|----------------------------------------------|---------------|------|
| whole-stream zstd -3                         | 0.26          | 0.30 |
| whole-stream zstd -19                        | 0.21          | 0.22 |
| per-row zlib -6                              | 0.40          |      |
| per-row zstd -3, no dictionary               | 0.39          |      |
| per-row zstd -3 + 113 KB trained dictionary  | **0.26**      |      |

Per-row is what a column store needs (random access by id), and per-row
without a dictionary loses a third of the gain because every row relearns
the same transcript vocabulary. A dictionary trained on ~3k rows closes
that gap entirely. PostgreSQL's TOAST (pglz/lz4, per value, no
dictionary) would land on the dictionary-less line, so this stays in
SQLite.

`messages.tool_input` (384 MB) is excluded: fifteen generated columns
read it with `->>`, and generated columns cannot see through a
compressed blob.

## Shape

Everything is additive, per the append-only schema policy (sqlift
`AllowNone`):

- `messages.text_z BLOB`, `docs.content_z BLOB` — the zstd frame. When
  set, the legacy column holds `''`. Both columns are appended *last* in
  the table definition so `ALTER TABLE ADD COLUMN` reproduces the order
  exactly; a column inserted mid-table makes sqlift plan a rebuild,
  which `AllowNone` refuses.
- `compression_dicts (dict_id UNIQUE, family, created_at, sample_rows,
  dict)` — every dictionary ever trained. Never deleted.
- `compression_gc (family PRIMARY KEY, next_id, done, saved_bytes, …)` —
  the backfill cursor.
- `messages_v`, `docs_v` — views with the same columns as the base tables
  and the text decoded. The documented surface for ad-hoc SQL.
- Trigger bodies for `messages_ai`, `docs_ai/au/ad` now feed FTS with
  `mnemo_text(new.text, new.text_z)` (resp. `old.*`), so the index sees
  plaintext and a rewrite removes the right tokens.

`mnemo_text(plain, z)` is a Go function registered on every connection
mnemo opens (`store.SQLiteDriverName` for the writer, the read-only
driver for the pool). `z` NULL or empty returns `plain`; otherwise the
frame is decoded. The zstd frame header carries the dictionary id, so a
blob is self-describing: the process-wide registry holds every
dictionary from `compression_dicts`, and a retrain never invalidates
older rows.

A consequence: any connection *without* the function cannot insert into
`messages` or `docs` (the triggers call it — and SQLite resolves function
names when the trigger is compiled, so a `CASE` around the call would
not help) and sees `''` when reading the base columns directly. That
includes an **older mnemo binary**: once a database has been migrated
by 0.89+, a downgrade keeps reading but stops ingesting. Do not run a
development build against `~/.mnemo/mnemo.db` before the release that
ships it. The `sqlite3` CLI and `sqldeep` can still
read every other table and the base columns of these two; anything that
needs the text goes through mnemo.

## Writing

`textCodec.pack(family, text)` returns `(text, nil)` when the row is under
64 bytes, when compression does not pay, or when the codec is not ready;
otherwise `("", frame)`. Readiness matters on an upgrade boot: 🎯T114.1
serves on the *old* schema while the pre-migration backup and
`sqlift.Apply` run in the background, and `text_z` does not exist yet.
The writer prepares the legacy INSERT until the upgrade lands, then
switches. Readers that reference `text_z` fail with "no such column"
during that window — the same posture every column added since T114.1
has, and it lasts as long as the backup of the index takes.

Dictionaries: a fresh install writes dictionary-less frames. Once a
family has 2,000 rows and no dictionary, the store trains one at boot
(`autoTrainDictionaries`). Training is a random sample of 3,000 rows
read through `mnemo_text` (so compressed rows count), concatenated as
the dictionary content and passed to `zstd.BuildDict` for the entropy
tables. Measured against `zstd --train` on the same sample: 0.267 vs
0.254, close enough that a COVER pass is not worth a subprocess. No
dictionary ships in the binary: one trained on this corpus would embed
fragments of private transcripts in a public artefact.

Retraining is explicit (`mnemo_ops op=compress_train`) and only affects
new rows; `op=compress_gc` repacks history under whatever dictionary is
active when it visits a row.

## Reading

Every SQL literal in the tree that selects from `messages` or `docs`
goes through `mnemo_text(col, col_z)`. A ratchet test
(`compress_readers_test.go`) parses every non-test Go file, finds string
literals that query either base table, and fails on a bare `text` /
`content` reference — a missed reader would not error, it would return
`''` for every new row, which is the failure mode worth a permanent
test. Writers, the FTS shadow tables and aliased subquery columns are
exempt.

`mnemo_query` documents `messages_v` and `docs_v` as the tables to read
text from.

## Backfill (phase 3 GC)

`CompressBackfill` walks the table in id order from the persisted
cursor in batches of 2,000 rows, one transaction each. For each plain
row that pays: encode, decode, compare bytes, and only then
`UPDATE … SET col = '', col_z = ?`. A mismatch halts the run with the
row id. `messages` has no UPDATE trigger, so FTS is untouched; `docs`
does, and the rewritten tokens are identical, so the index converges.

The cursor survives restarts; a second run resumes at the last committed
batch, and a run over a finished table visits nothing. Progress is in
`op=compress_status`. Space comes back only with `VACUUM`, which needs
free disk equal to the database and blocks writers while it runs; it is
left to the operator.

## Ops

```
mnemo_ops op=compress_status
mnemo_ops op=compress_train family=messages|docs
mnemo_ops op=compress_gc    family=messages|docs [wait=true]
```

`compress_gc` runs in the background by default because a full pass
over 5M rows runs for minutes, past any MCP call budget.

## Measured on the live corpus

`TestCompressLiveCorpus` (build tag `compresslive`) against an online
copy of the 33.7 GB index, 2026-08-29:

| | before | after | ratio |
|---|---|---|---|
| `messages` (dbstat) | 7.01 GB | 3.28 GB | 47% |
| `docs` (dbstat) | 1.40 GB | 0.26 GB | 19% |
| file after VACUUM | 33.68 GB | 28.67 GB | 85% |

3,554,001 message rows visited, 2,813,393 compressed (the rest are under
64 bytes or did not pay), 3.55 GB saved in 2m19s; docs 1.12 GB saved in
1m57s; 20,000 sampled compressed rows all decode; VACUUM 7m15s. The
`messages` table lands at 47% rather than the 26% text ratio because
the row also carries `tool_input` (384 MB), ids, timestamps and session
ids — fixed per-row cost compression does not touch.

The file as a whole only drops 15% because `entries.raw` (17.4 GB of
JSONB, plus ~2 GB of indexes on its generated columns) is untouched.
That is the next lever, and a different job: the generated columns
would have to be materialised at ingest before `raw` can be compressed
(🎯T152).

## entries.raw (🎯T152)

`entries.raw` is the JSON line of every transcript record, stored as
JSONB — 17.6 GB, half the file, and JSONB is 0.98 of the text size, so
it was never a compression format. Measured on 4,500 random rows:
per-row zstd 0.395 without a dictionary, **0.321 with**.

The obstacle is not the codec but the sixteen generated columns
(`uuid`, `model`, token counts, `agent_id`, `slug`, `is_sidechain`, hook
fields, tool-use ids) that read `raw` with `->>`, and the twelve indexes
over them. A generated column cannot see through a compressed blob, and
the append-only policy cannot redefine or drop them. So:

- Sixteen **materialised twins** (`uuid_m`, `model_m`, …) are real
  columns, written at ingest from the bound JSON text (`?5->>'$.uuid'`
  — numbered parameters let one bound value feed every expression) and
  copied from the generated columns for historical rows.
- Their indexes are duplicated (`idx_entries_*_m`), including the
  UNIQUE `(session_id, uuid_m)` that `INSERT OR IGNORE` deduplicates on.
- **`entries_v`** exposes the base columns, `mnemo_raw(raw, raw_z)` as
  `raw`, and the sixteen fields under their *original* names from the
  `_m` columns. It is a plain projection, so SQLite flattens queries
  onto the `_m` indexes. Readers changed one token — `entries` →
  `entries_v` — and the ratchet flags any `FROM|JOIN entries` literal
  that touches `raw` or a generated column.
- The sentinel for a compressed row is **`raw = NULL`**, not `''`: a
  NULL makes every generated column NULL, whereas `''` makes them raise
  "malformed JSON" and the insert fails.
- `mnemo_raw` passes a JSONB blob through and decodes a frame to JSON
  text; every reader hands the result to a `json_*` function, which
  accepts either.

### Ordering, and why the boot pass exists

Readers source the fields from `_m` exclusively, so a row without them
reads NULL — wrong usage totals, a missing model. Historical rows get
theirs from a **boot-time materialisation pass** (`entries.fields` in
`compression_gc`): id-ordered batches of `UPDATE … SET uuid_m =
COALESCE(uuid_m, uuid), …`, resumable, automatic once compression is
ready. Until it reports done, two things hold back:

- `op=compress_gc family=entries` is refused, and
- the writer keeps storing `raw` as JSONB even though it also writes
  `_m`, because `INSERT OR IGNORE` must keep its `(session_id, uuid)`
  key against rows that have no `uuid_m` yet.

After it, new rows write `raw = NULL, raw_z = frame`, and the GC's
UPDATE materialises and clears in one statement (RHS values are the
pre-update row).

`messages.tool_input` (0.38 GB behind fifteen `tool_*` generated
columns) is deliberately left alone: 1% of the file does not justify
fifteen more materialised columns.

### Measured on the live corpus (🎯T152)

`TestCompressLiveCorpus` on an online copy of the post-🎯T151 index
(29.63 GB), 2026-08-29:

| | before | after | ratio |
|---|---|---|---|
| `entries` (dbstat) | 19.37 GB | 7.17 GB | 37% |
| file after VACUUM | 29.63 GB | 18.69 GB | 63% |

5,063,388 rows visited, 5,057,202 compressed, 11.42 GB saved in 7m07s;
the boot-time field pass ran inside the upgrade window; 20,000 sampled
rows agree between decoded `raw` and `uuid_m`; VACUUM 3m03s. Together
with 🎯T151 the index went from 34 GB to 18.7 GB.
