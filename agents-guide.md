# mnemo — Agent Guide

mnemo is an MCP server that provides searchable memory across all
Claude Code session transcripts. It indexes JSONL transcript files from
`~/.claude/projects/` and maintains a realtime FTS5 index in SQLite.

## Full setup (all steps required)

Installation is not complete until all four steps succeed. Do not stop
after `brew install` — the server must be running and registered as an
MCP server for mnemo to be usable.

### 1. Install the binary

```bash
brew install marcelocantos/tap/mnemo
```

Or build from source (requires Go and CGo for SQLite):

```bash
go build -tags "sqlite_fts5" -o bin/mnemo .
```

### 2. Start the server as a background service

**macOS (Homebrew)**:

```bash
brew services start mnemo
```

This starts mnemo on `:19419` via launchd and keeps it running across
reboots. Logs go to `$(brew --prefix)/var/log/mnemo.log`.

**Linux (systemd)** — create `~/.config/systemd/user/mnemo.service`:

```ini
[Unit]
Description=mnemo MCP server

[Service]
ExecStart=%h/.local/bin/mnemo
Restart=always

[Install]
WantedBy=default.target
```

Then: `systemctl --user enable --now mnemo`

**Manual** (any platform):

```bash
mnemo                # listen on :19419 (default)
mnemo --addr :8080   # custom port
```

### 3. Register as an MCP server

**Claude Code** (global install to `~/.claude.json`):

```bash
claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp
```

**Generic MCP client** JSON config:

```json
{
  "mcpServers": {
    "mnemo": {
      "transport": "http",
      "url": "http://localhost:19419/mcp"
    }
  }
}
```

### 4. Restart your agent session

The MCP tools only become available after restarting the session. This
is not optional — tools registered mid-session are not picked up.

## Verifying the setup

**Before restarting** (to confirm the server is listening):

```bash
lsof -iTCP:19419 -sTCP:LISTEN
```

This should show a `mnemo` process. Do **not** use `curl` to check —
the MCP endpoint only responds to POST requests with a JSON-RPC body,
so a plain GET or empty POST will return nothing, which is normal
behaviour, not a sign that the server is down.

**After restarting** (to confirm the MCP integration works):

Call `mnemo_stats`. It should return session and message counts. If it
fails with a connection error, check `brew services list` and
`$(brew --prefix)/var/log/mnemo.log`.

## MCP Tools

### mnemo_search

Full-text search across session transcripts. Uses SQLite FTS5 syntax
(words, "quoted phrases", OR, NOT). Defaults to interactive sessions
only; pass `session_type: "all"` to include subagents and worktrees.
Pass `repo` to scope results to a specific repo — accepts bare name
("mnemo"), org/repo ("marcelocantos/mnemo"), or path fragments.

### mnemo_sessions

List sessions sorted by recency. Filter by `project`, `repo`
(org/name substring), or `work_type` (development, feature, bugfix,
refactor, chore, docs, test, ci, release, review, branch-work).
Defaults to interactive sessions with at least 6 substantive messages.

### mnemo_read_session

Read messages from a specific session. Accepts a full session ID or a
prefix. Supports `role` filtering ("user"/"assistant"), `offset`, and
`limit` for pagination.

### mnemo_query

Run a read-only SQL SELECT against the database. Useful for ad-hoc
analysis. Key tables:

| Table | Description |
|---|---|
| `messages` | All indexed messages (id, session_id, project, role, text, timestamp, type, is_noise) |
| `messages_fts` | FTS5 virtual table (excludes noise). Use `WHERE messages_fts MATCH 'terms'` |
| `sessions` | View with per-session stats: session_id, project, session_type, total/substantive msg counts, timestamps |
| `ingest_state` | Tracks ingestion progress per file |

Results capped at 100 rows.

### mnemo_stats

Index statistics — total sessions and messages broken down by session
type, with noise vs substantive counts.

## Common Patterns

- **Find past decisions**: `mnemo_search` with query `"decided to" OR "went with" OR "chose"`
- **Recent work on a repo**: `mnemo_sessions` with `repo: "org/repo"` and `limit: 5`
- **Read a specific session**: `mnemo_sessions` to find the ID, then `mnemo_read_session`
- **Custom analytics**: `mnemo_query` with SQL — e.g., message volume by day, most active projects
