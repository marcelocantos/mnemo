# mnemo

Searchable memory across all Claude Code session transcripts. Runs as
a standalone stdio MCP server — available in every Claude Code session.

## What it does

Indexes JSONL transcript files from `~/.claude/projects/`, maintains
a SQLite FTS5 index, and exposes search/query tools via MCP. Watches
for new transcripts in realtime.

## Build & Run

```bash
go build -tags "sqlite_fts5" -o bin/mnemo .
bin/mnemo                # listen on :19419
bin/mnemo --addr :8080   # custom port
```

## Install as MCP server

```bash
claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp
```

## MCP Tools

- `mnemo_search` — Full-text search across transcripts
- `mnemo_sessions` — List sessions by recency, type, project, repo, work type
- `mnemo_read_session` — Read messages from a specific session (supports prefix IDs)
- `mnemo_query` — Raw SQL against the transcript database
- `mnemo_stats` — Index statistics

## Code Structure

```
mnemo/
├── main.go                 # Entry point, stdio MCP server
├── internal/
│   ├── store/store.go      # SQLite FTS5 index, ingest, search
│   └── tools/tools.go      # MCP tool definitions and handlers
```

## Testing

```bash
go test -tags "sqlite_fts5" ./...
```

## Delivery

Merged to master via squash PR.
