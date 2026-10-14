# Stemmed search, caller exclusion, and in-query filters

Three search changes ship together. The schema change is additive
(sqlift `AllowNone`): new tables, new indexes, new triggers. Existing
FTS tables, their tokenizers, and their trigger bodies are untouched.
Dropping the old indexes is a later phase.

## Calling session (T195)

`mnemo_search` leaves out the session that issued the call. The
exclusion is an `EXISTS` predicate in the FTS query, before `LIMIT`.
Filtering the top-N after the fetch loses the real hit once the
caller's own Bash, rg, and Read output fills the window.
`include_caller=true` opts back in.

mnemo does not otherwise know which transcript session is calling.
Claude Code's `tools/call` `params._meta` carries
`claudecode/toolUseId` and `progressToken`. It does not send the
transcript session id. `Mcp-Session-Id` is the MCP transport session
and is not used. `CLAUDE_CODE_SESSION_ID` is a stdio-only variable of
the client process, not of this daemon. The id is not taken from tool
arguments.

Resolution order, first hit wins:

1. `params._meta`, including one level of nested maps. Keys, in order:
   `claudecode/sessionId`, `claudecode/session_id`,
   `claude-code/sessionId`, `claude-code/session-id`,
   `claude/sessionId`, `cursor/sessionId`, `cursor/conversationId`,
   `codex/sessionId`, `codex/threadId`, `grok/sessionId`, `sessionId`,
   `session_id`. The outer `CallToolParams._meta` wins over the
   embedded request meta.
2. Headers: `X-Claude-Code-Session-Id`, `X-Claude-Session-Id`,
   `Claude-Code-Session-Id`, `X-Cursor-Session-Id`,
   `X-Cursor-Conversation-Id`.
3. W3C `Baggage` keys `session_id`, `session.id`, `claude.session_id`.
4. A tool-use id (`claudecode/toolUseId`, `claude-code/toolUseId`,
   `cursor/toolCallId`, `toolUseId`) looked up in `messages.tool_use_id`.
   This races ingest: a call that has not been indexed yet misses.

An id must match `^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`. Anything else
is ignored. When nothing identifies a session, nothing is excluded and
the tool result says so. The most recent session is not guessed.

## In-query filters (T196)

Session type, repo, and caller exclusion used to run after a fixed
candidate window (200 for messages, 50 for tool outputs). A window
full of subagent or worktree rows dropped the interactive hit.

Those predicates are now `EXISTS` lookups against `session_summary`
and `session_meta`, in the same `WHERE` as the `MATCH`, before
`LIMIT`. An empty filter is omitted so the unfiltered path stays a
plain FTS scan. Empty `session_type` on the unified path means
interactive, matching message search. `all` means no type predicate.

The same shape covers messages, tool outputs, and compaction
summaries. Topic segments take caller exclusion only; they were not
post-filtered by session type or repo. Other corpora have no session
dimension.

## Porter migration (T197)

Each searched corpus gains a second FTS5 table,
`tokenize='porter unicode61'`, with the same columns and
`content_rowid` as the unicode61 table. `tokenchars` is not set.
Hyphens stay separators.

`INSERT INTO fts(fts) VALUES('rebuild')` reads the content table.
Compressed rows store `''` in `text` / `content`, so rebuild would
index empty documents. Triggers and the backfill pass
`mnemo_text(text, text_z)` the same way the existing triggers do.

### Who is stemmed

Search corpora, plus compaction summaries (they are queried inside
message search, not as their own corpus): messages, tool_outputs,
topic_segments, decisions, docs, targets, git_commits, github_prs,
memories, plans, claude_configs, skills, audit_entries, compactions.

Left on unicode61, with the reason:

| Table | Why |
|---|---|
| `github_issues_fts`, `ci_runs_fts` | Not in `mnemo_search` |
| `snapshot_files_fts` | File paths; stemming changes identifiers |
| `image_ocr_fts` | Exact tokens (names, codes) |
| `image_descriptions_fts` | Image search only, not this tool |
| `todos_fts` | The todo tools are gone; nothing else reads the index |
| `notes_fts` | Note search is its own exact tool |
| `themes_fts`, `patterns_fts` | Clustering, not transcript search |

### Marker

`fts_stem_progress` holds one row per corpus: `marker_rowid`,
`initial_marker`, `status` (`running` or `complete`), `rows_done`.

`marker_rowid` is the lowest rowid known to be in the stem index.
Rows with `rowid >= marker` are answered by the porter table. Older
rows are answered by the unicode61 table. The backfill selects
eligible rows with `id < marker ORDER BY id DESC`, inserts them, and
sets the marker to the minimum id in the batch, in one write
transaction. Decompressed text is read before that transaction.

An external-content FTS table answers a bare `rowid` lookup from the
content table, so that probe cannot tell an indexed row from one
still below the marker. Membership is the FTS `%_docsize` shadow
table, which has one row per indexed document. Deletes (self-noise,
dedupe) use the same check: deleting a row the stem index never
received corrupts it.

When the select returns nothing, the corpus is marked `complete` and
the marker is set to 0. Search then uses only the porter table.
Completion still flips if calibration cannot be built (a tiny corpus
cannot meet the sample floor). Staying in split mode at marker 0
would score every hit at the neutral prior.

An empty database marks every corpus complete at startup, before any
rows exist. Later inserts are dual-written by the new triggers and
searched on the porter index. A store upgraded from a schema without
these objects backfills the rows that were inserted before the
triggers existed.

### Dual-write and split query

From the moment the new triggers exist, an insert goes into both
indexes. The legacy trigger bodies are not edited.

While a corpus is `running`, search issues two queries:

- porter, `relaxQueryStemmed`, `rowid >= marker`
- unicode61, `relaxQuery` (the existing last-token `*` stopgap), `rowid < marker`

Each row is in exactly one result. The two sides are not deduped, so
a duplicate is visible. They are merged on a per-side calibrated
quantile (`kind` for the legacy side, `kind#stem` for the porter
side). The stem distribution is refreshed about every 10% of the id
space and once at completion. A side with no usable distribution
scores at the neutral prior. After completion, search returns raw
BM25 from the porter table, which is how a finished store has always
ranked.

### The quoted-phrase star

`"..."`* on the last token of a quoted phrase stays on the unicode61
query only. Porter already folds inflections, including inside
phrases. On a stemmed index the star widens the last token past the
stem: `"the cat"*` matches `the catalog`, and porter does not stem
`catalog` to `cat`. Below the marker the star stays, so plurals still
match during the migration. That over-match on the legacy side is the
existing stopgap, kept until the old index is retired.

### Yield and size

Batches are 400 messages, 40 tool outputs, 100 docs, and 500 for the
other corpora. Text is decoded outside the write transaction. The
worker pauses about as long as the batch took (50% duty, floor 2 ms,
cap 5 s) and runs a `PASSIVE` checkpoint every 64 MiB. A test hook
can pause the pass after one batch; the marker is already committed,
and the next call continues.

`/health` is unchanged. The fast check `search.stem` reads the
worker's published snapshot and is always healthy. It runs no SQL and
adds no mode.

### Measured throughput

`TestStemBackfillThroughput` indexed 800 synthetic messages of about
1.3 KiB (`alpha beta gamma delta` repeated) with yielding and
calibration off:

- 34,691 rows/s, 23 ms
- disk delta 256 KiB, 328 bytes/row (page quantized)

At that rate, 4.95 million messages is about 2 min 23 s of full-duty
backfill, or about 4 min 45 s at the 50% duty the daemon actually
uses. Messages dominate; the other corpora add a smaller amount of
the same shape. Calibration is extra and is not in the timed section.

The disk figure is a lower bound. The synthetic text is four words
repeated, so the inverted index is unusually small. A real transcript
vocabulary will cost more per row. 328 bytes/row extrapolated is
about 1.6 GB for the message stem index; treat that as the floor, not
the expected size of a 33 GB store.
