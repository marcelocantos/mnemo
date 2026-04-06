# mnemo

Searchable memory across all Claude Code session transcripts. Runs as
a persistent HTTP-based MCP server — available in every Claude Code
session.

mnemo indexes JSONL transcript files from `~/.claude/projects/`,
maintains a realtime SQLite FTS5 index, and exposes search/query tools
via MCP. New transcripts are picked up automatically via filesystem
watching.

## Install

```bash
brew install marcelocantos/tap/mnemo
```

Or build from source (requires Go and CGo for SQLite):

```bash
go build -tags "sqlite_fts5" -o bin/mnemo .
```

## Usage

Start the server:

```bash
mnemo                # listen on :19419 (default)
mnemo --addr :8080   # custom port
```

Register as an MCP server in Claude Code:

```bash
claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp
```

Restart your Claude Code session for the tools to become available.

## MCP Tools

| Tool | Description |
|---|---|
| `mnemo_search` | Full-text search across transcripts (FTS5 syntax: words, "phrases", OR, NOT) |
| `mnemo_sessions` | List sessions by recency; filter by project, repo, or work type |
| `mnemo_read_session` | Read messages from a specific session (supports prefix IDs) |
| `mnemo_query` | Raw SQL SELECT against the transcript database |
| `mnemo_stats` | Index statistics — sessions and messages by type |

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
| `messages` | id, session_id, project, role, text, timestamp, type, is_noise |
| `messages_fts` | FTS5 virtual table (excludes noise). `WHERE messages_fts MATCH 'terms'` |
| `sessions` | View: session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg |

## Agent guide

If you use an agentic coding tool, run `mnemo --help-agent` for the
full agent guide, or include
[`agents-guide.md`](agents-guide.md) in your project context.

## Licence

Apache 2.0 — see [LICENSE](LICENSE).
