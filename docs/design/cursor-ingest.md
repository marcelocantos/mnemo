# Cursor Transcript Ingest

*Status: design note — 2026-08-23. Anchors 🎯T149 (MVP) + 🎯T149.1
(fidelity). Grounded in real agent-transcript JSONL on disk
(`~/.cursor/projects`), Agent CLI `store.db` under `~/.cursor/chats`,
and `agent --help` on this machine. Sibling of 🎯T99 / `codex-ingest.md`
and 🎯T110 / `grok-ingest.md`.*

---

## TL;DR

Cursor Agent **does not follow Claude Code's schema.** Sessions live as
JSONL files under

```
~/.cursor/projects/<encoded-cwd>/agent-transcripts/<uuid>/<uuid>.jsonl
```

Each line is a `{role, message:{content:[...]}}` envelope (or a
`turn_ended` trailer). There is no per-line uuid, no parentUuid tree,
and no cwd on the line.

The MVP is a defensive parser that maps that stream into mnemo's
existing content model, plus one new watched root, one synthetic-id
rule, `session_meta.source = 'cursor'`, and resume via
`agent --resume <id>`. Fidelity (🎯T149.1) additionally watches
`~/.cursor/chats/**/store.db` for tool results and session meta.
Everything else — search, repo attribution, idempotent resumable
ingest — is reused.

---

## The format

### Layout

```
~/.cursor/projects/<hyphen-encoded-cwd>/
  agent-transcripts/<uuid>/<uuid>.jsonl   # durable conversation
  worker.log                             # workspacePath=… (cwd)
  repo.json                              # opaque workspace id — not used
  agent-tools/                           # skip
```

`CURSOR_HOME` overrides `~/.cursor` (for tests and relocated installs).

The project-directory encoding maps both `/` and `.` to `-`, so it is
lossy. Cwd is resolved in order:

1. `worker.log` `workspacePath=` — present on a minority of sessions.
2. Invert the slug (`-` → `/`, then `/github/com/` → `/github.com/`)
   **only if that path exists as a directory**. Hyphenated path
   components (`squz-multimaze2`) cannot be reconstructed and stay
   empty rather than inventing `squz/multimaze2`.

```
[info] Getting tree structure for workspacePath=/Users/…/repo
```

### JSONL

Typical lines:

```json
{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>…</timestamp>\n<user_query>\n…\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"…"},{"type":"tool_use","name":"Read","input":{"path":"…"}}]}}
{"type":"turn_ended","status":"aborted","error":"…"}
```

| Record | → mnemo | MVP |
|---|---|---|
| `role=user` text | user text; inner `<user_query>` only | ✅ |
| `role=assistant` text | assistant text | ✅ |
| `tool_use` / `tool_call` | tool_use (`normalizeAgentToolInput`) | ✅ |
| `tool_result` / `role=tool` | tool_result | ✅ |
| `thinking` / `reasoning` | thinking (noise) | ✅ |
| `turn_ended` | — | ❌ skip |
| `<environment_context>` / `<system_reminder>` / `<timestamp>` wrappers | — | ❌ skip |
| unknown / malformed JSON | — | ❌ skip-and-continue |

Verified on a live mnemo Cursor session: user turns are wrapped in
`<timestamp>` + `<user_query>`; assistant tool_use sits in the same
content array as text; a session may end with `turn_ended`.

---

## MVP — four adaptations, everything else reused

1. **New watched root.** Discover/watch `$CURSOR_HOME/projects`
   (default `~/.cursor/projects`) recursively. Only
   `agent-transcripts/<id>/<id>.jsonl` is ingested.
2. **Synthetic record id.** No per-line uuid. Id =
   `cursor-<session_id>-<byte-offset>`. Append-only → re-ingest dedups
   via `INSERT OR IGNORE`; resume from `ingest_state` offset.
3. **`source = 'cursor'`.** Reuses the additive `session_meta.source`
   column introduced for Codex (default `'claude'`).
4. **Envelope → content transform.** Unwrap `message.content`; strip
   user-query XML; ignore unknowns. Metadata (cwd) comes from sibling
   `worker.log`.

Repo attribution uses `extractRepo(cwd)` from that workspace path.

---

## Fidelity (🎯T149.1)

Cursor Agent CLI splits what Claude/Codex/Grok keep in one durable log.
JSONL under `agent-transcripts` has `tool_use` but **zero** `tool_result`
on the live tree. The CLI's source of truth is:

```
~/.cursor/chats/<md5(cwd)>/<uuid>/store.db   # blobs + meta
~/.cursor/chats/<md5(cwd)>/<uuid>/meta.json  # cwd, title
```

Session UUIDs are 1:1 with the JSONL transcripts. This layer does **not**
re-index user/assistant text (JSONL already does); it fills the parity
gaps.

| Cursor signal | → mnemo | Notes |
|---|---|---|
| `blobs` JSON `role=tool` / `tool-result` | `messages.content_type='tool_result'` | id `cursor-<session>-<blobId>`; `toolCallId` → `tool_use_id` when present |
| `meta.json` `cwd` | `session_meta.cwd` | Preferred over `worker.log` / slug (fixes hyphenated repos) |
| `meta.json` `title` | `session_meta.topic` | Used until a real `<user_query>` arrives |
| store `meta` `lastUsedModel` | `entries.model` (via `message.model`) | Same stamp as Grok T111 / Codex T112 |
| `git_branch` | — | Cursor does not record branch in meta.json or store meta; absence is documented, not faked |
| `role=system` / binary DAG / `blobEncryptionKey` | — | skip (noise / resume-graph internals) |
| `role=user` / `role=assistant` in store.db | — | skip (would double-index JSONL) |

**ACP `~/.cursor/acp-sessions/<id>/store.db`.** Same schema, **0** id
overlap with chats on observed machines. Excluded from discovery
(`CursorChatRootsFor` returns only `chats/`; `isCursorStore` rejects
acp paths). Locked by `TestCursorACPSessionsExcluded`. Not an
`agent --resume` surface for the JSONL corpus.

**Still out of scope:** `~/Library/Application Support/Cursor/.../state.vscdb`
composerData (0 id overlap with Agent CLI, no `agent --resume` — analogue
of Codex Desktop).

Incremental ingest: store.db is not an append log. On size growth the
whole DB is re-read; `INSERT OR IGNORE` on synthetic blob ids keeps
re-ingest idempotent. WAL/SHM sidecars are not watched.

Live oracle:

```bash
MNEMO_CURSOR_CHATS=$HOME/.cursor/chats go test -tags sqlite_fts5 \
  -run TestCursorLiveChatsCorpus ./internal/store/
```

---

## Resume (🎯T149, same target)

`agent --help` documents `--resume [chatId]`. The binary on PATH is
`agent` (Cursor Agent CLI), not the `cursor` editor shim (which is a
VS Code `code` fork). mnemo's session ids are the transcript-directory
UUIDs, which are the chatIds. ResumeCommand returns
`agent --resume <id>` and the existing sessiongo path `cd`s into the
recorded cwd first.

---

## Still out of scope

- `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`
  — desktop composer KV (see Fidelity for chats/store.db, which is now in scope as 🎯T149.1)
- Reconstructing cwd from the hyphen-encoded project directory name
  alone (lossy: both `/` and `.` become `-`). Prefer `meta.json` cwd.

`~/.cursor/chats/.../store.db` and `meta.json` are **in scope as of
🎯T149.1** (tool results + cwd/title/model). ACP `acp-sessions` remain
excluded (see Fidelity).

---

## Cross-check references

- Live corpus under `~/.cursor/projects/*/agent-transcripts/`
- Live chats under `~/.cursor/chats/*/*/store.db`
- `agent --help` (`--resume [chatId]`)
- Grok resume-session reader (`session_reader.py` cursor path)
- Implementation: `internal/store/cursor.go`, `internal/store/cursor_store.go`
- Mirrors: `internal/store/grok.go` (🎯T110/T111), `internal/store/codex.go` (🎯T99/T112)
- Live oracle: `MNEMO_CURSOR_CORPUS=$HOME/.cursor/projects go test -tags sqlite_fts5 -run TestCursorLiveCorpus ./internal/store/`
- Live chats oracle: `MNEMO_CURSOR_CHATS=$HOME/.cursor/chats go test -tags sqlite_fts5 -run TestCursorLiveChatsCorpus ./internal/store/`
