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

Download the installer for your architecture from the
[releases page](https://github.com/marcelocantos/mnemo/releases/latest)
— `mnemo-<version>-windows-amd64-setup.exe` for Intel/AMD PCs or
`mnemo-<version>-windows-arm64-setup.exe` for Copilot+ / Surface Pro
X / other Windows-on-ARM devices — and double-click it. The
installer:

- copies `mnemo.exe` to `C:\Program Files\mnemo\`,
- registers mnemo as a Windows Service (auto-start on boot,
  restart-on-failure, survives logoff / battery / sleep),
- patches `%USERPROFILE%\.claude.json` with an MCP URL that includes
  `?user=<your username>` so the service routes requests to your
  home directory correctly,
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

On Windows the installer registers mnemo as a Windows Service
(`sc query mnemo` to inspect, services.msc to manage). The service
runs as LocalSystem and routes each request to the right user's
home based on the `?user=<name>` query parameter on the MCP URL —
written into `%USERPROFILE%\.claude.json` automatically by the
installer.

Logs on macOS: `$(brew --prefix)/var/log/mnemo.log`.

> **PATH note for service deployments**: mnemo's compactor shells out
> to `claude -p`. Services (launchd, systemd) inherit a minimal
> `PATH` that typically excludes the `claude` binary. The Homebrew
> formula's service block sets `PATH` to include
> `$(brew --prefix)/bin` and `~/.claude/local` automatically. If you
> run mnemo via a custom plist or systemd unit, set `PATH` explicitly
> to include the directory where `claude` is installed. Without it,
> compaction will fail with a logged ERROR.

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
| `mnemo_session_structure` | Structural summary of a session — counts of entry types, stop_reasons, content-block kinds, tool names |
| `mnemo_tool_result` | Raw tool-result payload by `(session_id, tool_use_id)` — supports byte offset + truncation |
| `mnemo_locate_uuid` | Locate any entry by full or prefix UUID across six uuid sources, with surrounding context |

### Cross-project knowledge

| Tool | Description |
|---|---|
| `mnemo_memories` | Search auto-memory files from all projects |
| `mnemo_get_memory` | Read the raw markdown body of a named memory file (or list memories) |
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

## Federation across linked instances

Multiple mnemo daemons (different machines, different Claude Code
projects) can be peered so a single query reaches every host's index.
mTLS authentication, per-peer pinned trust, parallel fan-out, graceful
degradation under peer failure.

### Setup

On each host:

1. Generate the local mTLS material and print the public cert:

   ```bash
   mnemo print-endpoint > /tmp/<hostname>.pem
   ```

   First invocation generates `~/.mnemo/endpoint/{cert.pem,key.pem}`
   (key mode 0600, ECDSA P-256, 10-year validity, regenerated on
   corruption or expiry).

2. Distribute that PEM to peer hosts. Each peer drops it into its
   own `~/.mnemo/peers/<name>.pem` (any name is fine; it's the
   filename the peer uses to refer to this host).

3. Print this host's federation URL:

   ```bash
   mnemo print-federated-addr
   # → https://<hostname>:19420/mcp
   ```

4. On each peer, declare the link in `~/.mnemo/config.json`:

   ```json
   {
     "linked_instances": [
       {
         "name": "alice",
         "url": "https://alice.example:19420/mcp",
         "peer_cert": "alice"
       }
     ]
   }
   ```

   `peer_cert` is either the basename of the file under
   `~/.mnemo/peers/` (without `.pem`) or an inline PEM block. Validation
   fails loud at startup on duplicate names, non-https URLs, or
   unresolvable certs.

5. Restart the daemon (`brew services restart mnemo`).

6. Verify with `mnemo ping-peer <name>` — invokes `mnemo_stats` on the
   peer over mTLS and prints the response.

### Behaviour

When `linked_instances` is non-empty, 16 read-shaped tools wrap their
response in a `FanoutEnvelope`:

```json
{
  "local": <local result>,
  "peers": [{"instance": "alice", "result": <alice's result>}],
  "warnings": [{"instance": "bob", "error_kind": "timeout", "message": "..."}]
}
```

Slow or offline peers drop into `warnings[]` with a typed
`error_kind` (`timeout`, `connection_refused`, `tls_handshake`,
`server_error`, `malformed_response`, `connect_failed`); the local
response returns regardless. Per-peer timeout default 5s.

Write- and control-shaped tools (`mnemo_self`, template
register/evaluate, `mnemo_restore`, `mnemo_whatsup`, `mnemo_docs`,
`mnemo_synthesis`, `mnemo_permissions`, `mnemo_query`, `mnemo_stats`,
`mnemo_status`, `mnemo_chain`) bypass federation entirely.

When `linked_instances` is empty or absent, federation is disabled and
all tools return their original local-only response shape unchanged
(backwards-compatible).

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
