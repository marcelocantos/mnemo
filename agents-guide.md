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

### 4. Restart your agent session

The MCP tools only become available after restarting the session. This
is not optional — tools registered mid-session are not picked up.

## Verifying the setup

**Before restarting** (to confirm the serve process is running):

```bash
ls -la ~/.mnemo/mnemo.sock
```

This should show the Unix domain socket file. If it's missing, the
serve process isn't running — check `brew services list` and
`$(brew --prefix)/var/log/mnemo.log`.

**After restarting** (to confirm the MCP integration works):

Call `mnemo_stats`. It should return session and message counts. If it
fails with a connection error, the serve process may not be running.

## MCP Tools

### mnemo_search

Full-text search across session transcripts. Uses SQLite FTS5 syntax
(words, "quoted phrases", OR, NOT). Defaults to interactive sessions
only; pass `session_type: "all"` to include subagents and worktrees.

Key parameters:
- `query` (required) — FTS5 search query
- `repo` — scope to a specific repo. Flexible matching: bare name
  ("mnemo"), org/repo ("marcelocantos/mnemo"), or path fragments.
- `context_before` / `context_after` — number of surrounding messages
  to include with each hit (default 3 each, like `grep -C`)
- `context_filter` — `"substantive"` (default) returns only non-noise
  user/assistant messages as context. `"all"` includes tool calls,
  system messages, etc.
- `limit` — max results (default 20)

Each result includes a `message_id` for follow-up queries.

### mnemo_sessions

List sessions sorted by recency. Filter by `project`, `repo`
(org/name substring), or `work_type` (development, feature, bugfix,
refactor, chore, docs, test, ci, release, review, branch-work).
Defaults to interactive sessions with at least 6 substantive messages.

### mnemo_read_session

Read messages from a specific session. Accepts a full session ID or a
prefix. Supports `role` filtering ("user"/"assistant"), `offset`, and
`limit` for pagination.

### mnemo_recent_activity

Per-repo summary of recent session activity. Returns structured JSON
with session count, message count, last activity time, work types, and
key topics for each repo. Configurable recency window (default 7 days).

Use this for quick overviews of where active work is happening.

### mnemo_status

Rich status report: repos → sessions → conversation excerpts with
drill-down offsets. User messages in full, assistant messages truncated
(default 200 chars). Each message carries its database `id` — use
`mnemo_read_session` with `offset` to retrieve the full text.

Use this when you need context about recent work: the user references
prior discussions, you need project history before making decisions, or
you want to know what's been happening across repos. Don't dump the
output to the user — use it to inform your own understanding.

Parameters:
- `days` — recency window (default 7)
- `repo` — filter by repo name or path fragment
- `max_sessions` — per repo (default 3)
- `max_excerpts` — per session (default 20, most recent kept)
- `truncate_len` — assistant message truncation (default 200 chars)

### mnemo_query

Run a read-only SQL query against the database. Accepts plain SQL
(SELECT/WITH) or sqldeep nested syntax for hierarchical JSON output.

Key tables and columns:

| Table | Key columns |
|---|---|
| `messages` | id, session_id, project, role, text, timestamp, is_noise, content_type, tool_name, tool_use_id, tool_input (JSONB), is_error |
| `messages` (virtual) | tool_file_path, tool_command, tool_pattern, tool_description, tool_skill — computed from tool_input |
| `messages_fts` | FTS5 virtual table (excludes noise). `WHERE messages_fts MATCH 'terms'` |
| `sessions` | View: session_id, project, session_type, repo, work_type, topic, total_msgs, substantive_msgs, first_msg, last_msg |
| `session_summary` | Materialised session stats (trigger-maintained) |
| `session_meta` | Per-session metadata: repo, cwd, git_branch, work_type, topic |
| `memories` | id, project, file_path, name, description, memory_type, content |
| `memories_fts` | FTS5 on name, description, content, project |
| `skills` | id, file_path, name, description, content |
| `skills_fts` | FTS5 on name, description, content |
| `claude_configs` | id, repo, file_path, content |
| `claude_configs_fts` | FTS5 on content, repo |
| `audit_entries` | id, repo, file_path, date, skill, version, summary, raw_text |
| `audit_entries_fts` | FTS5 on summary, raw_text, repo |
| `targets` | id, repo, file_path, target_id, name, status, weight, description |
| `targets_fts` | FTS5 on name, description, raw_text, repo |
| `plans` | id, repo, file_path, phase, content |
| `plans_fts` | FTS5 on content, repo, phase |
| `ci_runs` | id, repo, run_id, workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url |
| `ci_runs_fts` | FTS5 on repo, workflow, branch, log_summary, conclusion |

Content types in `content_type`: `text`, `tool_use`, `tool_result`, `thinking`.

Example queries:
```sql
-- All Bash commands in a session
SELECT tool_command FROM messages WHERE tool_name = 'Bash' AND session_id = ?

-- Files edited across all sessions
SELECT DISTINCT tool_file_path FROM messages WHERE tool_name = 'Edit'

-- Failed tool calls
SELECT tool_name, text FROM messages WHERE content_type = 'tool_result' AND is_error = 1

-- Tool call with its result (join via tool_use_id)
SELECT tu.tool_name, tu.tool_command, tr.text AS result
FROM messages tu
JOIN messages tr ON tr.tool_use_id = tu.tool_use_id AND tr.content_type = 'tool_result'
WHERE tu.content_type = 'tool_use' AND tu.tool_name = 'Bash'
```

sqldeep nested syntax returns hierarchical JSON directly from SQL:
```sql
FROM session_meta sm
JOIN session_summary ss ON ss.session_id = sm.session_id
WHERE ss.last_msg >= datetime('now', '-7 days')
  AND ss.session_type = 'interactive'
SELECT {
  sm.repo,
  sm.cwd,
  ss.substantive_msgs,
  ss.last_msg,
}
ORDER BY ss.last_msg DESC
```

Results capped at 100 rows.

### mnemo_repos

List repositories that have been worked on in Claude Code sessions.
Returns repo name, filesystem path, session count, and last activity.

Use this to discover repo locations on disk, find related projects, or
get an overview of recent work across all repos.

The optional `filter` parameter supports:
- Bare name: `"mnemo"` — matches anywhere in repo name or path
- Org/repo: `"marcelocantos/mnemo"` — substring match
- Glob: `"marcelocantos/sql*"` — wildcard matching
- Path fragment: `"/work/github"` — matches against working directory

### mnemo_stats

Index statistics — total sessions and messages broken down by session
type, with noise vs substantive counts.

### mnemo_memories

Search across Claude Code auto-memory files from all projects. Memories
are structured notes with frontmatter (name, description, type) that
agents save across sessions.

Parameters:
- `query` — search query (fuzzy OR matching). Omit to list all.
- `type` — filter: "user", "feedback", "project", "reference"
- `project` — project name substring filter
- `limit` — max results (default 20)

### mnemo_usage

Token usage analytics across sessions. Aggregates input, output, cache
read, and cache creation tokens with cost estimates. Returns per-period
breakdown, totals, and hourly rate detection (tokens/hour, cost/hour).

Parameters:
- `days` — recency window (default 30)
- `repo` — repo filter
- `model` — model prefix filter (e.g. "claude-opus-4")
- `group_by` — "day" (default), "model", or "repo"

### mnemo_skills

Search across Claude Code skill files (`~/.claude/skills/`). Discover
available workflows and reusable procedures.

Parameters:
- `query` — search query (fuzzy OR matching). Omit to list all.
- `limit` — max results (default 20)

### mnemo_configs

Search across CLAUDE.md project instruction files from all repos. Find
build instructions, conventions, and delivery definitions.

Parameters:
- `query` — search query (fuzzy OR matching). Omit to list all.
- `repo` — repo filter
- `limit` — max results (default 20)

### mnemo_audit

Search across audit logs (docs/audit-log.md) from all repos. Find
when projects were last released or review maintenance patterns.

Parameters:
- `query` — search query (fuzzy OR matching). Omit to list all.
- `repo` — repo filter
- `skill` — skill name filter (e.g. "release", "audit")
- `limit` — max results (default 20)

### mnemo_targets

Search across convergence targets (docs/targets.md) from all repos.
Find targets across projects, check active/achieved status.

Parameters:
- `query` — search query (fuzzy OR matching). Omit to list all.
- `repo` — repo filter
- `status` — filter: identified, converging, achieved
- `limit` — max results (default 20)

### mnemo_plans

Search across implementation plans (.planning/ directories) from all
repos. Find past design decisions or understand how features were planned.

Parameters:
- `query` — search query (fuzzy OR matching). Omit to list all.
- `repo` — repo filter
- `limit` — max results (default 20)

### mnemo_who_ran

Find sessions that ran a specific shell command. Searches Bash tool_use
entries by command pattern, returning session ID, repo, matched command,
and timestamp.

Parameters:
- `pattern` (required) — command substring to match (LIKE)
- `days` — recency window (default 30)
- `repo` — repo filter
- `limit` — max results (default 20)

### mnemo_permissions

Analyze tool usage patterns across sessions to suggest allowedTools
rules for settings.json. Returns most frequently used tools with counts
and Bash command prefix analysis with suggested permission rules.

Parameters:
- `days` — recency window (default 30)
- `repo` — repo filter
- `limit` — max results per category (default 20)

### mnemo_ci

Search CI/CD run history across repos. Indexes GitHub Actions runs from
repos in session history. Failed run logs indexed for full-text search.

Parameters:
- `query` — search query (fuzzy OR matching against workflow, branch, logs). Omit to list recent runs.
- `repo` — repo filter
- `conclusion` — filter: success, failure, cancelled, skipped
- `days` — recency window (default 30)
- `limit` — max results (default 20)

### mnemo_self

Discover the calling session's ID. Two-phase nonce protocol:

1. Call `mnemo_self` with no arguments — returns a unique nonce
2. Call `mnemo_self` with `nonce: "<the nonce>"` — returns your session ID

The nonce appears in your transcript and is detected during ingestion.
Use the resolved session ID with `mnemo_read_session` to read your own
transcript.

## Common Patterns

- **What's been happening?**: `mnemo_status` — repos, sessions, and conversation excerpts from the last 7 days
- **Find a repo on disk**: `mnemo_repos` with `filter: "mnemo"` — returns the filesystem path
- **Find related repos**: `mnemo_repos` with `filter: "marcelocantos/sql*"` — glob matching
- **Find past decisions**: `mnemo_search` with query `"decided to" OR "went with" OR "chose"`
- **Recent work on a repo**: `mnemo_sessions` with `repo: "org/repo"` and `limit: 5`
- **Read a specific session**: `mnemo_sessions` to find the ID, then `mnemo_read_session`
- **What files were edited**: `mnemo_query` with `SELECT DISTINCT tool_file_path FROM messages WHERE tool_name = 'Edit'`
- **What commands were run**: `mnemo_query` with `SELECT tool_command FROM messages WHERE tool_name = 'Bash'`
- **Search within a repo**: `mnemo_search` with `repo: "mnemo"` and a query term
- **Custom analytics**: `mnemo_query` with SQL — e.g., message volume by day, most active projects
