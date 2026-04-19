# mnemo

Searchable memory across all Claude Code session transcripts. Runs as
a persistent MCP server — available in every Claude Code session.

mnemo indexes JSONL transcript files from `~/.claude/projects/`,
maintains a realtime SQLite FTS5 index, and exposes 30+ tools via MCP.
New transcripts are picked up automatically via filesystem watching.

**What it indexes:**

- **Session transcripts** — all content block types (text, tool use,
  tool results, thinking), with full-text search and surrounding context
- **Images** — inline and file-path images from transcripts, with AI
  descriptions, Apple Vision OCR, and CLIP embeddings for semantic and
  visual-similarity search
- **Git commits** — full history from all tracked repos, searchable by
  message, author, date
- **GitHub PRs and issues** — backfilled via `gh` CLI across all repos
  in session history
- **CI/CD runs** — GitHub Actions history with failure log indexing
- **Project documentation** — markdown, plain text, and PDF files from
  `docs/`, `design/`, `notes/`, repo root, and more
- **Auto-memory files** — cross-project memory search across all
  `~/.claude/projects/*/memory/` directories
- **Skills** — `~/.claude/skills/*.md` discovery and search
- **CLAUDE.md configs** — project instructions from all repos
- **Convergence targets** — `docs/targets.md` from all repos
- **Implementation plans** — `.planning/` directories from all repos
- **Audit logs** — `docs/audit-log.md` from all repos
- **Decisions** — automatically detected proposal+confirmation pairs
  across all sessions

**What else it does:**

- **Live session monitoring** — per-session CPU%, RSS, and memory
  pressure for active Claude Code processes
- **Session chain detection** — links `/clear`-bounded sessions into
  continuous work spans, with definitive detection for live transitions
  and heuristic fallback for historical sessions
- **Context compaction** — background summariser preserves context
  across `/clear` boundaries; `mnemo_restore` retrieves it
- **Token usage analytics** — aggregated input/output/cache tokens with
  cost estimates, grouped by day, model, or repo
- **Query templates** — save and reuse parameterised SQL queries
- **Pattern discovery** — analyses transcript history to find workaround
  patterns suggesting missing features
- **Permission analysis** — suggests `allowedTools` rules from actual
  tool usage patterns
- **Raw SQL access** — read-only queries against the full database,
  including sqldeep nested syntax for hierarchical JSON output

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
claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp
```

Then restart your Claude Code session. The `mnemo_*` tools will be
available in every session from that point on.

## Install

### macOS / Linux

```bash
brew install marcelocantos/tap/mnemo
```

### Windows

Download `mnemo-<version>-windows-amd64-setup.exe` from the
[releases page](https://github.com/marcelocantos/mnemo/releases/latest)
and double-click it. The installer:

- copies `mnemo.exe` to `C:\Program Files\mnemo\`,
- registers mnemo as a Windows Service (auto-start on boot,
  restart-on-failure),
- patches `%USERPROFILE%\.claude.json` so Claude Code picks up mnemo
  on its next session,
- shows up in **Add/Remove Programs** as `mnemo` for a clean uninstall.

No terminal required. Restart your Claude Code session after install.

### Build from source

Requires Go and CGo for SQLite:

```bash
go build -tags "sqlite_fts5" -o bin/mnemo .
```

On Windows, CGo requires a MinGW-w64 or LLVM clang toolchain on the
PATH — Go cgo does not use MSVC. The simplest setup is
[MSYS2](https://www.msys2.org/) with the `mingw-w64-x86_64-toolchain`
package. Release binaries from the GitHub release page are statically
linked and have no external runtime requirements. Live-session
discovery (`mnemo_whatsup`, `lsof`-based liveness) degrades gracefully
on Windows; transcript indexing and all query tools work identically.

## Running

**As a service** (recommended — survives reboots):

```bash
brew services start mnemo       # macOS / Linuxbrew
```

On Windows the installer already registers mnemo as a Windows Service
(`sc query mnemo` to inspect, Services.msc to manage). Service logs:
`%ProgramData%\mnemo\logs\mnemo.log` plus the Windows Event Log
(Application source: `mnemo`).

Logs on macOS: `$(brew --prefix)/var/log/mnemo.log`.

**Manually**:

```bash
mnemo               # listen on :19419 (default)
mnemo --addr :8080  # custom port
```

## Registering as an MCP server

**Claude Code** (global install to `~/.claude.json`):

```bash
claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp
```

**Generic MCP client** JSON config:

```json
{
  "mcpServers": {
    "mnemo": {
      "type": "http",
      "url": "http://localhost:19419/mcp"
    }
  }
}
```

Restart your agent session after registration — tools registered
mid-session are not picked up.

## MCP Tools

### Transcript search and browsing

| Tool | Description |
|---|---|
| `mnemo_search` | Full-text search with context (like `grep -C`). Repo filter, configurable before/after context. |
| `mnemo_sessions` | List sessions by recency; filter by project, repo, work type, or session type |
| `mnemo_read_session` | Read messages from a specific session (supports prefix IDs) |
| `mnemo_recent_activity` | Per-repo summary of recent session activity with work types and topics |
| `mnemo_status` | Rich status report: repos, sessions, and conversation excerpts |
| `mnemo_chain` | Retrieve the full `/clear`-bounded session chain for any session |
| `mnemo_self` | Discover the calling session's ID via two-phase nonce protocol |
| `mnemo_decisions` | Search past decisions (proposal + confirmation pairs) across sessions |

### Cross-project knowledge

| Tool | Description |
|---|---|
| `mnemo_memories` | Search auto-memory files from all projects |
| `mnemo_skills` | Search skill files from `~/.claude/skills/` |
| `mnemo_configs` | Search CLAUDE.md project instructions from all repos |
| `mnemo_targets` | Search convergence targets from all repos |
| `mnemo_plans` | Search implementation plans from all repos |
| `mnemo_audit` | Search audit logs from all repos |
| `mnemo_docs` | Search markdown, text, and PDF documentation from all repos |

### External source indexing

| Tool | Description |
|---|---|
| `mnemo_commits` | Search git commits across all tracked repos |
| `mnemo_prs` | Search GitHub PRs and issues across all repos |
| `mnemo_ci` | Search CI/CD run history (GitHub Actions) with failure log indexing |
| `mnemo_images` | Search images via FTS on descriptions/OCR, semantic embeddings, or visual similarity |

### Analytics and observability

| Tool | Description |
|---|---|
| `mnemo_usage` | Token usage analytics with cost estimates, grouped by day/model/repo |
| `mnemo_whatsup` | Live session resource monitor: CPU%, RSS, memory pressure |
| `mnemo_permissions` | Analyse tool usage patterns to suggest `allowedTools` rules |
| `mnemo_discover_patterns` | Find workaround patterns suggesting missing features |

### Database and templates

| Tool | Description |
|---|---|
| `mnemo_query` | Read-only SQL or sqldeep nested syntax against the full database |
| `mnemo_repos` | List repos with paths, session counts, last activity. Supports globs. |
| `mnemo_stats` | Index statistics — sessions and messages by type |
| `mnemo_define` | Save a reusable parameterised query template |
| `mnemo_evaluate` | Execute a named query template with parameters |
| `mnemo_list_templates` | List all saved query templates |

### Context restoration

| Tool | Description |
|---|---|
| `mnemo_restore` | Retrieve compacted context from prior `/clear` boundaries |

For full parameter documentation, see [`agents-guide.md`](agents-guide.md)
or run `mnemo --help-agent`.

## Workflows mnemo enables

mnemo's tools are building blocks. Some examples of what you can build
on top of them:

- **Context restoration after `/clear`** — use `mnemo_restore` to
  retrieve compacted summaries from prior conversation spans, so a
  fresh session can pick up where the last one left off
- **"Where are we?" briefings** — combine `mnemo_recent_activity` and
  `mnemo_search` to generate a summary of recent work across repos
  after being away
- **Convergence evaluation** — query `mnemo_recent_activity` to
  understand recent movement before assessing whether targets are on
  track
- **Cross-repo knowledge lookups** — search memories, configs, targets,
  and plans from any project without leaving your current session
- **Session forensics** — trace a multi-session work span with
  `mnemo_chain`, replay decisions with `mnemo_decisions`, or audit
  what commands were run with `mnemo_who_ran`
- **Custom dashboards** — use `mnemo_query` with SQL or sqldeep syntax
  to build project-specific analytics (message volume, tool usage
  patterns, active repos)

These workflows can be codified as Claude Code
[skills](https://docs.anthropic.com/en/docs/claude-code/skills) for
one-command invocation.

## Agent guide

If you use an agentic coding tool, run `mnemo --help-agent` for the
full agent guide, or include
[`agents-guide.md`](agents-guide.md) in your project context.

## Licence

Apache 2.0 — see [LICENSE](LICENSE).
