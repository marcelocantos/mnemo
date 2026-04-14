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

After installing, add the following to your global `~/.claude/CLAUDE.md`
so agents know when to use mnemo:

```markdown
## Session context via mnemo

The `mnemo` MCP server indexes all Claude Code session transcripts.
When you need context about recent work — what repos have been
active, what was discussed, what decisions were made — use
`mnemo_status` or `mnemo_search` rather than guessing or asking the
user. Good moments to reach for mnemo:
- The user references prior work ("that thing we discussed", "the
  approach from last session", "continue where I left off")
- You need to understand the broader context of a project before
  making architectural decisions
- `/waw` or `/cv` needs recent activity data
- The user asks what's been happening across repos
```

## MCP Tools

- `mnemo_search` — Full-text search with context (default 3 before/after). Supports repo filter.
- `mnemo_sessions` — List sessions by recency, type, project, repo, work type
- `mnemo_read_session` — Read messages from a specific session (supports prefix IDs)
- `mnemo_memories` — Search across auto-memory files from all projects. Filters by type (user/feedback/project/reference), project. Cross-project memory search.
- `mnemo_skills` — Search across skill files from ~/.claude/skills/. Discover available workflows and reusable procedures.
- `mnemo_configs` — Search across CLAUDE.md project instruction files from all repos. Find build instructions, conventions, and delivery definitions.
- `mnemo_usage` — Token usage analytics: aggregated input/output/cache tokens with cost estimates. Filters by repo, model, date range. Groups by day, model, or repo.
- `mnemo_audit` — Search across audit logs (docs/audit-log.md) from all repos. Filters by repo, skill (release/audit/docs). Use to check when a project was last released or find maintenance patterns.
- `mnemo_targets` — Search across convergence targets (docs/targets.md) from all repos. Filters by repo, status. Cross-project target search.
- `mnemo_plans` — Search across implementation plans (.planning/ directories) from all repos. Use this to find past design decisions or understand how features were planned.
- `mnemo_who_ran` — Find sessions that ran a specific shell command. Searches Bash tool_use entries by command pattern, returning session, repo, command, and timestamp. Supports days window and repo filter.
- `mnemo_permissions` — Analyze tool_use patterns to identify most-used tools and Bash command prefixes, then suggest concrete allowedTools rules for settings.json.
- `mnemo_ci` — Search CI/CD run history across repos. Indexes GitHub Actions runs from repos seen in session history. Supports filtering by repo, conclusion (success/failure/cancelled/skipped), recency, and FTS across workflow names, branches, and failure logs.
- `mnemo_query` — SQL SELECT/WITH or sqldeep nested syntax (FROM ... SELECT { }) against the transcript database. Tables include: audit_entries (id, repo, file_path, date, skill, version, summary, raw_text), audit_entries_fts; targets (id, repo, file_path, target_id, name, status, weight, description, raw_text), targets_fts; plans (id, repo, file_path, phase, content, updated_at), plans_fts; ci_runs (id, repo, run_id, workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url), ci_runs_fts.
- `mnemo_recent_activity` — Per-repo summary of recent session activity (counts, recency, work types, topics)
- `mnemo_status` — Rich status report: repos → sessions → truncated conversation excerpts with drill-down offsets
- `mnemo_repos` — List repos with paths, session counts, last activity. Supports globs.
- `mnemo_stats` — Index statistics
- `mnemo_chain` — Retrieve the full /clear-bounded session chain for any session ID. Returns ordered chain from oldest to newest with per-session summaries and gap/confidence annotations.
- `mnemo_self` — Session self-identification via nonce protocol
- `mnemo_decisions` — Search past decisions (proposal + confirmation pairs) across all sessions. Decisions detected automatically during ingest and backfilled for existing sessions.
- `mnemo_whatsup` — Live session resource monitor: per-session CPU%, RSS, CPU time correlated with session metadata (repo, topic, work type), plus system memory pressure.
- `mnemo_define` — Define a reusable parameterised query template with {{param}} placeholders. Templates persist in SQLite across sessions.
- `mnemo_evaluate` — Execute a named query template with parameter values. Returns results like mnemo_query.
- `mnemo_list_templates` — List all saved query templates.
- `mnemo_commits` — Search git commits across all indexed repos. FTS5 on commit messages. Supports repo, author, date range filters. Retroactive: indexes full history from all known repos at startup.
- `mnemo_prs` — Search GitHub PRs and issues across all indexed repos. FTS5 on title/body. Supports state, author, type (pr/issue) filters. Retroactive: backfills from GitHub API at startup.
- `mnemo_discover_patterns` — Analyze transcript history to find workaround patterns suggesting missing features. Detects direct JSONL reads, transcript dir greps, repeated query shapes, and recurring searches.
- `mnemo_images` — Search images captured from transcripts. Inline base64 and file-path image references are extracted at ingest, stored as BLOBs with width/height/MIME metadata, and described by AI using surrounding conversation context. Searchable via FTS5 on descriptions. Requires ANTHROPIC_API_KEY for description generation.

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
