# mnemo — Agent Guide

mnemo is an MCP server that provides searchable memory across coding-agent
session transcripts. It maintains a realtime FTS5 index in SQLite and
ingests:

- **Claude Code** — `~/.claude/projects/**/*.jsonl` (`source=claude`)
- **Codex CLI** — `~/.codex/sessions/**/rollout-*.jsonl` and
  `archived_sessions/` (`source=codex`; see `docs/design/codex-ingest.md`)
- **Grok CLI** — `~/.grok/sessions/**/updates.jsonl` plus sibling
  `summary.json` (`source=grok`; honour `GROK_HOME`; see
  `docs/design/grok-ingest.md`)

Filter or inspect provenance via `session_meta.source` (`mnemo_query`).

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

The Homebrew formula's service block sets `PATH` to
`$(brew --prefix)/bin:~/.claude/local:/usr/bin:/bin:/usr/sbin:/sbin`
so that mnemo's compactor can find the `claude` binary. If you run
mnemo via a different mechanism (custom launchctl plist, manual
invocation), make sure `PATH` includes the directory containing
`claude` — typically `/opt/homebrew/bin` (npm/bun install) or
`~/.claude/local` (Anthropic's official installer). Without a `claude`
binary on `PATH`, compaction fails and mnemo logs an ERROR:
`compact: claude subprocess spawn failed — executable not found in PATH`.

**Linux (systemd)** — create `~/.config/systemd/user/mnemo.service`:

```ini
[Unit]
Description=mnemo MCP server

[Service]
ExecStart=%h/.local/bin/mnemo
Restart=always
Environment=PATH=/usr/local/bin:/usr/bin:/bin:%h/.claude/local

[Install]
WantedBy=default.target
```

Then: `systemctl --user enable --now mnemo`

> **Note for service deployments**: launchd and systemd both start
> processes with a minimal `PATH` (`/usr/bin:/bin:/usr/sbin:/sbin`).
> The `Environment=` line above adds `~/.claude/local` (Anthropic's
> official install path). Add `/usr/local/bin` or other prefixes as
> needed for your setup.

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

**Grok Build** (writes `~/.grok/config.toml`):

```bash
grok mcp add --transport http mnemo http://localhost:19419/mcp
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

### 4. Restart your agent session

The MCP tools only become available after restarting the session. This
is not optional — tools registered mid-session are not picked up.

## Verifying the setup

**Before restarting** (to confirm the server is listening):

```bash
lsof -iTCP:19419 -sTCP:LISTEN
```

This should show the mnemo process holding the port. If nothing is
shown, the server isn't running — check `brew services list` and
`$(brew --prefix)/var/log/mnemo.log`.

Do **not** use `curl` to probe `/mcp` — MCP endpoints only respond to
POST requests with a JSON-RPC body. A plain GET or empty POST returns
nothing meaningful, which agents misread as "server not ready".

**After restarting** (to confirm the MCP integration works):

Call `mnemo_stats`. It should return session and message counts. If it
fails with a connection error, the server may not be running.

## Upgrading mnemo

If the user asks you to upgrade mnemo:

```bash
brew upgrade marcelocantos/tap/mnemo
brew services restart mnemo          # REQUIRED — see below
```

On **Windows**, there is no brew path: download the installer from
https://github.com/marcelocantos/mnemo/releases/latest and run it. It
stops the service, replaces the binary and restarts it.

Three things that trip agents up here:

1. **`brew upgrade` alone does nothing visible.** It replaces the
   binary on disk while the old daemon keeps running. Without the
   restart you will report a successful upgrade that did not take
   effect. Confirm with `mnemo --version`, or the `upgrade.available`
   check in `mnemo_ops` (op=doctor), which compares running against latest.
2. **Do not re-register the MCP server.** The registration is a stable
   URL; `register-mcp` is for first-time setup only. A running agent
   session reconnects on its own.
3. **The first start afterwards can take many minutes, and that is
   normal.** If the release changes the schema, mnemo takes a full
   pre-migration backup before applying it — around 11 minutes on a
   21 GB index. The daemon serves on the old schema throughout and
   logs `schema upgrade deferred to background`; `mnemo_ops` (op=doctor)'s
   `schema.upgrade` check says the same. **Do not restart it to "fix"
   this** — let the migration finish. Tools keep working meanwhile.

`auto_upgrade` in `~/.mnemo/config.json` opts into applying releases
automatically during a quiet window, but only for Homebrew non-Windows
installs; anything else stays notify-only.

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
Live sessions (with an active Claude Code process) are annotated with
`[LIVE pid=NNNNN]` in the output.

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

It is also the **first-line ingest-freshness health check** (🎯T75). The
top-level `diagnostics` block answers "is the index stale, where, and by
how much?" before you grep `~/.claude/projects` or run ad-hoc SQL:
- `freshness` — `now_utc`, freshest indexed timestamp, and lag.
- `divergence` — per-stream gap, including `transcript_index` pending
  bytes/files.
- `transcript_sources` — one row per configured project dir: total
  files, files never ingested, files behind, pending bytes, newest
  on-disk mtime, and forensic examples of the largest behind files
  (path, session_id, size, offset, pending bytes, and `state`:
  `new` / `append_behind` / `truncated` / `rewritten`).
- `repo_diagnostic` (only when `repo` is supplied) — the Claude project
  dirs that map to the repo, latest indexed vs latest on-disk mtime, and
  an explicit note when no source maps to the filter or when on-disk
  transcripts are newer than the index.

So when fresh transcript content for a repo seems missing, call
`mnemo_status repo=<name>` first.

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
| `session_chains` | successor_id (PK), predecessor_id, boundary, gap_ms, confidence, mechanism, detected_at |

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

### mnemo_usage

Token usage analytics across sessions. Aggregates input, output, cache
read, and cache creation tokens with costs. Returns per-period breakdown,
totals, and hourly rate detection (tokens/hour, cost/hour).

Costs come from a fetched rate card matched on the **exact** model
identifier — no prefix matching and no fallback. Two disclosure fields
matter as much as the totals:

- `unpriced_models` — counted but **not costed**, because the rate card
  has no entry. Normal for a newly released model, which is exactly the
  spend worth watching. Never reported as `$0.00`.
- `uncounted` — volume **excluded** from every row and total, per source,
  with the reason. A record carrying no message id cannot be
  deduplicated, and deduplication is worth 1.95x–2.83x, so sources that
  supply no key (Codex and Grok today) are reported separately rather
  than summed into a figure that claims to be deduplicated.

Pricing requires opting in (see "Budgeting" below). Without it, token
counts stay exact and every model reports as unpriced.

Parameters:
- `days` — recency window (default 30)
- `since` / `until` — RFC3339 bounds; override `days`
- `repo` — repo filter
- `model` — model prefix filter (e.g. "claude-opus-4")
- `group_by` — "day" (default), "model", "repo", "session", or "block"

### Budgeting and cost control

Opt in to pricing, and optionally set a cap, in `~/.mnemo/config.json`:

```json
{
  "pricing": { "enabled": true },
  "budget": {
    "monthly_cap_usd": 500,
    "timezone": "Australia/Sydney",
    "warn_at_pct": 100
  },
  "dedup_key": "message_request"
}
```

`pricing.enabled` gates the single outbound HTTP GET that fetches the
model rate card; it is read per attempt, so it takes effect in both
directions without a restart. Cards are cached at
`~/.mnemo/pricing.json` and archived by date under `~/.mnemo/pricing/`,
which is what lets a record be priced with the card that was in force
when it was written.

`budget.timezone` is an IANA zone and must be explicit — the same data
bucketed in a different zone yields a different report, and a period
boundary is exactly where that shows up.

`dedup_key` is configurable because its validity is
environment-dependent: `message_request` is right against a provider's
own API, but a gateway may retry, coalesce, or reissue identifiers.
Validate it by reconciling against your billing source per serving path.
An unrecognised value resolves to the default rather than being honoured.

Once a soft limit is breached, mnemo throttles the agents it invokes
itself — compactor, segmenter, reviewer, image description — and nothing
else. Throttling is soft (a delay between runs, never a refusal) and
loud (`mnemo_ops` (op=doctor) reports the level, reason, and what would lift it).

### mnemo_compacted_session

Return the compacted view of a session — its distilled retrieval form
under the token-volume model (🎯T72): the compaction summaries (the
dense, durable layer) followed by the addenda tail, the substantive
messages past the latest compaction cursor, computed live from the
index.

A converged session is mostly summary plus a bounded tail. A session
below the size floor has no summary and the addenda ARE the whole
session — its raw entries are its retrieval form. Reach for this instead
of `mnemo_read_session` when you want the distilled view rather than the
raw transcript.

Parameters:
- `session_id` (required) — exact ID or prefix, consistent with
  `mnemo_read_session`
- `addenda_limit` — max addenda messages past the cursor to include
  (default 200)

### mnemo_session_structure

Returns a structural summary of a session — counts of entry types,
assistant `stop_reason` values, system `subtype` values, content-block
kinds (text / tool_use / tool_result / thinking), and tool names
invoked inside `tool_use` blocks. JSON output, compact enough to
inspect directly. Use to quickly answer "what is in this session?"
without deep-reading the transcript, or to compare session shapes.

Parameters:
- `session_id` (required) — full ID or prefix

### mnemo_locate_uuid

Locates an entry by full or prefix UUID across all sessions. Searches
six uuid sources: `entry_uuid`, `parent_uuid`, `top_tool_use_id`,
`parent_tool_use_id`, content-block `tool_use_id`, content-block
`tool_result_id`. Returns each match with `session_id`, `entry_id`,
type, timestamp, `match_kind` (which arm matched), the full matched
UUID, and surrounding context. Use to debug session chaining, trace
tool_use_id retries, or resolve cross-session references when all you
have is an opaque uuid.

Parameters:
- `uuid` (required) — full UUID or prefix (≥ 8 chars usually unique)
- `context_before` — context messages before the match (default 3)
- `context_after` — context messages after the match (default 3)

### mnemo_vault (🎯T143.3)

All vault operations behind one `op`: `status`, `sync`, `gc`,
`migration_doc`, `bridge_list`, `recluster`, `themes_inspect`,
`themes_pin`, `themes_split`, `themes_merge`. Ten tools were folded
here; none had ever been called by an agent. `themes_split` and
`themes_merge` remain stubs recording a `theme_overrides` row.

Document-level themes cluster decisions, compaction summaries,
patterns, and (when indexed) user vault notes into named groups.

Default engine is fully local: TF-IDF + single-link agglomerative
clustering, bigram labels. Two independent opt-ins open egress
(API keys alone never call out):

```json
{
  "vault_clustering": {
    "engine": "embeddings",
    "label": { "engine": "llm" }
  }
}
```

- `engine: "embeddings"` + `VOYAGE_API_KEY` → Voyage vectors (cached in
  `cluster_embeddings`); falls back to heuristic on outage when
  `fallback_to_heuristic_on_outage` is true (default).
- `label.engine: "llm"` + `ANTHROPIC_API_KEY` → Haiku labels; otherwise
  bigram / user-anchor labels.

Tools:

- `mnemo_vault_recluster` — run a clustering pass now. Params:
  `engine` (optional override: `heuristic` | `embeddings`),
  `force_reembed` (bool, default false). Returns a `cluster_runs` row.
- `mnemo_vault_themes_inspect` — full members, centroid, pin/archive
  state, and `label_path` / `label_gate` for a theme id or slug.
- `mnemo_vault_themes_pin` — pin/unpin so `retire_after` (default 180d)
  does not auto-archive. Params: `theme`, `unpin`, `reason`.
- `mnemo_vault_themes_split` / `mnemo_vault_themes_merge` — **stubs**:
  record a `theme_overrides` directive only; live apply is a follow-up.

Vault pages land at `_mnemo/themes/<slug>.md` (archived under
`_mnemo/themes/_archive/`). A 24h reconciler also runs when the daemon
is up; use recluster for an immediate pass.

### mnemo_config

Read or update mnemo's runtime configuration (`~/.mnemo/config.json`)
without restarting the daemon. Use this to flip `vault_path` on or off,
add a new workspace root, or rotate `synthesis_roots` from the same
agent that just told you what to change.

Parameters:
- `op` — `"read"` (default) or `"write"`
- `patch` — for `op=write`, a JSON object with the keys to update.
  Same shape as `~/.mnemo/config.json`. Only keys present in the patch
  are modified; unspecified keys retain their current value. Set a
  field to its zero value (empty string for `vault_path`, empty array
  for the slices) to clear it.

Hot-reload coverage:
- `vault_path` — applied live. Old vault workers stop, a fresh
  exporter is built at the new path, and an initial sync starts in the
  background. Set to `""` to disable vault export entirely.
- `vault_layout` — applied live. Values: `"v1"` (legacy root layout),
  `"both"` (dual-write for migration), `"v2"` (new `_mnemo/` namespace,
  default for new vaults). See `internal/vault/README.md` for the
  migration path from v1 to v2.
- `vault_profile`, `vault_bridges`, `vault_bridges_max_links` — applied
  live (🎯T64.5 / 🎯T64.6). Profile selects Obsidian/Logseq/Foam/generic
  link dialect; bridges inject fenced link blocks into user-owned anchors.
- `vault_clustering` — read per clustering pass (no restart). Controls
  engine, thresholds, label chain, and retire_after (🎯T64.8).
- `workspace_roots`, `extra_project_dirs`, `synthesis_roots`,
  `todo_globs` — applied live; subsequent ingest passes pick up the new
  roots/globs.
- `plugins` (🎯T102) — list of out-of-process (or in-process later)
  extension instances. Each entry: `name`, `enabled`, `transport`
  (`launch` | `connect` | `inprocess`), plus transport fields
  (`command`/`args`, `url`, or `script`) and optional `params`. Applied
  live: enable starts an instance, disable tears one down. Metadata
  (facets, UI, config schema) comes from the plugin's `/manifest`, not
  config. Optional default home: `~/.mnemo/plugins/<name>/`. Connect
  attaches to a base URL (ready + manifest); launch spawns an executable
  that prints `MNEMO_PLUGIN_PORT <port>` on stdout. Ready plugins are
  reverse-proxied at `/plugins/<name>/*`. Facet adapters (reconcile /
  check / notify) ride the existing scheduler and diag surface.
  Health: `plugin.<name>.ready` on `mnemo_ops` (op=doctor) / `/health`.
  UI (🎯T102.9): `GET /api/plugins` lists each ready plugin's menu
  contribution; the menu-bar popup renders footer rows and loads
  `preview_url` in a live WKWebView. `plugin.reload` on `/api/events`
  forces a WebView reload.
  In-process (🎯T102.6): `transport: "inprocess"` + `script` path to a
  JS file defining `handle(req)` (goja). MCP tools (🎯T102.10): plugins
  with `facets.mcp` exposing `GET …/mcp/tools` and `POST …/mcp/call`
  appear as `plugin_<name>__<tool>` on mnemo's MCP list.
- `signal_sources` (🎯T102.8) — pure-config liveness probes
  (`file_mtime`, `launchd`, `newest_artifact`, `last_commit`) with
  `cadence` + `grace_multiple`. Surface as `signal.<name>` on
  `mnemo_ops` (op=doctor) / `/health` without a plugin process.
- `linked_instances` — persisted but requires a daemon restart (the
  federation client is wired once at startup).

The write response lists which fields changed, which were adopted live,
and which require a restart.

## Federation across linked instances

If `~/.mnemo/config.json` declares `linked_instances`, 16 read-shaped
tools (`mnemo_search`, `mnemo_sessions`, `mnemo_recent_activity`,

`mnemo_who_ran`, `mnemo_audit`, `mnemo_targets`,
`mnemo_skills`, `mnemo_configs`) wrap their result in a `FanoutEnvelope`
attributing per-instance results:

```json
{
  "local": <local result>,
  "peers": [{"instance": "alice", "result": <alice's result>}],
  "warnings": [{"instance": "bob", "error_kind": "timeout", "message": "..."}]
}
```

`error_kind` values: `timeout`, `connection_refused`, `tls_handshake`,
`server_error`, `malformed_response`, `connect_failed`,
`unknown_instance`, `unknown`. Slow or offline peers drop into
`warnings[]` with a typed kind; the local response always returns.
Per-peer timeout default 5s.

When `linked_instances` is empty or absent, all tools return their
original local-only response shape unchanged. Write- and
control-shaped tools (`mnemo_ops`, `mnemo_query`,
`mnemo_stats`, `mnemo_status`, `mnemo_vault`,
`mnemo_thread`, `mnemo_note`) bypass federation entirely.

Setup is documented in the README under "Federation across linked
instances" — `mnemo print-endpoint`, `mnemo print-federated-addr`,
`mnemo ping-peer <name>` are the operator-facing CLI tools.

## Self-diagnostics

mnemo continuously checks its own health (🎯T83). A registry of named
checks runs on a schedule — the full suite at startup, fast checks every
~3 minutes, the full suite hourly — each returning a severity
(ok / warn / fail), a detail, and a remediation hint.

Checks: `compactor.workdir` (the summariser's working dir exists and is
writable), `claude.path` (the `claude` binary is on the daemon's PATH),
`ingest.roots` (configured roots resolve), `compactor.breaker` (the
compaction circuit-breaker has not tripped), `ingest.backfill` (the
indexer has backfilled since the daemon started), `db.readable`,
`images.embedder` (🎯T121 — whether the CLIP image embedder ran or was
skipped and why: disabled by config, no `uv`, no helper script, or
running with per-outcome counts; failures are never retried past their
attempt budget, and are also queryable in `image_embedding_attempts`).

Three surfaces expose the same report:
- **`mnemo_ops` (op=doctor)** (MCP) — runs the full suite on demand: the single
  "is mnemo healthy, and what do I do about it" call.
- **`GET /health`** — the JSON report; backs the dashboard **health
  page** at `http://localhost:19419/#health` (issues sorted by severity,
  with copy-remediation and file-a-fix affordances).
- **OS notifications** — on a transition *into* fail severity, a native
  notification (macOS `osascript`, Linux `notify-send`; local-only, no
  network) deep-links the health page. **Opt-out**: enabled by default,
  fail-only, deduped with a 6h re-notify cooldown. Silence with
  `"disable_health_notifications": true` in `~/.mnemo/config.json`.

(There is also a one-shot `mnemo diagnose` CLI subcommand for a
terminal health check.)

**Resilience (🎯T84).** A background task that fails repeatedly — the
compaction watcher when every tick fails (a missing summariser cwd,
`claude` off PATH), or a stream reconciler that keeps erroring — trips a
circuit-breaker and backs off for a cooldown instead of retrying hot.
This stops one broken task from burning CPU and contending the SQLite
writer, so it can never starve ingestion. A tripped breaker surfaces as
a fail-severity `mnemo_ops` (op=doctor) check.

## Index freshness

**Invariant: `mnemo_*` tools reflect the full on-disk corpus at the time
of the last query.** Agents do not need to reason about whether the
index is stale, which repos mnemo has seen, or whether a given stream
has been kept in sync with the filesystem.

On daemon startup, every repo-level stream (`targets`, `audit`, `plans`,
`claude_configs`, CI) performs a filesystem-walk backfill rather than
enumerating repos from session history alone. Sources are:

1. **Workspace roots** — configured via `~/.mnemo/config.json`
   (`workspace_roots: ["/path/to/work"]`). Defaults to `~/work`. Each
   root is walked for `.git` entries to discover repos.
2. **Session metadata** — any repo reached through a Claude Code
   session's `cwd` is also included, so repos outside the workspace
   roots are not lost.

The union is the discovery set. While the daemon is stopped, any
changes to `docs/targets.md`, `docs/audit-log.md`, `CLAUDE.md`,
`.planning/**/*.md`, or new repos created under a workspace root are
picked up automatically on the next startup — no manual re-index.

Per-stream coverage is surfaced via `mnemo_status` and `mnemo_stats`
under the `streams` key:

```json
{
  "streams": [
    {"stream": "audit",          "files_indexed": 38, "files_on_disk": 38, "last_backfill": "2026-04-12T11:55:59Z"},
    {"stream": "claude_configs", "files_indexed": 52, "files_on_disk": 52, "last_backfill": "2026-04-12T11:55:59Z"},
    {"stream": "plans",          "files_indexed": 10, "files_on_disk": 10, "last_backfill": "2026-04-12T11:55:59Z"},
    {"stream": "targets",        "files_indexed": 10, "files_on_disk": 18, "last_backfill": "2026-04-12T11:55:59Z"}
  ]
}
```

`files_on_disk` counts the artefacts discovered under the workspace
roots; `files_indexed` counts how many actually landed in the index.
Non-zero drift (on_disk > indexed) typically indicates a parse error or
an empty source and is surfaced, not hidden.

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
- **Trace a work span across /clear**:  with any session ID — returns the full chain of linked sessions
- **Which sessions are live?**: `mnemo_sessions` — live sessions are annotated with `[LIVE pid=NNNNN]`
- **Custom analytics**: `mnemo_query` with SQL — e.g., message volume by day, most active projects
