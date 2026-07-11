# Grok Transcript Ingest (MVP)

*Status: design note — 2026-07-11. Anchors 🎯T110. Scope is deliberately
narrow: capture Grok CLI session transcripts into the existing index,
and not much else.*

**Tracking.** Design source for 🎯T110. Grounded in real session trees
on disk (`~/.grok`, Grok CLI 0.2.93) and the shipped user guide
(`~/.grok/docs/user-guide/17-sessions.md`). Sibling of 🎯T99 /
`docs/design/codex-ingest.md`.

---

## TL;DR

Grok **does not follow Claude Code's schema.** Sessions live as
**directories** under `~/.grok/sessions/<url-encoded-cwd>/<session-id>/`,
with an ACP-style `updates.jsonl` as the durable conversation log and a
`summary.json` for session metadata.

The MVP is a defensive parser that maps that stream into mnemo's
existing content model, plus one new watched root, one synthetic-id
rule, and `session_meta.source = 'grok'`. Everything else — search,
repo attribution, idempotent resumable ingest — is reused from the
Claude/Codex spine.

MCP connectivity (streamable HTTP) already works; this target is
**ingest only**.

---

## The format

### Layout

```
~/.grok/sessions/<url-encoded-cwd>/<session-id>/
  summary.json       # metadata: title, cwd, git, model, timestamps
  updates.jsonl      # ACP session/update stream (authoritative, append-only)
  chat_history.jsonl # model-facing messages (rewritten on compact — lossy)
  events.jsonl       # telemetry — skip
  …
```

`GROK_HOME` overrides `~/.grok`. When the encoded cwd exceeds 255 bytes,
Grok uses a slug+hash and stores the original path in a `.cwd` file
(see the user guide); `summary.json` still carries `info.cwd`.

### `summary.json`

```json
{
  "info": { "id": "<uuidv7>", "cwd": "/abs/path" },
  "generated_title": "…",
  "session_summary": "…",
  "current_model_id": "grok-4.5",
  "git_remotes": ["git@github.com:org/repo.git"],
  "head_branch": "…",
  "head_commit": "…",
  "created_at": "…", "updated_at": "…", "last_active_at": "…",
  "parent_session_id": "…"   // forks only
}
```

### `updates.jsonl`

Every line:

```json
{
  "timestamp": 1783679368,
  "method": "session/update",
  "params": {
    "sessionId": "<uuid>",
    "update": { "sessionUpdate": "<type>", … }
  }
}
```

`method` may also be `_x.ai/session/update` for Grok extensions
(hooks, goals, subagent lifecycle, compact markers). Those are skipped
in the MVP.

| `sessionUpdate` | → mnemo | MVP |
|---|---|---|
| `user_message_chunk` | user text | ✅ |
| `agent_message_chunk` | assistant text | ✅ |
| `agent_thought_chunk` | thinking | ✅ |
| `tool_call` | tool_use (`toolCallId`, name, `rawInput`) | ✅ |
| `tool_call_update` + `status: completed` | tool_result | ✅ |
| other `tool_call_update` | intermediate UI | ❌ skip |
| `subagent_*`, `goal_*`, `auto_compact_*`, `session_recap`, `plan`, hooks | bookkeeping | ❌ skip |

Observed chunk sizes are whole messages (median ~100 chars), not token
streams — each chunk is one entry.

### Why not `chat_history.jsonl`?

It is the model-facing view and is **rewritten on compact**, dropping
pre-compaction user turns. `updates.jsonl` is append-only and is what
drives `/resume`. Index the durable stream.

Other `.jsonl` siblings (`events.jsonl`, `rewind_points.jsonl`, …) must
**not** be fed to the Claude parser. Discovery only selects
`updates.jsonl` under the Grok root.

---

## MVP — four adaptations, everything else reused

1. **New watched root.** Discover/watch `$GROK_HOME/sessions` (default
   `~/.grok/sessions`) recursively. Only `updates.jsonl` is ingested.
2. **Synthetic record id.** No per-line uuid. Id =
   `grok-<session_id>-<byte-offset>`. Append-only → re-ingest dedups via
   `INSERT OR IGNORE`; resume from `ingest_state` offset.
3. **`source = 'grok'`.** Reuses the additive `session_meta.source`
   column introduced for Codex (default `'claude'`).
4. **Envelope → content transform.** Unwrap `params.update`; map the
   handful of `sessionUpdate` types above; ignore unknowns. Metadata
   (cwd, branch, title) comes from sibling `summary.json`.

Repo attribution uses `summary.info.cwd` via the existing `extractRepo`
path logic (`git_remotes` is free context but not required for MVP).

---

## Parser posture

No schema-version field. Map the core `sessionUpdate` types and
**ignore unknown types** rather than exhaustively modelling every
variant. Unknown → skip-and-continue, never fail the file.

---

## Explicitly out of scope (MVP)

`events.jsonl` telemetry; rewind snapshots; images; Grok's own
`session_search.sqlite`; decryption of anything; modelling forks /
subagents as mnemo chains (subagent child sessions are normal sibling
session dirs and get indexed independently); `mnemo_whatsup` live-PID
for `grok` processes; Grok-native memory; usage/token streams from
`signals.json`.

**Just text + tool calls + tool results, attributed to a session / repo
/ timestamp, searchable.**

---

## Cross-check references

- `~/.grok/docs/user-guide/17-sessions.md` (storage layout, resume)
- Live corpus under `~/.grok/sessions/` (CLI 0.2.93)
- Implementation mirror: `internal/store/codex.go` (🎯T99)
