# mnemo

Searchable memory across all Claude Code session transcripts. mnemo
runs as a single HTTP MCP daemon — clients register it directly via
the streamable-HTTP transport.

## What it does

Indexes JSONL transcript files from `~/.claude/projects/`, maintains
a SQLite FTS5 index, and exposes search/query tools via MCP. Watches
for new transcripts in realtime. Indexes all content block types:
text, tool_use, tool_result, and thinking.

Also ingests OpenAI **Codex CLI** rollout transcripts from
`~/.codex/sessions/` (and `archived_sessions/`) — its OpenAI
Responses-API records are transformed into the same content model and
flow through the same search/session machinery, tagged
`session_meta.source = 'codex'`. See `docs/design/codex-ingest.md`.

Also ingests xAI **Grok CLI** sessions from `~/.grok/sessions/`
(honours `GROK_HOME`) — ACP `updates.jsonl` + `summary.json` are
transformed into the same content model, tagged
`session_meta.source = 'grok'`. See `docs/design/grok-ingest.md`.

## Build & Run

```bash
go build -tags "sqlite_fts5" -o bin/mnemo .
bin/mnemo                # HTTP MCP daemon (default :19419)
bin/mnemo --addr :8080   # custom listen address
```

## Install as MCP server

```bash
brew services start mnemo                                                              # start the daemon
claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp          # register
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
- `mnemo_usage` — Token usage analytics: aggregated input/output/cache tokens with costs. Filters by repo, model, date range. Groups by day, model, repo, session, or 5-hour billing block. Costs come from a fetched rate card matched on the **exact** model identifier — no prefix matching, no fallback (🎯T135). Two disclosure fields matter as much as the totals: `unpriced_models` (counted but not costed, because the card has no entry — normal for a newly released model, which is exactly the spend you want to see) and `uncounted` (volume EXCLUDED from every total, per source, with the reason: a record with no message id cannot be deduplicated, and deduplication is worth 1.95x-2.83x).
- `mnemo_budget` — Spend against a resetting budget period, with projection and culprits (🎯T135). Alerts on the **projection**, not a threshold already crossed: "at $47/day, 2026-07 exceeds its $500 cap on the 19th" is actionable where "80% consumed" arrives after the decision that caused it. Burn rate is measured over a trailing week. When not `ok`, names culprit sessions largest-first, each resolved to a repo, working directory and live pid where one exists.
- `mnemo_agent_trees` — Sub-agent fan-outs reconstructed and costed **as a whole**, ranked by aggregate tree cost (🎯T137). For the failure a per-session ranking cannot see: forty individually-unremarkable agents that collectively trip the wire. Reports the skill and turn that started each tree, `tree_cost_usd` vs `direct_cost_usd`, depth, and whether it is still running. Claude-only.
- `mnemo_audit` — Search across audit logs (docs/audit-log.md) from all repos. Filters by repo, skill (release/audit/docs). Use to check when a project was last released or find maintenance patterns.
- `mnemo_targets` — Search across convergence targets (`docs/targets.md`) from all repos. Filters by repo, status. Cross-project target search. **Note**: today the indexer only reads `docs/targets.md`. mnemo's own targets live in `bullseye.yaml` at the repo root (matching the global bullseye convention) and are therefore not visible to this tool. Teaching the indexer to also read `bullseye.yaml` is a known follow-up.
- `mnemo_plans` — Search across implementation plans (.planning/ directories) from all repos. Use this to find past design decisions or understand how features were planned.
- `mnemo_who_ran` — Find sessions that ran a specific shell command. Searches Bash tool_use entries by command pattern, returning session, repo, command, and timestamp. Supports days window and repo filter.
- `mnemo_permissions` — Analyze tool_use patterns to identify most-used tools and Bash command prefixes, then suggest concrete allowedTools rules for settings.json.
- `mnemo_ci` — Search CI/CD run history across repos. Indexes GitHub Actions runs from repos seen in session history. Supports filtering by repo, conclusion (success/failure/cancelled/skipped), recency, and FTS across workflow names, branches, and failure logs.
- `mnemo_query` — SQL SELECT/WITH or sqldeep nested syntax (FROM ... SELECT { }) against the transcript database. Tables include: audit_entries (id, repo, file_path, date, skill, version, summary, raw_text), audit_entries_fts; targets (id, repo, file_path, target_id, name, status, weight, description, raw_text), targets_fts; plans (id, repo, file_path, phase, content, updated_at), plans_fts; ci_runs (id, repo, run_id, workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url), ci_runs_fts.
- `mnemo_recent_activity` — Per-repo summary of recent session activity (counts, recency, work types, topics)
- `mnemo_status` — Rich status report: repos → sessions → truncated conversation excerpts with drill-down offsets, PLUS a transcript-ingest freshness diagnostics block (🎯T75): now_utc + freshest-indexed lag, per-stream divergence, per-project-dir coverage (files behind, pending bytes, forensic examples), and a repo-specific section when filtered. First-line check for "is the index stale/behind for this repo?" Also `diagnostics.watch` (🎯T142): tree-watch backend, open FDs, roots, poll counters — first-line check for "will mnemo exhaust vnodes again?"
- `mnemo_repos` — List repos with paths, session counts, last activity. Supports globs.
- `mnemo_stats` — Index statistics
- `mnemo_chain` — Retrieve the full /clear-bounded session chain for any session ID. Returns ordered chain from oldest to newest with per-session summaries and gap/confidence annotations.
- `mnemo_compacted_session` — Return the compacted view of a session: its compaction summaries (the dense, durable layer) plus the addenda tail (substantive messages past the latest compaction cursor, computed live). The token-volume retrieval form (🎯T72) — use instead of `mnemo_read_session` when you want the distilled view rather than the raw transcript.
- `mnemo_self` — Session self-identification via nonce protocol
- `mnemo_decisions` — Search past decisions (proposal + confirmation pairs) across all sessions. Decisions detected automatically during ingest and backfilled for existing sessions.
- `mnemo_todos` — Query TODO items indexed from `TODO.md` / `todos.md` files across all repos (Obsidian Tasks dialect: 📅 due, ⏳ scheduled, 🛫 start, ✅/❌ done/cancelled, 🔺⏫🔼🔽⏬ priority, 🔁 recurrence, #tags, [[wikilinks]]). Filters compose: repo, status, tag, priority, section, full-text, and date predicates (due before/after/on, overdue, due-soon-N-days, no-date). Each result carries its source `file_path` and `line`. Discovery walks every known root — git repos (from workspace_roots and session cwds) and non-git synthesis roots (planning spaces such as `~/think`) — for default names plus any `todo_globs` in config, honouring `.gitignore` and the loop-safety exclusion fence.
- `mnemo_todo_set` — Edit an existing TODO item in place (status / due / priority) by `id` from `mnemo_todos`. Rewrites only the target line, preserving the rest of the file byte-for-byte; atomic (tmp + fsync + rename) and guarded against concurrent external edits. `done`/`cancelled` stamp today's completion date.
- `mnemo_todo_add` — Append a new TODO item to an already-tracked TODO file (the `file_path` from `mnemo_todos`), optionally filed under a heading (created if absent). Text may carry Obsidian decorations.
- `mnemo_note_post` — Post a cross-session inbox note (🎯T65) for another Claude Code session to pick up. `inbox` is a directory path (absolute, or relative to the calling session's *initial* cwd derived from connection identity — not pwd); `body` is required. `from_session`/`from_repo` default from the MCP connection identity. Inbox canonicalization rejects a leading `~` (shell home-expansion), collapses `./..`, resolves symlinks, and requires the directory to exist (a non-existent inbox errors and inserts no row), so every spelling of one directory addresses one inbox. The producer half of the directory-addressed inbox primitive that supersedes 🎯T42's message bus.
- `mnemo_note_recv` — Receive inbox notes addressed to a directory. Defaults: `unread_only=true`, `mark_read=true` (idempotent — concurrent receivers never double-deliver). Notes are retained after delivery (append-only) and stay browsable via `mnemo_note_list`. Inbox canonicalized identically to `mnemo_note_post`. The consumer half; the `/inbox` skill wraps it, and `/loop /inbox` covers the wait-on-event case.
- `mnemo_note_list` — Browse inbox notes without consuming them. Omit `inbox` to list every inbox touched within the window (default 30 days), newest first; supply `inbox` to scope to one directory. Never marks notes read.
- `mnemo_whatsup` — Live session resource monitor: per-session CPU%, RSS, CPU time correlated with session metadata (repo, topic, work type), plus system memory pressure.
- `mnemo_doctor` — Run mnemo's self-diagnostics (🎯T83) and return a per-check health report: name, severity (ok/warn/fail), tier (fast/full), detail, remediation. Checks the summariser working dir, `claude` on PATH, configured roots, the compaction circuit-breaker (a tripped breaker = every compaction failing systemically), backfill-since-startup, db responsiveness, and **`watch.fds`** (🎯T142: tree-watch backend + process open-FD bound; warn ≥3k, fail ≥8k). A scheduler runs the full suite at startup, fast checks every ~3m, full hourly; fail-severity transitions fire **opt-out, local-only** OS notifications (disable via `disable_health_notifications: true` in config) that deep-link the dashboard health page (`http://localhost:19419/#health`). The same report is served at `GET /health`. (Distinct from the one-shot `mnemo diagnose` CLI subcommand.) Repeatedly-failing background tasks trip a circuit-breaker (🎯T84) and back off instead of hammering.
- `mnemo_define` — Define a reusable parameterised query template with {{param}} placeholders. Templates persist in SQLite across sessions.
- `mnemo_evaluate` — Execute a named query template with parameter values. Returns results like mnemo_query.
- `mnemo_list_templates` — List all saved query templates.
- `mnemo_commits` — Search git commits across all indexed repos. FTS5 on commit messages. Supports repo, author, date range filters. Retroactive: indexes full history from all known repos at startup.
- `mnemo_prs` — Search GitHub PRs and issues across all indexed repos. FTS5 on title/body. Supports state, author, type (pr/issue) filters. Retroactive: backfills from GitHub API at startup.
- `mnemo_discover_patterns` — Workaround patterns suggesting missing features: direct JSONL reads, transcript-dir greps, repeated query shapes, recurring searches. Served from the persisted `patterns` table, refreshed hourly by a reconciler (🎯T64.7), so patterns accumulate a real `first_seen` instead of being re-derived per call, and the reported mine timestamp says how fresh the answer is. `occurrence_count` and `session_count` are different numbers and both are reported — one session that read six transcript files is 6 occurrences across 1 session — and the emission gate is occurrence ≥ 3 across ≥ 2 sessions, because a single session's habit is not yet a pattern. The same gate governs the `_mnemo/patterns/` vault pages and the clustering corpus stream, so the three can never disagree about which patterns exist.
- `mnemo_images` — Search images captured from transcripts. Inline base64 and file-path image references are extracted at ingest, stored as BLOBs with width/height/MIME metadata, and described by AI using surrounding conversation context. Searchable via FTS5 on descriptions. Requires ANTHROPIC_API_KEY for description generation.
- `mnemo_rework_history` — Return prior rework attempts for a bullseye target, ordered most-recent first. Sourced from compaction spans where the target appeared in targets_active or targets_progressed. Returns session_id, timestamp, repo, progress note, prose summary, and open threads. Feed output as `mnemo_history` to `bullseye_rework` to avoid repeating prior failed approaches.
- `mnemo_session_go` — Reopen a past conversation (🎯T125). Resolves a loose reference — a session id or unique prefix, a repo/project fragment, `latest`, or `latest:<scope>` — then opens an iTerm2 tab in the directory that session ran in and resumes it there. An exact id always wins, so the natural flow works: find the session with `mnemo_search`/`mnemo_sessions`, then hand the id back to reopen it. Resumes Claude Code and Grok CLI (`claude --resume` / `grok --resume`, neither forking); Codex/ChatGPT sessions are indexed but refused by name, having no verified terminal resume. A recorded cwd that no longer exists is reported rather than silently substituted. `mnemo resume [<ref>]` is the CLI twin for when no agent is running; both go through the daemon's `POST /api/session/go`, which owns the terminal Automation grant.
- `mnemo_config` — Read or update mnemo's runtime configuration (`~/.mnemo/config.json`) without restarting the daemon. `op=read` returns the current config + resolved paths; `op=write` with a `patch` object merges and persists. Hot-reload covers `vault_path`, `vault_profile`, `vault_bridges`, `vault_bridges_max_links` (🎯T64.5/T64.6, re-derived by rebuilding the vault exporter in place), `workspace_roots`, `extra_project_dirs`, `synthesis_roots`, `plugins` (🎯T102.2 enable/disable); `linked_instances` is persisted but requires a restart.
- `mnemo_vault_sync` — Synchronise the vault: write/update Markdown notes for every session, decision, memory, plan, target, CI run, and PR, then re-ingest the vault so human-added notes and below-fence edits are searchable. Up-to-date notes are skipped. Requires `vault_path`.
- `mnemo_vault_status` — Report vault configuration: enabled state, root path, active indexing scope + `.mnemoignore` state, active `vault_layout` (v1/both/v2) with soak recommendation, note counts by section, the detected/overridden PKM `vault_profile` (🎯T64.5), and the configured bridges (🎯T64.6).
- `mnemo_vault_migration_doc` — Return or (with `write=true`) regenerate `_mnemo/MIGRATION.md`, the once-written v1→v2 explainer. Preview-only by default. Requires `vault_path`.
- `mnemo_vault_bridge_list` — List the vault bridges mnemo maintains (🎯T64.6): each configured collection (themes/patterns/cross-repo/lessons/decisions/memories) → anchor file, whether its fenced block is written yet, plus any per-bridge errors from the last sync. Configured via `vault_bridges` + `vault_bridges_max_links`.
- `mnemo_vault_gc` — Inspect (and with `confirm=true`, clean up) vault GC orphans: `manifest_path_missing` rows (removable) and `disk_not_in_manifest` files (informational only — user content). Dry-run by default.

## Code Structure

```
mnemo/
├── main.go                 # Entry point: HTTP MCP daemon
├── internal/
│   ├── store/              # SQLite FTS5 index, ingest, search
│   │   ├── store.go        # Database operations
│   │   └── iface.go        # Backend interface
│   ├── compact/            # Background /clear-span compactor
│   └── tools/
│       ├── tools.go        # MCP tool definitions and handlers
│       └── mcp.go          # Adapter that registers tools on the MCP server
```

## Testing

```bash
go test -tags "sqlite_fts5" ./...
```

## Schema policy

The schema of `~/.mnemo/mnemo.db` is an append-only contract.

**Allowed**: new tables, new columns (nullable, or `NOT NULL` with a
`DEFAULT`), new indexes, new views, modified trigger bodies (if
sqlift can express the modification without a destructive op).
Trigger and generated-column *expressions* may evolve, but their
effect must remain reproducible by older binaries reading the data.

**Forbidden**: dropped columns, dropped tables, type changes, added
`NOT NULL` / `UNIQUE` / `CHECK` constraints on existing columns,
*relaxed* constraints on existing columns (`NOT NULL` → nullable
is implemented via SQLite's 12-step rebuild and is also disallowed),
column reorders, anything that would make an older binary crash or
lose data when reading the new DB.

**Migration runner**: schema upgrades are mediated by **sqlift v0.14+**.
The previous wipe-and-reingest path (schema-version mismatch →
delete `mnemo.db` and reindex from `~/.claude/projects/`) is gone:
some users' source JSONL has been pruned by Claude Code, so a wipe
is permanent data loss.

sqlift v0.14 has four independent gates:

- `AllowRebuild` — permits SQLite's 12-step rebuild (column type
  changes, dropping CHECK/FK constraints, reordering columns).
- `AllowDestructive` — permits drops (`DROP TABLE`, `DROP COLUMN`,
  fully removing a trigger/view/index).
- `AllowLoosen` — permits rebuilds whose *only* changes are strict
  constraint relaxations (`NOT NULL` → nullable, drop CHECK/FK).
- `AllowDataDependent` — permits changes whose success depends on
  existing data (nullable → NOT NULL, new NOT NULL column without
  DEFAULT, new FK/CHECK on an existing table).

mnemo invokes sqlift with `sqlift.ApplyOptions{}` (= `AllowNone`),
**always**. All four gates stay off — no globally, no per-migration,
no exceptions. This is the strictest setting sqlift offers.

What `AllowNone` allows, and is sufficient for every forward
evolution we realistically need:

- `CREATE TABLE`
- `ALTER TABLE ADD COLUMN` (nullable, or `NOT NULL` with `DEFAULT`)
- `CREATE INDEX` / `CREATE VIEW` / `CREATE TRIGGER`
- **Modifying a trigger body** — sqlift emits `DROP+CREATE` but the
  `DROP` is classified non-destructive when the same-named trigger
  appears in the desired schema (`dist/sqlift.cpp:1424`).

Anything that needs a flag — including the cleaner-looking `NOT NULL`
→ nullable on `messages.text` — must be redesigned. Encode the new
shape in a *new* nullable column with a sentinel value or a flag
column, not by modifying the existing one.

**Deprecating data — three-phase strategy.**

The append-only rule does not mean "data can never be removed". It
means data removal is decoupled from schema change.

1. **Phase 1 (additive release).** Add new columns/views to support
   the new shape. Stop *writing* the deprecated content. Create a
   new view (with a *new name* — do not rename existing tables) that
   exposes reads consistently across both old and new rows. Modify
   triggers as needed. The deprecated columns stay in the schema
   with their existing data intact.
2. **Phase 2 (soak).** Wait several releases / a defined period.
   The new code path proves itself in production.
3. **Phase 3 (GC, not a migration).** Once trusted, an in-product
   garbage-collection pass nullifies the deprecated columns on
   existing rows after verifying the new source has equivalent
   content. This is **product code, not a schema migration** —
   sqlift has no hook for application-side verification. The GC is
   user-triggered or scheduled, customisable in scope, and
   idempotent. The columns themselves are still not dropped.

## External API egress

mnemo is conservative about outbound calls to hosted APIs. The
ingest path is fully local (filesystem only); only a few features
talk to external services, and each is gated independently.

**Anthropic Admin API (cost reconciliation, 🎯T63).** Disabled by
default. The reconciler — which fetches authoritative daily costs
from the Admin API to populate `reconciled_costs` — only starts when
**both** of these hold:

1. `cost_reconciliation.enabled: true` is set in `~/.mnemo/config.json`.
2. `ANTHROPIC_ADMIN_API_KEY` is present in the daemon's environment.

The key alone is not sufficient. A user who keeps the key around for
other tooling will **not** trigger any mnemo Admin API call until
they explicitly opt in via config. The estimated-cost path (derived
from transcript tokens by the ingest layer) is always on and
requires zero external calls — the default `mnemo_usage` view runs
purely off local data.

This default exists for users operating in environments where
unsolicited outbound calls to hosted APIs require security-team
review. Opt-in must be deliberate; defaults are silent.

**GitHub API.** Used by the PR/issue/CI backfill workers, and driven by
**agent-session discovery** (🎯T117). mnemo is a session tracking tool,
not a code management tool: it collects data for the repos its sessions
were connected to, and never goes looking for repos on its own. A
checkout mnemo has not seen a session in is never contacted, however
plausibly it is laid out. (This retires 🎯T17, which additionally walked
the workspace so an untouched project could be polled — on a real
machine that meant 70 of 147 repos were contacted purely because a
directory existed.) No org-scope fan-out; no secret material required
(relies on the local `gh` auth).

Within that set, a repo is only fetched when it is genuinely a GitHub
checkout: identity comes from its configured `origin` remote, never
from its path (🎯T116). A never-pushed `git init` scaffold, a backup
copy, or a checkout whose remote points somewhere other than GitHub
produces no outbound call. Git worktrees and prefix-named local clones
(`foo.experiment` beside `foo`) resolve to the repo they actually
belong to, so a session in a worktree collects for its parent repo,
once, under the right name.

The same session-driven bound applies to the local `commits` stream.
File discovery for docs, todos, plans and targets is separate and
still walks git repos, synthesis roots, and session cwds.

**Model rate card (pricing, 🎯T135).** Disabled by default. Costing
model calls needs per-model prices, and those are the one input that
cannot be derived from transcripts mnemo already holds. They are fetched
as a single HTTP GET of a public, community-maintained rate file
(~1.6 MB, ~2,983 models), cached at `~/.mnemo/pricing.json` with a dated
snapshot archived under `~/.mnemo/pricing/`.

Nothing is fetched until you opt in:

```json
{ "pricing": { "enabled": true } }
```

The flag is read per attempt, so it takes effect — in both directions —
without a daemon restart. `source_url` overrides the upstream for an
air-gapped mirror or a site-pinned copy; `refresh_hours` sets the
interval (default 24).

With it off, token counts remain exact and **every model reports as
unpriced**. That is deliberate rather than a degradation: a cost computed
from no rate card would be `$0.00`, and zero is the one answer
indistinguishable from "you spent nothing". `mnemo_budget` and the
`budget.projection` health check both say "unpriced" rather than showing
a figure.

The dated archive is what makes pricing **contemporaneous**: a record is
priced with the card that was in force when it was written, so a price
revision cannot propagate backwards into a settled period. Applying
today's card to last January's tokens is an error in the *opposite*
direction from staleness — a prochronism — and an invisible one, since
prices are stable after release and a blanket recompute is usually
approximately right.

**Federation (🎯T15 / linked instances).** Outbound calls to peer
mnemo daemons are gated on `linked_instances` being non-empty in
config. Absent → zero federation calls.

**PyPI + HuggingFace Hub (image embeddings, 🎯T20).** Disabled by
default. The CLIP image embedder shells out to `uv run --script
tools/embed-clip/embed.py`; that subprocess resolves the script's
Python dependencies (`sentence-transformers`, `torch`, `pillow`) from
PyPI and, on first use, downloads the `clip-ViT-B-32` model weights
(~340 MB) from the HuggingFace Hub, unauthenticated. In practice the
caches land around 1.6 GB (uv) plus ~580 MB (HuggingFace).

Nothing is fetched until you opt in (🎯T121):

```json
{ "image_embeddings": { "enabled": true } }
```

Same posture, and the same reasoning, as `cost_reconciliation`: having
`uv` installed is not consent to download a couple of gigabytes from
two package hosts. With the section absent — or the config unreadable —
no embedding subprocess is spawned at all, so there is no PyPI
resolution and no weight download regardless of what is installed. The
flag is read per attempt, so toggling it takes effect without a daemon
restart.

Two further preconditions apply once enabled: `uv` must be on the
daemon's PATH, and `tools/embed-clip/embed.py` must resolve — it ships
**only in the source tree**, never in a release archive or the Homebrew
bottle. A packaged install therefore cannot make this call even when
opted in.

Only embedding-backed image search (`semantic` / `similar`) depends on
this. Image extraction, OCR, AI descriptions and FTS are unaffected.
Whether the embedder ran or was skipped, and why, is reported by the
`images.embedder` check in `mnemo_doctor` / `GET /health`; per-image
outcomes are in the `image_embedding_attempts` table.

The append-only schema policy and the opt-in egress posture compose:
restoring an older backup never silently triggers a backfill of
data from external APIs that the user did not authorise.

## Budgeting and cost control

Three pieces, in dependency order.

**🎯T135 measures.** Deduplicated per-record token counts, priced from a
fetched rate card by exact model match, with cache writes split by TTL
tier (73% of volume is the long tier, which prices higher) and
long-context rates applied **per request** rather than per aggregate
(applying the 200k threshold to a daily total roughly doubles the bill).
Sources with no dedup key — Codex and Grok today — are quarantined and
reported separately rather than summed into a figure that claims to be
deduplicated. Validated against ccusage as a build-tagged regression
oracle: `go test -tags "sqlite_fts5 ccusage" ./internal/store/`.

**🎯T136 controls, partially.** Once a budget's soft limit is breached,
mnemo throttles the agents it invokes itself — compactor, segmenter,
reviewer, image description — and nothing else. Work started from Claude
Code, and the sub-agent fan-outs it spawns, passes through nothing mnemo
can gate. That asymmetry is the premise, not a limitation: observation is
universal, control is partial, and the user closes the gap. `mnemo_budget`
reports `governed_usd` beside the total so a rising headline alongside an
active throttle does not read as a broken throttle.

Throttling is post hoc and soft (a delay between runs, never a refusal),
ordered by time-insensitivity rather than cost (backfill first), with the
streaming segmenter **paused** rather than slowed — a drip costs ~45k
input tokens regardless of payload, so half-rate pays nearly the same
money for spans too late to be fresh. State is durable across restarts,
recovery needs a 10-point margin, and the governor **refuses to act** when
the number is untrustworthy.

**🎯T137 attributes.** Agent trees, costed as a whole, because a fan-out
of forty modest agents is invisible to a per-session ranking.

Config:

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

`dedup_key` is configurable because its validity is environment-dependent:
against a provider's own API `message_request` is right, but behind a
gateway (Bedrock+Portkey has been reported to diverge) a proxy may retry,
coalesce or reissue identifiers. Validate it by reconciling against the
billing source for each serving path. An unrecognised value resolves to
the default rather than being honoured — a typo that quietly disabled
deduplication would inflate every figure ~2x with nothing on screen to
suggest it.

Full specification: `docs/design/token-cost-specification.md`.

## Convergence

Targets live in `bullseye.yaml` at the repo root, managed by the
bullseye MCP server (`bullseye_list`, `bullseye_put`,
`bullseye_retire`, etc.). This matches the global convention.

The `mnemo_targets` MCP tool — which reads `docs/targets.md` across
all repos — does **not** index `bullseye.yaml` today, so mnemo's own
targets are invisible to it. If you want a cross-project target
search that includes mnemo, use `bullseye_list(cwd)` directly until
the indexer is taught to read `bullseye.yaml`.

## Gates

profile: mnemo

The `mnemo` gate profile (`~/.claude/gates/mnemo.yaml`, merged over
`base.yaml`) adds a **`windows-vm-validated`** pre-merge gate: the
Windows build + test suite is validated on the local Parallels VM
(`ssh hms-vm`, Win11 ARM64) by `scripts/win-validate.sh`, not by the
cloud CI Windows job. Cloud `test (windows-latest …)` still runs on
every PR but is **non-required** (removed from the master ruleset's
required checks) — it's the slow ~15-18 min CGO/SQLite long pole, so
it no longer gates the merge.

Flow is **push-first**: push the branch, then run
`scripts/win-validate.sh` (it validates the *pushed* commit), which
builds mnemo + sqldeep with the clang/ARM64 CGO toolchain on the VM
and runs `go test -tags sqlite_fts5 ./...` — far faster than the cloud
job (native ARM64, ~1-2 min). One-time VM provisioning: Go, MSYS2
CLANGARM64 toolchain (`clang`/`llvm-ar`), and
`mingw-w64-clang-aarch64-sqlite3`.

## Delivery

Merged to master via squash PR.
