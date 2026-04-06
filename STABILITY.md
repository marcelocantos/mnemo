# Stability

## Stability commitment

Once mnemo reaches 1.0, backwards compatibility becomes a binding
contract. Breaking changes to the CLI interface, MCP tool signatures,
configuration, or database schema will not be made without forking to a
new product. The pre-1.0 period exists to get these surfaces right.

## Interaction surface catalogue

Snapshot as of v0.3.0.

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

#### mnemo_query

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | yes | SQL SELECT/WITH query | Stable |

**Note**: Only SELECT and WITH queries are accepted. The SQL schema is
an implicit part of this surface. See database schema below.

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
| `messages` (virtual) | tool_file_path, tool_command, tool_pattern, tool_description, tool_skill | Needs review |
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

- **Structured output**: MCP tools return plain text. Structured JSON
  output would enable programmatic consumption by other tools. Must
  decide on output format before 1.0. sqldeep integration (🎯T8)
  would enable nested JSON queries.
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
