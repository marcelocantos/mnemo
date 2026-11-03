# Oh My Pi Transcript Ingest

*Status: design note — 2026-10-26. Grounded in omp v18.12.1 session
JSONL on disk (`~/.omp/agent/sessions`), `omp --help`, and upstream
`docs/session.md` (session format version 3). Sibling of Cursor ingest
(`docs/design/cursor-ingest.md`).*

---

## TL;DR

Oh My Pi (**omp**) does not follow Claude Code's schema. It also does
not have one schema per backend. A session is a single versioned JSONL
tree, and the provider is a field on the assistant message
(`provider`, `api`, `model`). The same file already holds Cursor-backed
Grok turns and Anthropic Claude turns. Extensions hang off the same
tree as `custom` (not in model context) or `custom_message` (in
context). Compaction is a node in that tree, not a side file.

mnemo indexes that tree directly (`session_meta.source = 'omp'`,
resume `omp --resume <id>`). It does not adopt the v3 schema as its own
storage format. The lesson it does take is about layers: the entry log
is the catch-all, and searchable messages are a projection of it.

---

## Why this matters inside mnemo

mnemo already has two layers, but each new agent has been taught as a
transform that *replaces* the foreign log with Claude-shaped messages.
The raw line is kept, then anything the parser does not recognise is
never written. That is fine for a closed schema. It is the wrong default
for a format whose own reader is specified as:

```text
switch entry.type:
  message          → model context
  custom_message   → model context, from an extension
  custom           → extension state, not model context
  default          → preserve, do not guess
```

omp is the first source where that split is the product, not an
accident. One session is not one provider. On disk, assistant turns
are `provider=cursor` / `api=cursor-agent` and `provider=anthropic` /
`api=anthropic-messages` in the same file, with different models and
different usage shapes (`cacheWrite` versus Anthropic `cttl`).
`session_meta.source` stays `omp` — the agent that wrote the file.
The provider stays on the entry (`message.provider`). Putting the
backend into `source` would make a mixed session unrepresentable, and
it would double-count a Claude turn that omp imported with
`--from-claude` next to the native Claude ingest.

The tree is the other half. Every record after the header has `id` and
`parentId`. A compaction node carries `summary`, `firstKeptEntryId`,
`tokensBefore`, and `tokensAfter`. That is the same shape as mnemo's
compacted view: a durable summary plus the id where the still-live tail
begins. mnemo's compactor writes that shape into the `compactions`
table from the outside. omp has already written it into the transcript.
`session_chains` is the inter-file version of `parentId`. Neither is
replaced by this ingest. The omp node is stored so a later compacted
read can use the transcript's own cursor instead of inventing one.

What mnemo does not do: store session v3 as the canonical row format,
or route Claude and Codex ingest through `omp --from-claude` /
`--from-codex`. Those agents already have native parsers. Importing
them again through omp would index the same conversation twice.

---

## The format

```
~/.omp/agent/sessions/<encoded-cwd>/<timestamp>_<session-id>.jsonl
```

Version 3. A fixed-width `title` record may sit in front of the
`session` header (`id`, `cwd`, `timestamp`). The encoded directory name
is lossy; the header `cwd` is authoritative.

| Record | Entry | Message projection |
|---|---|---|
| `session`, `model_change`, `title`, `title_change` | kept | none (cwd, model, topic are session metadata) |
| `message` role `user` / `assistant` | `user` / `assistant` | text, `thinking` (noise), `toolCall` |
| `message` role `toolResult` | `user` | `tool_result`, joined by `toolCallId` |
| `message` role `developer` | `user` | synthetic resume prompt, noise |
| `compaction`, `branch_summary` | kept under that type | `[omp compaction]` plus the summary. `firstKeptEntryId` stays on the raw entry |
| `custom_message` | kept | text, noise. This is in-context extension text |
| `custom` (`tool_execution_start`, `session_exit`, …) | kept | none. The assistant `toolCall` is the call; projecting the start event would index it twice |
| anything else (`label`, `mode_change`, `credential_pin`, `ttsr_injection`, a type this version has never seen) | kept under that type | none |

`credential_pin` stores a hash, not a secret. It is still not projected
into search.

Assistant `message.usage` uses `input` / `output` / `cacheRead` /
`cacheWrite`. Ingest copies those onto Claude's
`input_tokens` / `output_tokens` / `cache_read_input_tokens` /
`cache_creation_input_tokens` so the entries generated columns fill in.
Provider-specific extras (Anthropic `cttl`, cost breakdowns) stay on
the raw message. Per-message `model` wins over the session
`model_change`, so a mixed-provider file does not bill every turn as
the first model. Tool arguments use `path` and `command`; the shared
normaliser rewrites `path` to `file_path`.

`toolCallId` values contain an embedded newline between two id forms.
Ingest joins them with `|` so the call and the result still match.

---

## Roots

`OmpRootsFor` yields:

- `$PI_CODING_AGENT_DIR/sessions` when that env is set, otherwise
  `$OMP_HOME/agent/sessions` or `~/.omp/agent/sessions`
- `$OMP_HOME/profiles/<name>/agent/sessions` (or `~/.omp/profiles/...`)
  for profiles that exist when the daemon opens the store

`recent-sessions.jsonl` under `~/.omp/agent/cache/` is not a transcript.
Only `sessions/<encoded>/<timestamp>_<uuid>.jsonl` is parsed. No schema
change: `session_meta.source` is an unconstrained string, and the
provider is not a column.

---

## Resume

`omp --resume <id>` resumes by id prefix, path, or picker (`omp --help`,
v18.12.1). mnemo passes the filename uuid, which matches the session
header `id`. `--restore` is not used: it continues unfinished work
rather than simply reopening the conversation.

---

## T3 Code

T3 Code does not get its own source. It is an ACP host. The
conversation is written by the provider it drives, and mnemo already
indexes those stores:

- Grok sessions land in `~/.grok/sessions/` (`session_meta.source = 'grok'`).
  A T3 thread on the Grok harness is one of those sessions. `GROK_OAUTH2_REFERRER=t3code`
  is how the process was launched, not a second transcript.
- Claude, Cursor, and Oh My Pi sessions stay on their own trees.

`~/.t3/userdata/logs/provider/events.*.log` is an ACP method trace
(`initialize`, `session/new`) with field counts and no message text.
Indexing it would not recover a conversation the provider store does
not already hold.
