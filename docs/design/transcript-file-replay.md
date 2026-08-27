# Transcript File Replay

*Status: implemented — 2026-08-26 (🎯T150). Design note 🎯T150.1; engine,
adapters, scope selectors, CLI, and tests 🎯T150.2–T150.9.*

**Tracking.** Design source for 🎯T150 (parent) and its children. Grounded
in mnemo's indexed `messages` rows (`tool_use` / `tool_result`) across
Claude, Codex, Grok, and Cursor, plus optional Claude file-history
sidecars.

---

## TL;DR

Transcript file replay reconstructs **what agents wrote to disk** from
indexed session transcripts into a **disposable quarantine tree** — never
the live checkout unless an explicit, hard-to-typo override is used.
Provider-specific tool signals are normalised into one globally ordered
timeline of `{write, patch, delete}` ops; a shared engine applies them
with **last-writer-wins** semantics, path safety, and dry-run support.

Shell-only mutations (`Bash`, `exec_command`, `run_terminal_command`, …)
are **out of MVP fidelity** — recorded as skip+warn, not reconstructed.

---

## Problem

Agents mutate files through tool calls recorded in transcripts. When the
live tree has moved on — reverted commits, deleted branches, or work
done only in a session that never landed — mnemo already holds the
write/edit signals but not a way to **materialise** them safely.

This is a **recovery / forensics** feature: answer "what did the agent
try to write?" without touching production checkouts.

Evidence already indexed (approximate corpus counts at filing):

| Source | Tool signals | Notes |
|--------|--------------|-------|
| Claude | `Write`, `Edit` (~11k / ~65k) | Richest; optional `~/.claude/file-history/<session>/` backups |
| Codex | `apply_patch` (~11k) | Patch text in `messages.text`, not structured `tool_input` |
| Grok | `write`, `search_replace` | Paths via `normalizeAgentToolInput` → `tool_file_path` |
| Cursor | `Write`, `StrReplace`, `Delete` | Same shape as Claude-like tools |

Read-only tools (`Read`, `Grep`, …) emit no replay ops.

---

## Quarantine model

### Default output root

Real runs write **only** under an explicit quarantine directory:

```
~/.mnemo/replay/<run-id>/          # default when --out omitted
/tmp/mnemo-replay-<run-id>/        # acceptable override
<any-user-path>/                   # via --out; subject to safety checks below
```

`<run-id>` is a UTC timestamp + short random suffix (e.g.
`20260826T095012Z-a1b2c3`) so concurrent replays never collide.

### Never the session cwd by default

The session's `cwd` from `session_meta` is **context for path
resolution**, not the write target. Default `--out` must not equal any
indexed session cwd or git worktree root.

### Git worktree guard

Before creating files, the engine checks whether the quarantine root
**is or contains** a git repository's working tree (presence of `.git`
file or directory anywhere at or above the target path, or the target
path is inside a known repo root from the index).

| Condition | MVP behaviour |
|-----------|---------------|
| `--out` resolves inside a git work tree | **hard-fail** with message naming the repo root and suggesting `~/.mnemo/replay/…` |
| `--out` is a fresh empty directory outside any repo | **apply** |
| `--allow-live-tree` flag set (exact name for T150.8 CLI) | **apply** with a one-line stderr warning: `writing into live tree; quarantine contract waived` |

The override flag name is deliberately verbose; there is no short alias.

### Quarantine layout

Resolved file paths map under the quarantine root preserving **repo-relative**
structure when `session_meta.repo` is known and the absolute path lies
inside that repo:

```
<quarantine>/<repo-slug>/<path-relative-to-repo-root>
```

When repo is unknown or the path is outside the repo boundary, fall back
to **cwd-relative** layout:

```
<quarantine>/by-cwd/<sha256(cwd)[:8]>/<path-relative-to-cwd>
```

`<repo-slug>` is the `session_meta.repo` string with `/` → `--` (same
convention as elsewhere in mnemo). This keeps multi-session replay from
colliding when two sessions in different repos edit similarly named
relative paths.

### Permissions and symlinks

- Created directories: `0755`; created files: `0644`.
- The engine **never follows symlinks** when creating or modifying paths.
  If a path component under the quarantine root is an existing symlink,
  the op **hard-fails** for that path (partial-run rules apply).
- New symlinks are **never created** by replay.

---

## Normalised op timeline

Provider adapters emit a stream of normalised ops consumed by the shared
engine (🎯T150.2):

```go
// Conceptual shape — exact Go types land in T150.2.
type ReplayOp struct {
    Timestamp  time.Time // UTC; global sort key
    SessionID  string
    Source     string    // claude | codex | grok | cursor
    ToolUseID  string    // for audit trail; may be empty
    Path       string    // absolute path from transcript (pre-resolution)
    CWD        string    // session cwd at op time (for fallback layout)
    Repo       string    // session_meta.repo when known
    Kind       string    // write | patch | delete
    Body       []byte    // full content for write
    OldString  string    // patch: search anchor (Edit / search_replace / StrReplace)
    NewString  string    // patch: replacement text
    PatchText  string    // codex apply_patch raw hunk (engine parses)
}
```

### Global ordering

1. Collect ops matching scope (🎯T150.7).
2. Sort by `(timestamp ASC, session_id ASC, source ASC, tool_use_id ASC)`.
   Tie-breakers are stable but arbitrary — documented so tests can lock
   behaviour.
3. Apply sequentially to the quarantine virtual filesystem state.

### Last-writer-wins

For a given **quarantine-relative key** (after path resolution), later
ops supersede earlier ones:

| Kind | Effect on quarantine key |
|------|--------------------------|
| `write` | Replace entire file content |
| `patch` | Transform current quarantine content (see matrix for missing base) |
| `delete` | Remove file; later `write` recreates it |

**Conflict reporting:** when two ops in the **same replay run** target
the same quarantine key at the **same timestamp** (after normalisation
to millisecond precision), the engine applies the tie-break order above
and emits a **warning** in the run report (`conflict_same_ts: <key>`).
No interactive merge UI in MVP.

Cross-session duplicate paths are expected and resolve by last-writer-wins
— not an error.

---

## Scope selectors

The selector API (🎯T150.7) chooses which `tool_use` rows become ops.

| Shape | Parameters | Semantics |
|-------|------------|-----------|
| Single session | `session_id` | All file ops in that session, timestamp order |
| Intra-session window | `session_id`, `since`, `until` | Ops whose `messages.timestamp ∈ [since, until]` (inclusive) |
| Multi-session window | `since`, `until` | Ops across all sessions in range, merged into one global timeline |
| Filters (optional) | `repo`, `source` | Restrict to matching `session_meta.repo` / `session_meta.source` |

`source` filter values: `claude`, `codex`, `grok`, `cursor` (exact match
on `session_meta.source`).

**Tool inclusion:** only `tool_use` rows whose `tool_name` maps to a
replay kind (see provider table below). Paired `tool_result` rows inform
error/truncation policy but do not emit ops themselves.

**Out of scope rows:** `is_noise=1` messages are excluded.

---

## Path resolution and safety

### Input paths

Transcript paths arrive as absolute (`/Users/…/repo/foo.go`) or
occasionally relative. Adapters pass the path recorded in
`tool_file_path` or parsed from patch headers unchanged; the engine
resolves.

### Resolution algorithm

1. If path is relative, absolutise against session `cwd`.
2. Clean with `filepath.Clean` (platform-aware in engine; tests use
   forward slashes in reports for stability).
3. If absolutised path is under repo root → quarantine key
   `<repo-slug>/<rel-to-repo>`.
4. Else if under session cwd → quarantine key
   `by-cwd/<cwd-hash>/<rel-to-cwd>`.
5. Else → **skip+warn** (`path_outside_scope: <absolute-path>`).

### Escape refusal

After mapping to quarantine key, the engine joins `quarantineRoot + key`
and verifies the result is **strictly under** `quarantineRoot` using
`filepath.Rel` (reject `..` segments and drive-letter tricks on Windows).
Violations → **hard-fail** for that op (partial-run rules apply).

Absolute paths that traverse symlinks **outside** the quarantine root are
not followed; resolution uses the literal path string from the transcript.

---

## Provider signal sources

| Source | `tool_name` | Normalised kind | Payload location |
|--------|-------------|-----------------|------------------|
| Claude | `Write` | `write` | `tool_input.content` or `tool_input.file_text` |
| Claude | `Edit` | `patch` | `tool_input.old_string`, `tool_input.new_string`, `tool_file_path` |
| Claude | `StrReplace` | `patch` | same as Edit (if present in corpus) |
| Claude | `NotebookEdit` | — | **skip+warn** in MVP (`notebook_not_supported`) |
| Codex | `apply_patch` | `write` / `patch` / `delete` | `messages.text` (patch grammar); `tool_input` often nil |
| Grok | `write` | `write` | `tool_input` (normalised path + content fields) |
| Grok | `search_replace` | `patch` | `tool_input.old_string` / `new_string` or Grok aliases |
| Cursor | `Write` | `write` | `tool_input` |
| Cursor | `StrReplace` | `patch` | `tool_input` |
| Cursor | `Delete` | `delete` | `tool_file_path` |
| All | `Bash`, `shell`, `exec_command`, `run_terminal_command`, … | — | **skip+warn** (`shell_mutation_not_reconstructed`) |

Codex `apply_patch` grammar (MVP parser subset):

```
*** Begin Patch
*** Add File: <path>
+<line>
*** Update File: <path>
-<old>
+<new>
*** Delete File: <path>
*** End Patch
```

---

## Edge-case policy matrix

Each row states MVP behaviour: **apply**, **skip+warn**, or **hard-fail**.
The run report lists every skip/fail with the reason code cited here.
🎯T150.9 table-driven tests map one-to-one to these rows.

| # | Edge case | MVP behaviour | Reason code |
|---|-----------|---------------|-------------|
| E1 | `Edit` / `search_replace` / `StrReplace` / Codex **Update File** when no base file exists in quarantine | **skip+warn** | `patch_no_base` |
| E2 | Patch op when base exists but `old_string` / deletion hunk not found in quarantine content | **skip+warn** | `patch_anchor_missing` |
| E3 | `tool_use` paired with `tool_result.is_error = 1` | **skip+warn** (do not emit op) | `tool_use_failed` |
| E4 | `tool_use` with no `tool_result` yet / missing pairing | **apply** (transcript-only reconstruction; warn once per session) | `tool_result_missing` |
| E5 | Truncated tool payload (empty `content`, patch text ends mid-hunk, JSON truncated in `tool_input`) | **skip+warn** | `truncated_payload` |
| E6 | Duplicate paths across sessions (same quarantine key) | **apply** (last-writer-wins) | — (info only: `supersedes_prior`) |
| E7 | `Delete` / Codex **Delete File** for path with no quarantine file | **apply** (no-op success) | `delete_missing` |
| E8 | Rename / move if a provider expresses it (Claude none; Codex none in MVP grammar; Cursor none) | **skip+warn** if encountered | `rename_not_supported` |
| E9 | Binary content (NUL bytes in write body, or `file` magic heuristic on decoded text) | **skip+warn** | `binary_not_supported` |
| E10 | Huge file (write body > 32 MiB after decode) | **skip+warn** | `file_too_large` |
| E11 | Bash / exec / shell-only writes (content never in structured tool) | **skip+warn** | `shell_mutation_not_reconstructed` |
| E12 | Claude file-history enrichment (`~/.claude/file-history/<session>/`) | **Off by default.** When `--file-history` set: `Write` may fill missing body from sidecar; sidecar missing → fall back to transcript-only; sidecar path mismatch → **skip+warn** | `file_history_miss` / `file_history_used` |
| E13 | Dry-run (`--dry-run`) | **apply** to in-memory plan only; zero filesystem mutations; report lists would-be outcomes | — |
| E14 | Partial failure mid-run (earlier ops applied, later op hard-fails) | **Stop or continue?** → **continue**; earlier writes stand; run exits non-zero if any hard-fail; report lists applied/skipped/failed per op | `partial_run` |
| E15 | Path escape (resolved quarantine path would leave root) | **hard-fail** for that op | `path_escape` |
| E16 | Path outside repo/cwd scope (see resolution §) | **skip+warn** | `path_outside_scope` |
| E17 | `--out` inside git worktree without `--allow-live-tree` | **hard-fail** (before any op) | `live_tree_refused` |
| E18 | Codex **Add File** with malformed patch (no `+` lines) | **skip+warn** | `malformed_patch` |
| E19 | Codex **Update File** / **Delete File** on path not yet in quarantine | **skip+warn** (same as E1 for update) | `patch_no_base` / `delete_no_base` |
| E20 | NotebookEdit / provider-specific unsupported tools | **skip+warn** | `tool_not_supported` |
| E21 | Quarantine path component is an existing symlink | **hard-fail** for that op | `symlink_in_path` |
| E22 | Same timestamp collision on same key | **apply** with tie-break order; emit warning | `conflict_same_ts` |

### Patch-without-base rationale (E1)

Transcripts record edits, not guaranteed pre-images. Replaying an `Edit`
without a prior `Write` in scope cannot produce correct bytes unless
file-history enrichment (E12) or an earlier op in the same run seeded the
file. Inventing an empty base would silently corrupt content. **skip+warn**
is honest; users widen scope or enable file-history to recover.

### Failed tool_use rationale (E3)

`is_error=1` on the paired result means the tool did not succeed in the
original session; replaying its payload would fabricate changes that never
landed. Skip the op; include the tool name and id in the report.

### Dry-run vs apply (E13)

Dry-run executes the full pipeline — scope selection, adapter normalisation,
path resolution, patch simulation — but the engine's filesystem mutator is
a no-op. The report shows `would_apply`, `would_skip`, `would_fail` with
the same reason codes as a real run.

---

## Claude file-history enrichment (optional)

Claude Code copies touched files to:

```
~/.claude/file-history/<session-id>/<hash>/<filename>
```

When `--file-history` is enabled (CLI flag, default **off**):

1. For each `Write` whose transcript body is empty or truncated, look up
   the sidecar by matching basename and session id.
2. On match, use sidecar bytes as `Body`.
3. Sidecars are **never** copied blindly across mismatched absolute paths
   — basename match only when transcript path ends with the sidecar
   filename and the session id matches.
4. Missing sidecar → transcript-only path; may hit E5.

Replay must not **require** file-history for basic Write-only cases
(acceptance 🎯T150.3).

---

## Run report

Every CLI / API invocation emits a machine-readable summary (JSON to
stdout or sibling `.report.json` — exact surface in 🎯T150.8):

```json
{
  "run_id": "20260826T095012Z-a1b2c3",
  "quarantine_root": "/Users/me/.mnemo/replay/20260826T095012Z-a1b2c3",
  "dry_run": false,
  "ops_planned": 120,
  "ops_applied": 98,
  "ops_skipped": 20,
  "ops_failed": 2,
  "files_written": 45,
  "warnings": [{"code": "patch_no_base", "path": "…", "tool_use_id": "…"}],
  "failures": [{"code": "path_escape", "path": "…"}]
}
```

Exit code: `0` if no hard-fails; `1` if any `ops_failed > 0` or
`live_tree_refused`.

---

## Product surface (preview)

🎯T150.8 lands the user-facing entrypoint. Shapes assumed here:

```bash
# Single session, default quarantine
mnemo replay-files --session <id>

# Multi-session window, dry-run
mnemo replay-files --since 2026-08-01T00:00:00Z --until 2026-08-26T00:00:00Z \
  --source cursor --dry-run

# Explicit output + file-history enrichment
mnemo replay-files --session <id> --out ~/.mnemo/replay/manual-run \
  --file-history
```

MCP exposure is optional and secondary; filesystem mutation prefers CLI.

---

## Implementation map

| Target | Owns |
|--------|------|
| 🎯T150.1 | This document |
| 🎯T150.2 | Shared replay engine (`internal/replay` or similar) |
| 🎯T150.3 | Claude adapter + optional file-history |
| 🎯T150.4 | Grok adapter |
| 🎯T150.5 | Codex `apply_patch` parser |
| 🎯T150.6 | Cursor adapter |
| 🎯T150.7 | Scope selector over `messages` / `session_meta` |
| 🎯T150.8 | CLI (+ optional MCP) |
| 🎯T150.9 | Fixture + matrix oracle tests |

---

## Out of scope (MVP)

- Reconstructing shell-induced mutations (E11).
- Rename/move ops not expressed in provider tool grammar (E8).
- Binary / huge files (E9, E10).
- NotebookEdit.
- Applying replay directly into live checkouts without `--allow-live-tree`.
- Three-way merge or interactive conflict resolution.
- Watching quarantine for re-ingest into mnemo.

---

## Cross-references

- Parent target: 🎯T150 in `bullseye.yaml`
- Ingest formats: `docs/design/codex-ingest.md`, `docs/design/grok-ingest.md`,
  `docs/design/cursor-ingest.md`
- Tool path normalisation: `internal/store/tool_input.go`
- Indexed columns: `messages.tool_name`, `tool_file_path`, `tool_input`,
  `is_error`, `session_meta.cwd`, `session_meta.repo`, `session_meta.source`
