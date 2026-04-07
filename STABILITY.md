# Stability

## Stability commitment

Once mnemo reaches 1.0, backwards compatibility becomes a binding
contract. Breaking changes to the CLI interface, MCP tool signatures,
configuration, or database schema will not be made without forking to a
new product. The pre-1.0 period exists to get these surfaces right.

## Interaction surface catalogue

Snapshot as of v0.8.0.

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
| `query` | string | yes | FTS5 search query | Stable |
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

#### mnemo_self

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `nonce` | string | no | Nonce from previous call. Omit to generate. | Needs review |

**Notes**: Two-phase nonce protocol. Nonces detected during ingest and
stored in indexed `session_nonces` table. The mechanism may evolve.

### Database schema (exposed via mnemo_query)

| Table/View | Columns | Stability |
|---|---|---|
| `messages` | id, session_id, project, role, text, timestamp, type, is_noise, content_type, tool_name, tool_use_id, tool_input (JSONB), is_error | Needs review |
| `messages` (virtual) | tool_file_path, tool_command, tool_pattern, tool_description, tool_skill, tool_old_string, tool_new_string, tool_content, tool_query, tool_url, tool_name_param, tool_prompt, tool_subject, tool_status, tool_task_id | Needs review |
| `messages_fts` | FTS5 virtual table matching `messages` (excludes noise) | Stable |
| `sessions` | View joining session_summary + session_meta: session_id, project, session_type, repo, git_branch, work_type, topic, total_msgs, substantive_msgs, first_msg, last_msg | Needs review |
| `session_summary` | Trigger-maintained materialised table: session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg | Needs review |
| `session_meta` | Per-session metadata: session_id, repo, cwd, git_branch, work_type, topic | Needs review |
| `session_nonces` | nonce → session_id mapping for mnemo_self | Fluid |
| `ingest_state` | path, offset | Fluid |

**Notes**: The `messages` schema expanded significantly in v0.3.0 with
content block columns (content_type, tool_name, tool_use_id, tool_input,
is_error) and virtual computed columns. This surface is still evolving.
`ingest_state` and `session_nonces` are internal implementation details.

Content types: `text`, `tool_use`, `tool_result`, `thinking`.

### Output formats

Tool outputs are plain text, not structured JSON. This is intentional
for readability in agent contexts but **Fluid** — structured output may
be added or replace text output before 1.0.

### Configuration

No config files. All configuration is via CLI flags. **Stability**: Stable.

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
- **System entry indexing**: System entries (hooks, stop reasons) are
  not yet indexed. The ingest pipeline skips non-user/assistant entries.

## Out of scope for 1.0

- Multi-user / remote database support
- Authentication / access control on the HTTP endpoint
- Transcript modification or deletion tools
- Integration with non-Claude-Code transcript formats
- Summarisation or semantic search (vector embeddings)
- Agent-defined query templates (🎯T7) — valuable but not a 1.0 gate
