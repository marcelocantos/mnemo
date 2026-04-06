# mnemo

Searchable memory across all Claude Code session transcripts. Runs as
a persistent HTTP-based MCP server — available in every Claude Code
session.

mnemo indexes JSONL transcript files from `~/.claude/projects/`,
maintains a realtime SQLite FTS5 index, and exposes search/query tools
via MCP. New transcripts are picked up automatically via filesystem
watching. All content block types are indexed — text, tool use, tool
results, and thinking blocks.

## Quick start

Tell your agent:

```
Install mnemo from https://github.com/marcelocantos/mnemo — brew
install, start the service, register it as an MCP server, and
restart the session. Follow the agents-guide.md in the repo.
```

Or do it yourself:

```bash
brew install marcelocantos/tap/mnemo
brew services start mnemo
claude mcp add --scope user mnemo -- mnemo
```

Then restart your Claude Code session. The `mnemo_*` tools will be
available in every session from that point on.

## Install

```bash
brew install marcelocantos/tap/mnemo
```

Or build from source (requires Go and CGo for SQLite):

```bash
go build -tags "sqlite_fts5" -o bin/mnemo .
```

## Running

**As a service** (recommended — survives reboots):

```bash
brew services start mnemo       # macOS
```

Logs: `$(brew --prefix)/var/log/mnemo.log`

**Manually**:

```bash
mnemo serve
```

## Registering as an MCP server

**Claude Code** (global install to `~/.claude.json`):

```bash
claude mcp add --scope user mnemo -- mnemo
```

**Generic MCP client** JSON config:

```json
{
  "mcpServers": {
    "mnemo": {
      "command": "mnemo"
    }
  }
}
```

Restart your agent session after registration — tools registered
mid-session are not picked up.

## MCP Tools

| Tool | Description |
|---|---|
| `mnemo_search` | Full-text search with context. Supports `repo` filter, configurable before/after context (default 3). |
| `mnemo_sessions` | List sessions by recency; filter by project, repo, or work type |
| `mnemo_read_session` | Read messages from a specific session (supports prefix IDs) |
| `mnemo_query` | Raw SQL SELECT against the transcript database |
| `mnemo_repos` | List repos with paths, session counts, last activity. Supports globs (`marcelocantos/sql*`). |
| `mnemo_stats` | Index statistics — sessions and messages by type |
| `mnemo_self` | Discover the calling session's ID via nonce protocol |

### Search with context

`mnemo_search` returns surrounding messages with each hit (like
`grep -C`). Defaults to 3 messages before and after. Context defaults
to substantive messages only (user/assistant, non-noise); pass
`context_filter: "all"` for everything including tool calls.

### Session filtering

By default, `mnemo_search` and `mnemo_sessions` return only interactive
sessions (excluding subagents, worktrees, and ephemeral sessions). Pass
`session_type: "all"` to include everything.

`mnemo_sessions` supports filtering by:
- `repo` — org/name substring (e.g. `"marcelocantos/mnemo"`)
- `work_type` — development, feature, bugfix, refactor, chore, docs,
  test, ci, release, review, branch-work
- `project` — project name substring

### SQL schema

The `mnemo_query` tool provides read-only access to:

| Table | Key columns |
|---|---|
| `messages` | id, session_id, project, role, text, timestamp, is_noise, content_type, tool_name, tool_use_id, tool_input (JSONB), is_error |
| `messages` (virtual) | tool_file_path, tool_command, tool_pattern, tool_description, tool_skill |
| `messages_fts` | FTS5 virtual table (excludes noise). `WHERE messages_fts MATCH 'terms'` |
| `sessions` | View: session_id, project, session_type, repo, work_type, topic, total_msgs, substantive_msgs, first_msg, last_msg |

Content types: `text`, `tool_use`, `tool_result`, `thinking`.

### Session self-identification

`mnemo_self` lets an agent discover its own session ID using a
two-phase nonce protocol, then query its own transcript with
`mnemo_read_session`.

## Agent guide

If you use an agentic coding tool, run `mnemo --help-agent` for the
full agent guide, or include
[`agents-guide.md`](agents-guide.md) in your project context.

## Licence

Apache 2.0 — see [LICENSE](LICENSE).
