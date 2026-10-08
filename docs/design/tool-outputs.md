# Persisted tool output (🎯T192)

## Problem

Claude Code persists any tool result over about 2 KB to a sidecar file
(`<project>/<session>/tool-results/<id>.txt`, or
`mcp-<server>-<tool>-<ts>.txt` for MCP tools; a few are json, pdf or
jpg) and writes a `<persisted-output>` preview into the transcript in
its place. mnemo's `jsonlEntry` had no `toolUseResult` field and
`extractBlocks` read only `message.content`, so only the preview was
indexed. On the owner's store 4,674 preview rows existed on 2026-10-08,
each hiding a body the search could not see. 🎯T29 was marked achieved
on this false premise: the tool it added returned the preview and
called it the raw body.

## Shape of the data

`toolUseResult` is not always the full body. Observed shapes:

| Tool | toolUseResult |
|---|---|
| Bash | `stdout` capped near 30,000 chars, `stderr`, `interrupted`, `isImage`, `noOutputExpected`, `persistedOutputPath`, `persistedOutputSize` |
| Grep / Glob | `content`, `filenames`, … |
| WebFetch | `bytes`, `code`, `result`, `url` |
| MCP tools | array of `{type: "text", text}` blocks |
| about 12% | absent |

The sidecar path comes from `persistedOutputPath` or from the preview's
"Full output saved to:" line. It is never constructed from the
tool_use_id: the naming differs per tool class.

## Design

- **A table, not a rewrite of `messages.text`.** `mnemo_read_session`
  and the compactor read that column; a body of megabytes per tool
  call would bloat both. `tool_outputs(id, message_id UNIQUE,
  session_id, project, tool_use_id, source, text, text_z, plain_len,
  z_len, indexed_at)` plus `tool_outputs_fts(text, session_id)` and
  insert/delete triggers mirroring `messages_ai`. Additive only, so it
  applies under sqlift `AllowNone`. `CREATE TABLE` is not
  metadata-only, so the pre-migration backup runs on upgrade; it is
  logged as such.
- **Resolution order:** sidecar (text file, under
  `.claude/projects/**/tool-results/`, regular file, capped at 4 MiB)
  → string leaves of `toolUseResult` excluding metadata keys → nothing,
  recorded as `source = 'none'` so the preview is examined once.
- **Both ingest paths.** `parseFile` resolves in the parallel parse
  workers; `ingestFile` resolves inline. `insertMessage` now returns
  the row id so the body hangs off the preview without a lookup. A
  preview that belongs to one of mnemo's own tools is noise (🎯T190)
  and gets no body.
- **Search.** `tool_output` is in the default corpus set. Hits carry
  `msg:<id>`, the session, and the preview's timestamp; session_type
  and repo filters apply through the session. Calibration samples the
  decoded text like every other corpus.
- **Backfill.** A startup phase behind `CapSchemaCurrent` and
  `CapCodecReady` lists preview messages through
  `messages_fts MATCH '"persisted output" AND "full output saved to"'`
  with no `tool_outputs` row, reads each entry's stored line from
  `entries_v.raw`, resolves, and inserts `OR IGNORE` in batches of 25
  with a yield between commits. Idempotent across runs through the
  unique key; a second pass finds no candidates.

## Proof

- Unit: sentinel past the first 2 KB found via bulk and realtime
  ingest; sidecar-only; toolUseResult fallback; neither source keeps
  the preview searchable; migration from a pre-🎯T192 schema fills the
  table from stored entries; second backfill inserts nothing;
  session_type and repo filters; sidecar path outside the allowed tree
  refused; binary sidecar falls back; 4 MiB cap.
- Live: `SELECT source, COUNT(*) FROM tool_outputs GROUP BY source`
  against the preview count, upgrade and backup duration from the
  daemon log, and a search for a word that only the body carries.
