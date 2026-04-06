# mnemo

Searchable memory across all Claude Code session transcripts. Bimodal
architecture: `mnemo serve` is the persistent daemon that owns the
database, and `mnemo` (no args) is the stdio MCP proxy that agents use.

## What it does

Indexes JSONL transcript files from `~/.claude/projects/`, maintains
a SQLite FTS5 index, and exposes search/query tools via MCP. Watches
for new transcripts in realtime. Indexes all content block types:
text, tool_use, tool_result, and thinking.

## Build & Run

```bash
go build -tags "sqlite_fts5" -o bin/mnemo .
bin/mnemo serve          # persistent daemon (listens on ~/.mnemo/mnemo.sock)
bin/mnemo                # stdio MCP proxy (connects to serve via UDS)
```

## Install as MCP server

```bash
brew services start mnemo                     # start the daemon
claude mcp add --scope user mnemo -- mnemo    # register stdio proxy
```

## MCP Tools

- `mnemo_search` — Full-text search with context (default 3 before/after). Supports repo filter.
- `mnemo_sessions` — List sessions by recency, type, project, repo, work type
- `mnemo_read_session` — Read messages from a specific session (supports prefix IDs)
- `mnemo_query` — Raw SQL SELECT/WITH against the transcript database
- `mnemo_repos` — List repos with paths, session counts, last activity. Supports globs.
- `mnemo_stats` — Index statistics
- `mnemo_self` — Session self-identification via nonce protocol

## Code Structure

```
mnemo/
├── main.go                 # Entry point: stdio proxy or serve daemon
├── internal/
│   ├── store/              # SQLite FTS5 index, ingest, search
│   │   ├── store.go        # Database operations
│   │   └── iface.go        # Backend interface
│   ├── rpc/                # UDS communication between proxy and daemon
│   │   ├── rpc.go          # Protocol types and client
│   │   ├── server.go       # Daemon-side RPC handler
│   │   └── proxy.go        # Client-side Backend implementation
│   └── tools/tools.go      # MCP tool definitions and handlers
```

## Testing

```bash
go test -tags "sqlite_fts5" ./...
```

## Delivery

Merged to master via squash PR.
