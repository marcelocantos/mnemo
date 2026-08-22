# Cursor Transcript Ingest

*Status: design note — 2026-08-23. Anchors 🎯T149. Grounded in real
agent-transcript JSONL on disk (`~/.cursor/projects`) and
`agent --help` on this machine. Sibling of 🎯T99 / `codex-ingest.md`
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
`agent --resume <id>`. Everything else — search, repo attribution,
idempotent resumable ingest — is reused.

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

## Resume (🎯T149, same target)

`agent --help` documents `--resume [chatId]`. The binary on PATH is
`agent` (Cursor Agent CLI), not the `cursor` editor shim (which is a
VS Code `code` fork). mnemo's session ids are the transcript-directory
UUIDs, which are the chatIds. ResumeCommand returns
`agent --resume <id>` and the existing sessiongo path `cd`s into the
recorded cwd first.

---

## Still out of scope

- `~/.cursor/chats/<md5(cwd)>/<uuid>/store.db` — CLI sqlite blobs
- `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`
  — desktop composer KV
- `~/.cursor/acp-sessions/<uuid>/store.db` — ACP session store
- Reconstructing cwd from the hyphen-encoded project directory name
  (lossy: both `/` and `.` become `-`)

Those are different formats. JSONL is the analog of Claude transcripts
and of Grok `updates.jsonl`.

---

## Cross-check references

- Live corpus under `~/.cursor/projects/*/agent-transcripts/`
- `agent --help` (`--resume [chatId]`)
- Grok resume-session reader (`session_reader.py` cursor path)
- Implementation: `internal/store/cursor.go`
- Mirrors: `internal/store/grok.go` (🎯T110), `internal/store/codex.go` (🎯T99)
