# Codex Transcript Ingest

*Status: design note — 2026-06-30 (MVP 🎯T99); fidelity layer 2026-07-14
(🎯T112, parallel to Grok 🎯T111).*

**Tracking.** Design source for 🎯T99 + 🎯T112. Grounded in real rollout
files on disk (`~/.codex`) and source-verified against `openai/codex`
`codex-rs` rollouts.

---

## TL;DR

Codex **does not follow Claude Code's schema at all.** It records the
**OpenAI Responses API** item stream wrapped in a thin Codex envelope.
The MVP is a single defensive parser that maps that envelope into
mnemo's existing content model, plus one new watched root, one
synthetic-id rule, and one nullable `source` column. Everything else —
search, usage, repo attribution, idempotent resumable ingest — is reused
from the Claude ingest spine.

This is a genuine **transform**, not a passthrough, but a contained one.

---

## The format

Every rollout line is:

```json
{"timestamp": "2026-06-20T01:32:25.992Z", "type": "<envelope>", "payload": { ... }}
```

- **Location:** `~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ISO-ts>-<uuidv7>.jsonl`
  (date-nested), plus `~/.codex/archived_sessions/*.jsonl` (flat). Walk
  **recursively** — early Rust builds and archives are flat.
- **First line is always `session_meta`** (the header). Conversation
  content is in `response_item` lines.
- **No per-record `uuid`, no `parentUuid` tree, no per-line cwd/branch.**
  The only ids are the session uuidv7 (header + filename) and per-tool
  `call_id`s. Records are ordered purely by file position.
- It's the Rust `codex-rs` generation (`originator "Codex Desktop"`,
  `source vscode`, model `gpt-5.5`).

Envelope `type`s: `session_meta`, `response_item`, `event_msg`,
`turn_context`, `compacted`, `world_state`, `inter_agent_communication*`.

### Record taxonomy → mnemo mapping

| Codex record | content | → mnemo | MVP |
|---|---|---|---|
| `session_meta` | `id`, `cwd`, `git{commit,branch,repository_url}`, `model_provider`, `cli_version`, `source`, `forked_from_id`/`parent_thread_id` | session row (id, cwd→repo, git, started_at) | ✅ |
| `turn_context` | `model` (gpt-5.5), `cwd`, `workspace_roots` | per-turn model / cwd | ✅ light |
| `response_item / message` | `role` (developer/user/**assistant**), `content:[{input_text\|output_text\|input_image}]` | message: role + text | ✅ **canonical stream** |
| `response_item / function_call`·`custom_tool_call`·`tool_search_call`·`local_shell_call` | `name`, `arguments` (**JSON string**)/`input`, `call_id` | tool_use | ✅ |
| `response_item / *_output` | `call_id`, `output` (string **or** `content_items[]`) | tool_result (paired by `call_id`) | ✅ |
| `response_item / reasoning` | `summary[]`, `encrypted_content` (opaque) | thinking — **non-indexable placeholder** (index `summary` if present) | ⚠️ |
| `event_msg / token_count` | input/cached/output/reasoning tokens | usage | optional |
| `event_msg / user_message`·`agent_message` | clean text — **1:1 echo** of `response_item/message` | — | ❌ skip (avoid double-index) |
| `event_msg / patch_apply_end`·`task_*`·`mcp_tool_call_end` | diffs, turn boundaries, rich results | — | ❌ later |
| `compacted`·`turn_context`·`world_state`·`inter_agent_*` | bookkeeping | — | ❌ skip/ignore |

Verified on a real session: `response_item/message` carries all roles
(assistant 175, user 28, developer 6) and `event_msg/agent_message`
(175) is a 1:1 echo — so **`response_item` is the single canonical
conversation stream**; `event_msg` is UI echo plus token usage.

## MVP — four adaptations, everything else reused

1. **New watched root, defensively read.** Discover/watch
   `~/.codex/sessions/**` + `~/.codex/archived_sessions/` recursively.
   The reader must handle:
   - **Compressed rollouts** — newer codex-rs compresses some files
     (`compression.rs`); detect by extension/magic, decompress.
   - **Legacy TypeScript era** — pre-Rust Codex wrote a *single
     pretty-printed `.json`* per session (`{session, items[]}`), not
     JSONL. Detect and skip-with-log (or read `items[]`); **never crash
     the watcher.**
   `~/.codex/session_index.jsonl` (`{id, thread_name, updated_at}`) is a
   cheap discovery shortcut and gives a free session **title**.
2. **Synthetic record id.** Codex has no per-record `uuid` but `entries`
   keys on `(session_id, uuid)`. Synthesize a deterministic id =
   `(session_id, line-ordinal)`. Append-only files make it stable →
   re-ingest dedups via the existing `INSERT OR IGNORE`; resume from the
   `ingest_state` byte offset.
3. **A `source` discriminator** — one additive **nullable** column
   (default `'claude'`), append-only-policy compliant. Codex entries are
   `source='codex'`, so existing tools don't conflate the two corpora
   and can filter.
4. **The envelope→content transform.** Two-level unwrap (`line.type` →
   `payload.type`); `message.role` + block type (`input_text` vs
   `output_text`) give direction; `function_call.arguments` is a JSON
   **string** (parse it); tool results are top-level lines re-associated
   by `call_id`; reasoning is encrypted → placeholder.

Stamp the header's `session_id` / `cwd` / `git` onto every derived
record (Claude has these per line; Codex does not). Repo attribution can
use `git.repository_url`/`branch` directly, falling back to `cwd`.

## Parser posture (defensive by construction)

There is **no schema-version field**; the format is serde-tolerant
(`#[serde(other)]` catch-alls, field aliases) and ships near-daily. So
map the envelope + the handful of core `response_item` inner types and
**ignore unknown `type` / `payload.type`** rather than exhaustively
modelling every variant. Unknown → skip-and-continue, never fail the
file. (Aligns with the external-input defensive-coding rules.)

## Fidelity layer (🎯T112)

Parallel to Grok's T111: map Codex-native metadata into the same
columns/tables Claude already uses where the format allows.

| Codex signal | Mapping |
|--------------|---------|
| `turn_context.model` | Current model stamp on subsequent entries (`raw.message.model` → `entries.model`) |
| `session_meta.parent_thread_id` | `session_chains` edge, `mechanism=codex_parent` |
| `session_meta.forked_from_id` | `session_chains` edge, `mechanism=codex_fork` (if parent empty) |
| `session_meta.source.subagent` | `project=subagents` → `session_type=subagent` |
| `event_msg/token_count` | Synthetic assistant entry with usage + `[codex tokens]` text (noise); `last_token_usage` preferred over total |
| tool `cmd` / `command` array | `normalizeAgentToolInput` → Claude `command` string for `tool_command` |

### Still out of scope

The four `~/.codex/*.sqlite` DBs (logs/memories/state/goals — the state
DB is itself backfilled from the JSONL, which stays canonical);
`~/.codex/history.jsonl` (global *input* keystroke history, not a
transcript); decrypting `reasoning` beyond summary placeholders;
`event_msg` UI echoes of `response_item` (would double-index);
`compacted` / `world_state` / `inter_agent_*` bookkeeping; image
pipeline; Codex-specific MCP / threads / decisions plumbing.

## Cross-check references

- Source (pinned `cfead68`): `codex-rs/rollout/src/{lib,recorder,compression,state_db}.rs`,
  `codex-rs/protocol/src/{protocol,models}.rs`.
- Existing third-party parsers to validate against:
  `github.com/PixelPaw-Labs/codex-trace`,
  `jazzyalex/agent-sessions` (both read `~/.codex/sessions/*.jsonl`).
