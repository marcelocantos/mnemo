# Grok Transcript Ingest

*Status: design note — 2026-07-11. Anchors 🎯T110 (MVP) + 🎯T111
(fidelity). Grounded in real session trees on disk (`~/.grok`, Grok CLI
0.2.93+) and the shipped user guide (`17-sessions.md`). Sibling of
🎯T99 / `docs/design/codex-ingest.md`.*

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

## Fidelity layer (🎯T111)

Beyond the MVP conversation core, Grok often carries **richer** session
metadata than Claude's per-line envelope. We map those into the same
columns/tables Claude already uses where possible, and index the rest
as searchable text.

| Grok signal | Mapping | Beyond Claude? |
|-------------|---------|----------------|
| `summary.current_model_id` | `entries.raw.message.model` → generated `entries.model` | fills same column |
| `summary.session_kind=subagent` | `project=subagents` → `session_summary.session_type` | same classification path |
| `summary.parent_session_id` | `session_chains` edge (`mechanism=grok_parent`) | explicit parent without MCP identity |
| `summary.git_remotes` | `session_meta.repo` fallback when cwd is opaque/worktree | Claude usually has path-based cwd |
| `signals.json` context tokens | synthetic assistant entry with `input_tokens` + `[grok signals]` text | session-level occupancy (no Anthropic turn split) |
| tool `target_file` / `path` | normalised to `file_path` for `tool_file_path` gen-col | shared `normalizeAgentToolInput` (Codex too) |
| `session_recap` | assistant text `[grok recap]` | durable prose summary without mnemo compact |
| `plan` ACP updates | assistant text `[grok plan]` checklist | first-class plan stream |
| `goal_updated` | assistant text `[grok goal]` | no Claude analogue |
| `subagent_spawned/finished` | text + `session_chains` (`grok_subagent`) | parent→child graph |
| `task_completed` | tool_result-shaped text with exit/cmd/output | bg task completion |
| `auto_compact_*` | thinking noise with token before/after | Grok-native compact markers |

Codex reuses tool-input normalisation and, as of 🎯T112, the same
session_type / model / usage / parent-chain extension points
(`docs/design/codex-ingest.md` fidelity layer).

## Still out of scope

`events.jsonl` raw telemetry flood; rewind snapshots; image pipeline
(assets/images dirs → OCR/describe); `mnemo_whatsup` for `grok` PIDs;
Grok-native memory store; decrypting reasoning beyond visible summaries.

---

## Cross-check references

- `~/.grok/docs/user-guide/17-sessions.md` (storage layout, resume)
- Live corpus under `~/.grok/sessions/` (CLI 0.2.93+)
- Implementation: `internal/store/grok.go`, `tool_input.go`
- Mirror: `internal/store/codex.go` (🎯T99)
