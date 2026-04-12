# Stability

## Stability commitment

Once mnemo reaches 1.0, backwards compatibility becomes a binding
contract. Breaking changes to the CLI interface, MCP tool signatures,
configuration, or database schema will not be made without forking to a
new product. The pre-1.0 period exists to get these surfaces right.

## Interaction surface catalogue

Snapshot as of v0.15.0.

### CLI flags

| Flag | Type | Default | Stability |
|---|---|---|---|
| `--addr` | string | `:19419` | Stable |
| `--version` | bool | false | Stable |
| `--help-agent` | bool | false | Stable |

### MCP tools

#### mnemo_search

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | yes | Search query — plain words use OR (fuzzy), explicit AND/NOT/NEAR/quotes for precise control | Stable |
| `limit` | number | no | Max results (default 20) | Stable |
| `session_type` | string | no | Filter: interactive, subagent, worktree, ephemeral, all (default interactive) | Stable |
| `repo` | string | no | Repo filter (bare name, org/repo, or path fragment) | Stable |
| `context_before` | number | no | Messages before each hit (default 3) | Stable |
| `context_after` | number | no | Messages after each hit (default 3) | Stable |
| `context_filter` | string | no | "substantive" (default) or "all" | Needs review |

#### mnemo_sessions

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `session_type` | string | no | Filter by session type (default interactive) | Stable |
| `min_messages` | number | no | Min substantive messages (default 6) | Needs review |
| `limit` | number | no | Max sessions (default 30) | Stable |
| `project` | string | no | Project name substring filter | Stable |
| `repo` | string | no | Repo org/name substring filter | Stable |
| `work_type` | string | no | Work type filter | Needs review |

**Notes**: `min_messages` default of 6 may need tuning. `work_type`
values are heuristically extracted and the set may evolve.

#### mnemo_read_session

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `session_id` | string | yes | Session ID or prefix | Stable |
| `role` | string | no | Filter: user, assistant | Stable |
| `offset` | number | no | Skip first N messages (default 0) | Stable |
| `limit` | number | no | Max messages (default 50) | Stable |

#### mnemo_recent_activity

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 7) | Stable |
| `repo` | string | no | Repo filter (name or path fragment) | Stable |

**Added in v0.8.0.** Returns structured JSON per repo. **Stability**: Needs review — output shape may evolve.

#### mnemo_status

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 7) | Stable |
| `repo` | string | no | Repo filter | Stable |
| `max_sessions` | number | no | Max sessions per repo (default 3) | Needs review |
| `max_excerpts` | number | no | Max message excerpts per session (default 20) | Needs review |
| `truncate_len` | number | no | Assistant message truncation length (default 200) | Needs review |

**Added in v0.8.0.** Returns hierarchical JSON (repos → sessions → excerpts). **Stability**: Needs review — defaults and output shape may evolve.

#### mnemo_memories

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `type` | string | no | Filter: user, feedback, project, reference | Needs review |
| `project` | string | no | Project name substring filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.11.0.** Searches across auto-memory files from all projects.
Returns name, description, type, project, content. **Stability**: Needs
review — first release, output format and filters may evolve.

#### mnemo_usage

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 30) | Stable |
| `repo` | string | no | Repo filter (name or path fragment) | Stable |
| `model` | string | no | Model prefix filter (e.g. "claude-opus-4") | Needs review |
| `group_by` | string | no | Group by: day (default), model, repo | Needs review |

**Added in v0.11.0.** Returns aggregated token usage with cost estimates.
**Stability**: Needs review — cost model and grouping options may evolve.

#### mnemo_skills

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `~/.claude/skills/*.md`. **Stability**: Needs review.

#### mnemo_configs

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches CLAUDE.md files from all repos. **Stability**: Needs review.

#### mnemo_audit

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `skill` | string | no | Skill name filter (e.g. "release") | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `docs/audit-log.md` from all repos. **Stability**: Needs review.

#### mnemo_targets

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `status` | string | no | Status filter: identified, converging, achieved | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `docs/targets.md` from all repos. **Stability**: Needs review.

#### mnemo_plans

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `.planning/` directories from all repos. **Stability**: Needs review.

#### mnemo_who_ran

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `pattern` | string | yes | Command substring to match (LIKE) | Needs review |
| `days` | number | no | Recency window in days (default 30) | Stable |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.13.0.** Searches Bash tool_use entries by command pattern. **Stability**: Needs review.

#### mnemo_permissions

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 30) | Stable |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results per category (default 20) | Stable |

**Added in v0.13.0.** Analyzes tool_use patterns to suggest allowedTools rules. **Stability**: Needs review.

#### mnemo_ci

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `conclusion` | string | no | Filter: success, failure, cancelled, skipped | Needs review |
| `days` | number | no | Recency window in days (default 30) | Stable |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.13.0.** Indexes GitHub Actions runs from repos in session history. Failed run logs indexed for FTS. **Stability**: Needs review.

#### mnemo_query

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | yes | SQL SELECT/WITH or sqldeep nested syntax | Stable |

**Note**: Accepts plain SQL (SELECT/WITH) and sqldeep nested syntax
(`FROM ... SELECT { }`). sqldeep queries are transparently transpiled.
The SQL schema is an implicit part of this surface. See database schema below.

#### mnemo_repos

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `filter` | string | no | Bare name, org/repo, path fragment, or glob (e.g. `marcelocantos/sql*`) | Needs review |

**Notes**: Returns repo name, filesystem path, session count, and last
activity. Filter matching uses SQL LIKE with `*` mapped to `%`.

#### mnemo_stats

No parameters. Returns session/message counts by type. **Stability**: Stable.

#### mnemo_chain

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `session_id` | string | yes | Any session ID in the chain (or a prefix) | Needs review |

**Added in v0.16.0.** Resolves the full /clear-bounded session chain for a given session. Returns an ordered list of ChainLinks (oldest → newest) with per-session summaries and gap/confidence for each link. Single-element result if no chain is found. **Stability**: Needs review — first release, output format may evolve.

#### mnemo_self

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `nonce` | string | no | Nonce from previous call. Omit to generate. | Needs review |

**Notes**: Two-phase nonce protocol. Nonces detected during ingest and
stored in indexed `session_nonces` table. The mechanism may evolve.

### Database schema (exposed via mnemo_query)

| Table/View | Columns | Stability |
|---|---|---|
| `entries` | id, session_id, project, type, timestamp, raw (JSONB) | Needs review |
| `entries` (virtual) | model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, agent_id, version, slug, is_sidechain, data_type, data_command, data_hook_event, top_tool_use_id, parent_tool_use_id | Needs review |
| `messages` | id, entry_id, session_id, project, role, text, timestamp, type, is_noise, content_type, tool_name, tool_use_id, tool_input (JSONB), is_error | Needs review |
| `messages` (virtual) | tool_file_path, tool_command, tool_pattern, tool_description, tool_skill, tool_old_string, tool_new_string, tool_content, tool_query, tool_url, tool_name_param, tool_prompt, tool_subject, tool_status, tool_task_id | Needs review |
| `messages_fts` | FTS5 virtual table matching `messages` (excludes noise) | Stable |
| `snapshot_files` | id, entry_id, session_id, file_path, backup_time — auto-extracted via trigger from file-history-snapshot entries | Needs review |
| `snapshot_files_fts` | FTS5 on file_path | Needs review |
| `sessions` | View joining session_summary + session_meta: session_id, project, session_type, repo, git_branch, work_type, topic, total_msgs, substantive_msgs, first_msg, last_msg | Needs review |
| `session_summary` | Trigger-maintained materialised table: session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg | Needs review |
| `session_meta` | Per-session metadata: session_id, repo, cwd, git_branch, work_type, topic | Needs review |
| `memories` | id, project, file_path (unique), name, description, memory_type, content, updated_at — auto-memory files from ~/.claude/projects/*/memory/*.md | Needs review |
| `memories_fts` | FTS5 on name, description, content, project — with insert/update/delete triggers | Needs review |
| `skills` | id, file_path (unique), name, description, content, updated_at — ~/.claude/skills/*.md | Needs review |
| `skills_fts` | FTS5 on name, description, content | Needs review |
| `claude_configs` | id, repo, file_path (unique), content, updated_at — CLAUDE.md from all repos | Needs review |
| `claude_configs_fts` | FTS5 on content, repo | Needs review |
| `audit_entries` | id, repo, file_path, date, skill, version, summary, raw_text — docs/audit-log.md | Needs review |
| `audit_entries_fts` | FTS5 on summary, raw_text, repo | Needs review |
| `targets` | id, repo, file_path, target_id, name, status, weight, description, raw_text — docs/targets.md | Needs review |
| `targets_fts` | FTS5 on name, description, raw_text, repo | Needs review |
| `plans` | id, repo, file_path (unique), phase, content, updated_at — .planning/**/*.md | Needs review |
| `plans_fts` | FTS5 on content, repo, phase | Needs review |
| `ci_runs` | id, repo, run_id (unique), workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url | Needs review |
| `ci_runs_fts` | FTS5 on repo, workflow, branch, log_summary, conclusion | Needs review |
| `session_chains` | successor_id (PK), predecessor_id, boundary, gap_ms, confidence, mechanism, detected_at — /clear-bounded chain links | Fluid |
| `session_nonces` | nonce → session_id mapping for mnemo_self | Fluid |
| `ingest_state` | path, offset | Fluid |
| `ingest_status` | stream, last_backfill, files_indexed, files_on_disk — per-stream backfill state | Fluid |

**Notes**: v0.9.0 added the `entries` table which stores every JSONL line
as JSONB with 15 virtual columns for high-query fields. All entry types
(user, assistant, progress, system, file-history-snapshot) are now ingested.
`messages` gained `entry_id` FK linking content blocks to their source entry.
v0.10.0 added `snapshot_files` with trigger-based extraction from
file-history-snapshot entries and FTS5 on file paths.
v0.11.0 added `memories` table for cross-project auto-memory indexing,
`mnemo_usage` for token analytics, and changed search to OR-by-default
with BM25 ranking for fuzzy matching.
v0.12.0 added five context source tables: `skills`, `claude_configs`,
`audit_entries`, `targets`, `plans` — each with FTS5 indexes. These
index non-transcript sources (skill files, CLAUDE.md configs, audit
logs, convergence targets, implementation plans) from all known repos.
v0.13.0 added three observability tools: `mnemo_who_ran` (process
attribution), `mnemo_permissions` (permission analysis), `mnemo_ci`
(CI/CD run history). Added `ci_runs` table with FTS5 for GitHub Actions
indexing. `mnemo_usage` gained hourly rate detection.
v0.16.0 (🎯T16) added session chain detection. `session_chains` table
links /clear-bounded JSONL sessions into work spans via a time-gap
heuristic (≤5s, same cwd, /clear marker in successor's first message).
Chain detection runs at ingest time and on startup (backfill). Added
`Predecessor`, `Successor`, `Chain` store methods and the `mnemo_chain`
MCP tool.
v0.15.0 (🎯T17) made every repo-level stream self-heal on startup:
targets, audit logs, plans, CLAUDE.md, and CI polling discover repos
via filesystem walk of configured workspace roots in addition to
session_meta. Added `ingest_status` table, `streams` field on
`StatusResult` and `StatsResult`, `Config` / `LoadConfig` /
`SetWorkspaceRoots` public API, and `~/.mnemo/config.json` optional
config file with `workspace_roots` (default `[~/work]`) and
`extra_project_dirs` keys.
This surface is still evolving. `ingest_state`, `ingest_status`, and
`session_nonces` are internal implementation details.

Entry types: `user`, `assistant`, `progress`, `system`, `file-history-snapshot`.
Content types (messages): `text`, `tool_use`, `tool_result`, `thinking`.

### Output formats

Tool outputs are plain text, not structured JSON. This is intentional
for readability in agent contexts but **Fluid** — structured output may
be added or replace text output before 1.0.

### Configuration

Optional config file at `~/.mnemo/config.json` (since v0.15.0):

```json
{
  "workspace_roots": ["/Users/you/work"],
  "extra_project_dirs": []
}
```

- `workspace_roots` — filesystem roots walked for `.git` entries to
  discover repos. Defaults to `[~/work]` when absent. The workspace
  walker skips `node_modules`, `.venv`, `venv`, `target`, `build`,
  `.build`, `dist`, `.next`, `.cache`, `__pycache__`, `.tox`,
  `.mypy_cache`, and `.pytest_cache`.
- `extra_project_dirs` — reserved for cross-platform transcript ingest
  (🎯T15).

All other configuration is via CLI flags. **Stability**: Fluid — the
config file is new and its schema may grow before 1.0.

### Data storage

- Database location: `~/.mnemo/mnemo.db`
- Transcript source: `~/.claude/projects/` (JSONL files)

Both paths are hardcoded. **Stability**: Needs review — may become
configurable before 1.0.

## Gaps and prerequisites

- **Structured output**: Most MCP tools return plain text. `mnemo_recent_activity`
  and `mnemo_status` return structured JSON; `mnemo_query` with sqldeep
  syntax returns hierarchical JSON. Remaining text-output tools may
  migrate to structured output before 1.0.
- **Configurable paths**: Database and transcript paths are hardcoded.
  Should be configurable via flags or env vars.
- **Session metadata completeness**: repo, work_type, and topic are
  extracted heuristically and may be missing or inaccurate. Extraction
  quality should be audited before locking the schema.

## Out of scope for 1.0

- Multi-user / remote database support
- Authentication / access control on the HTTP endpoint
- Transcript modification or deletion tools
- Integration with non-Claude-Code transcript formats
- Summarisation or semantic search (vector embeddings)
- Agent-defined query templates (🎯T7) — valuable but not a 1.0 gate
