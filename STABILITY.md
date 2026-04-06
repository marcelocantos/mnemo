# Stability

## Stability commitment

Once mnemo reaches 1.0, backwards compatibility becomes a binding
contract. Breaking changes to the CLI interface, MCP tool signatures,
configuration, or database schema will not be made without forking to a
new product. The pre-1.0 period exists to get these surfaces right.

## Interaction surface catalogue

Snapshot as of v0.1.0.

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
| `query` | string | yes | SQL SELECT query | Stable |

**Note**: The SQL schema is an implicit part of this surface. See
database schema below.

#### mnemo_stats

No parameters. Returns session/message counts by type. **Stability**: Stable.

### Database schema (exposed via mnemo_query)

| Table/View | Columns | Stability |
|---|---|---|
| `messages` | id, session_id, project, role, text, timestamp, type, is_noise | Needs review |
| `messages_fts` | FTS5 virtual table matching `messages` (excludes noise) | Stable |
| `sessions` | session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg | Needs review |
| `ingest_state` | path, offset | Fluid |

**Notes**: The `messages` and `sessions` schemas may gain columns
(repo, work_type, topic are stored but not yet formally in the view).
`ingest_state` is an internal implementation detail and should not be
considered part of the public API.

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
  output would enable programmatic consumption by other tools (e.g.,
  jevon dashboard). Must decide on output format before 1.0.
- **Configurable paths**: Database and transcript paths are hardcoded.
  Should be configurable via flags or env vars.
- **Session metadata completeness**: repo, work_type, and topic are
  extracted heuristically and may be missing or inaccurate. Extraction
  quality should be audited before locking the schema.
- **Database migrations**: No migration system. Adding columns or
  changing the schema requires rebuilding the database. Need a
  migration strategy before 1.0.
- **Test coverage**: Minimal test suite. Core search, ingest, and
  session filtering need comprehensive tests.

## Out of scope for 1.0

- Multi-user / remote database support
- Authentication / access control on the HTTP endpoint
- Transcript modification or deletion tools
- Integration with non-Claude-Code transcript formats
- Summarisation or semantic search (vector embeddings)
