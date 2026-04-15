# Stability

## Stability commitment

Once mnemo reaches 1.0, backwards compatibility becomes a binding
contract. Breaking changes to the CLI interface, MCP tool signatures,
configuration, or database schema will not be made without forking to a
new product. The pre-1.0 period exists to get these surfaces right.

## Interaction surface catalogue

Snapshot as of v0.17.0.

### CLI flags

| Flag | Type | Default | Stability |
|---|---|---|---|
| `--addr` | string | `:19419` | Stable |
| `--version` | bool | false | Stable |
| `--help-agent` | bool | false | Stable |

### MCP tools

#### mnemo_search

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | yes | Search query — plain words use OR (fuzzy), explicit AND/NOT/NEAR/quotes for precise control | Stable |
| `limit` | number | no | Max results (default 20) | Stable |
| `session_type` | string | no | Filter: interactive, subagent, worktree, ephemeral, all (default interactive) | Stable |
| `repo` | string | no | Repo filter (bare name, org/repo, or path fragment) | Stable |
| `context_before` | number | no | Messages before each hit (default 3) | Stable |
| `context_after` | number | no | Messages after each hit (default 3) | Stable |
| `context_filter` | string | no | "substantive" (default) or "all" | Needs review |

#### mnemo_sessions

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `session_type` | string | no | Filter by session type (default interactive) | Stable |
| `min_messages` | number | no | Min substantive messages (default 6) | Needs review |
| `limit` | number | no | Max sessions (default 30) | Stable |
| `project` | string | no | Project name substring filter | Stable |
| `repo` | string | no | Repo org/name substring filter | Stable |
| `work_type` | string | no | Work type filter | Needs review |

**Notes**: `min_messages` default of 6 may need tuning. `work_type`
values are heuristically extracted and the set may evolve. As of v0.16.0,
live sessions (detected via `lsof` with a 5-second cache) are annotated
with `[LIVE pid=NNNNN]` in the output.

#### mnemo_read_session

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `session_id` | string | yes | Session ID or prefix | Stable |
| `role` | string | no | Filter: user, assistant | Stable |
| `offset` | number | no | Skip first N messages (default 0) | Stable |
| `limit` | number | no | Max messages (default 50) | Stable |

#### mnemo_recent_activity

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 7) | Stable |
| `repo` | string | no | Repo filter (name or path fragment) | Stable |

**Added in v0.8.0.** Returns structured JSON per repo. **Stability**: Needs review — output shape may evolve.

#### mnemo_status

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 7) | Stable |
| `repo` | string | no | Repo filter | Stable |
| `max_sessions` | number | no | Max sessions per repo (default 3) | Needs review |
| `max_excerpts` | number | no | Max message excerpts per session (default 20) | Needs review |
| `truncate_len` | number | no | Assistant message truncation length (default 200) | Needs review |

**Added in v0.8.0.** Returns hierarchical JSON (repos → sessions → excerpts). **Stability**: Needs review — defaults and output shape may evolve.

#### mnemo_memories

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `type` | string | no | Filter: user, feedback, project, reference | Needs review |
| `project` | string | no | Project name substring filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.11.0.** Searches across auto-memory files from all projects.
Returns name, description, type, project, content. **Stability**: Needs
review — first release, output format and filters may evolve.

#### mnemo_usage

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 30) | Stable |
| `repo` | string | no | Repo filter (name or path fragment) | Stable |
| `model` | string | no | Model prefix filter (e.g. "claude-opus-4") | Needs review |
| `group_by` | string | no | Group by: day (default), model, repo | Needs review |

**Added in v0.11.0.** Returns aggregated token usage with cost estimates.
**Stability**: Needs review — cost model and grouping options may evolve.

#### mnemo_skills

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `~/.claude/skills/*.md`. **Stability**: Needs review.

#### mnemo_configs

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches CLAUDE.md files from all repos. **Stability**: Needs review.

#### mnemo_audit

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `skill` | string | no | Skill name filter (e.g. "release") | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `docs/audit-log.md` from all repos. **Stability**: Needs review.

#### mnemo_targets

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `status` | string | no | Status filter: identified, converging, achieved | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `docs/targets.md` from all repos. **Stability**: Needs review.

#### mnemo_plans

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.12.0.** Searches `.planning/` directories from all repos. **Stability**: Needs review.

#### mnemo_who_ran

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `pattern` | string | yes | Command substring to match (LIKE) | Needs review |
| `days` | number | no | Recency window in days (default 30) | Stable |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.13.0.** Searches Bash tool_use entries by command pattern. **Stability**: Needs review.

#### mnemo_permissions

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 30) | Stable |
| `repo` | string | no | Repo filter | Needs review |
| `limit` | number | no | Max results per category (default 20) | Stable |

**Added in v0.13.0.** Analyzes tool_use patterns to suggest allowedTools rules. **Stability**: Needs review.

#### mnemo_ci

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `conclusion` | string | no | Filter: success, failure, cancelled, skipped | Needs review |
| `days` | number | no | Recency window in days (default 30) | Stable |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.13.0.** Indexes GitHub Actions runs from repos in session history. Failed run logs indexed for FTS. **Stability**: Needs review.

#### mnemo_query

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | yes | SQL SELECT/WITH or sqldeep nested syntax | Stable |

**Note**: Accepts plain SQL (SELECT/WITH) and sqldeep nested syntax
(`FROM ... SELECT { }`). sqldeep queries are transparently transpiled.
The SQL schema is an implicit part of this surface. See database schema below.

#### mnemo_repos

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `filter` | string | no | Bare name, org/repo, path fragment, or glob (e.g. `marcelocantos/sql*`) | Needs review |

**Notes**: Returns repo name, filesystem path, session count, and last
activity. Filter matching uses SQL LIKE with `*` mapped to `%`.

#### mnemo_stats

No parameters. Returns session/message counts by type, including a Streams
table showing per-stream ingest state (added in v0.16.0). **Stability**: Stable.

#### mnemo_chain

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `session_id` | string | yes | Any session ID in the chain (or a prefix) | Needs review |

**Added in v0.16.0.** Resolves the full /clear-bounded session chain for a given session. Returns an ordered list of ChainLinks (oldest → newest) with per-session summaries and gap/confidence for each link. Single-element result if no chain is found. **Stability**: Needs review — first release, output format may evolve.

#### mnemo_self

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `nonce` | string | no | Nonce from previous call. Omit to generate. | Needs review |

**Notes**: Two-phase nonce protocol. Nonces detected during ingest and
stored in indexed `session_nonces` table. The mechanism may evolve.

#### mnemo_decisions

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (fuzzy OR matching) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `days` | number | no | Recency window in days (default 30) | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.17.0.** Surfaces past decisions across all sessions.
Detects proposal+confirmation patterns (assistant proposes, user
confirms with "yes"/"lgtm"/etc) at ingest time and stores them in
`decisions` FTS5 table. Retroactive backfill runs at startup. **Stability**:
Fluid — detection heuristic is first-cut and tuning is expected.

#### mnemo_whatsup

No parameters.

**Added in v0.17.0.** Live session resource monitor: per-session CPU%,
RSS, CPU time for active Claude Code processes, plus system memory
pressure (macOS). Cross-references PIDs via `lsof` with session metadata
(repo, topic, work type). **Stability**: Fluid — metric set and output
shape may evolve.

#### mnemo_commits

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (FTS on subject/body) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `author` | string | no | Author name/email substring | Needs review |
| `days` | number | no | Recency window in days (default 30) | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.17.0.** Indexes git commits from all known repos (session_meta
+ workspace walker union) into `git_commits` with FTS5 on subject, body,
repo, and author. Incremental — only fetches new commits per repo since
last ingest. Backfill limited to last 365 days. **Stability**: Needs review.

#### mnemo_prs

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Search query (FTS on title/body) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `state` | string | no | open, closed, merged, all (default all) | Needs review |
| `author` | string | no | GitHub username filter | Needs review |
| `type` | string | no | pr, issue, all (default all) | Needs review |
| `days` | number | no | Recency window in days (default 30) | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |

**Added in v0.17.0.** GitHub PR/issue activity via `gh` CLI across all
repos that appear in session history. Stores in `github_prs` and
`github_issues` with FTS5. Backfill runs in a goroutine at startup (non-
blocking). **Stability**: Needs review — output fields and filter
semantics may evolve.

#### mnemo_discover_patterns

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `days` | number | no | Recency window in days (default 90) | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `min_occurrences` | number | no | Minimum pattern occurrences to report (default 3) | Needs review |

**Added in v0.17.0.** Query-time analysis over the indexed transcript corpus
to find workaround patterns that suggest missing mnemo features. Detects
direct JSONL reads, transcript-directory grep, repeated mnemo_query
shapes (normalised), and recurring mnemo_search queries. Feeds the
"self-improving tool discovery" feedback loop. **Stability**: Fluid —
detection heuristics are first-cut.

#### mnemo_define

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `name` | string | yes | Template name (unique) | Needs review |
| `description` | string | no | What the template does | Needs review |
| `query` | string | yes | SQL with `{{param}}` placeholders | Needs review |
| `params` | array | no | Parameter names referenced in the query | Needs review |

**Added in v0.17.0.** Stores a reusable parameterised query template
in `query_templates`. Upserts on name collision. **Stability**: Needs
review.

#### mnemo_evaluate

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `name` | string | yes | Template name | Needs review |
| `params` | object | no | Parameter values as key-value pairs | Needs review |

**Added in v0.17.0.** Executes a named query template with parameters.
Validates that all declared parameters are supplied; substitutes
`{{param}}` placeholders; delegates to mnemo_query. **Stability**:
Needs review.

#### mnemo_list_templates

No parameters. **Added in v0.17.0.** Lists all saved query templates
with names, descriptions, and parameter definitions. **Stability**:
Stable.

#### mnemo_images

| Parameter | Type | Required | Description | Stability |
|---|---|---|---|---|
| `query` | string | no | Text for 'text' or 'semantic' mode; omit for recent list | Needs review |
| `mode` | string | no | `text` (default), `semantic`, or `similar` | Needs review |
| `similar_to` | number | no | Image ID for `similar` mode | Needs review |
| `repo` | string | no | Repo filter | Needs review |
| `session` | string | no | Session ID prefix filter | Needs review |
| `days` | number | no | Recency window in days (default 90) | Needs review |
| `limit` | number | no | Max results (default 20) | Stable |
| `search_fields` | string | no | `both` (default), `description`, or `ocr` — applies to text mode | Needs review |

**Added in v0.17.0.** Unified image search across three complementary
indexes:

- `text` mode: FTS5 over AI-generated descriptions and OCR text
- `semantic` mode: embeds the query via CLIP (SigLIP/ViT-B-32) and does
  brute-force k-NN cosine search over stored image embeddings
- `similar` mode: visual similarity using the target image's stored
  embedding

Every ingested image triggers three sidecars (OCR via Apple Vision/CGO
or Tesseract, AI description via batched `claude -p` calls, and CLIP
embedding via a local Python helper). Backfill runs at startup; fresh
images process on arrival throttled by a shared `runtime.NumCPU()`
semaphore. **Stability**: Fluid — output format, score field
interpretation, mode set, and descriptor vs OCR balance may evolve.

### Store / Backend interface methods

These methods are part of the `Backend` interface used by both the local `Store`
and the RPC proxy. Breaking changes here would require a protocol version bump.

| Method | Signature | Added | Stability |
|---|---|---|---|
| `LiveSessions` | `() map[string]int` | v0.16.0 | Needs review |
| `Predecessor` | `(sessionID string) (string, error)` | v0.16.0 | Needs review |
| `Successor` | `(sessionID string) (string, error)` | v0.16.0 | Needs review |
| `Chain` | `(sessionID string) ([]ChainLink, error)` | v0.16.0 | Needs review |
| `SearchDecisions` | `(query, repo string, days, limit int) ([]DecisionInfo, error)` | v0.17.0 | Fluid |
| `Whatsup` | `() (*WhatsupResult, error)` | v0.17.0 | Fluid |
| `SearchCommits` | `(query, repo, author string, days, limit int) ([]GitCommit, error)` | v0.17.0 | Needs review |
| `SearchGitHubActivity` | `(query, repo, state, author, activityType string, days, limit int) ([]GitHubActivityResult, error)` | v0.17.0 | Needs review |
| `DiscoverPatterns` | `(days int, repoFilter string, minOccurrences int) ([]PatternCandidate, error)` | v0.17.0 | Fluid |
| `DefineTemplate` | `(name, description, queryText string, paramNames []string) error` | v0.17.0 | Needs review |
| `EvaluateTemplate` | `(name string, params map[string]string) ([]map[string]any, error)` | v0.17.0 | Needs review |
| `ListTemplates` | `() ([]QueryTemplate, error)` | v0.17.0 | Stable |
| `SearchImages` | `(query, repo, session string, days, limit int) ([]ImageSearchResult, error)` | v0.17.0 | Fluid |
| `SearchImagesFiltered` | `(query, repo, session string, days, limit int, searchFields string) ([]ImageSearchResult, error)` | v0.17.0 | Fluid |

**Notes**: `LiveSessions` uses `lsof` to detect active Claude Code processes
and maps session IDs to PIDs, with a 5-second in-process cache. The lsof
dependency and cache TTL are implementation details and may change.
`Predecessor`/`Successor`/`Chain` traverse the `session_chains` table.

### Database schema (exposed via mnemo_query)

| Table/View | Columns | Stability |
|---|---|---|
| `entries` | id, session_id, project, type, timestamp, raw (JSONB) | Needs review |
| `entries` (virtual) | model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, agent_id, version, slug, is_sidechain, data_type, data_command, data_hook_event, top_tool_use_id, parent_tool_use_id | Needs review |
| `messages` | id, entry_id, session_id, project, role, text, timestamp, type, is_noise, content_type, tool_name, tool_use_id, tool_input (JSONB), is_error | Needs review |
| `messages` (virtual) | tool_file_path, tool_command, tool_pattern, tool_description, tool_skill, tool_old_string, tool_new_string, tool_content, tool_query, tool_url, tool_name_param, tool_prompt, tool_subject, tool_status, tool_task_id | Needs review |
| `messages_fts` | FTS5 virtual table matching `messages` (excludes noise) | Stable |
| `snapshot_files` | id, entry_id, session_id, file_path, backup_time — auto-extracted via trigger from file-history-snapshot entries | Needs review |
| `snapshot_files_fts` | FTS5 on file_path | Needs review |
| `sessions` | View joining session_summary + session_meta: session_id, project, session_type, repo, git_branch, work_type, topic, total_msgs, substantive_msgs, first_msg, last_msg | Needs review |
| `session_summary` | Trigger-maintained materialised table: session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg | Needs review |
| `session_meta` | Per-session metadata: session_id, repo, cwd, git_branch, work_type, topic | Needs review |
| `memories` | id, project, file_path (unique), name, description, memory_type, content, updated_at — auto-memory files from ~/.claude/projects/*/memory/*.md | Needs review |
| `memories_fts` | FTS5 on name, description, content, project — with insert/update/delete triggers | Needs review |
| `skills` | id, file_path (unique), name, description, content, updated_at — ~/.claude/skills/*.md | Needs review |
| `skills_fts` | FTS5 on name, description, content | Needs review |
| `claude_configs` | id, repo, file_path (unique), content, updated_at — CLAUDE.md from all repos | Needs review |
| `claude_configs_fts` | FTS5 on content, repo | Needs review |
| `audit_entries` | id, repo, file_path, date, skill, version, summary, raw_text — docs/audit-log.md | Needs review |
| `audit_entries_fts` | FTS5 on summary, raw_text, repo | Needs review |
| `targets` | id, repo, file_path, target_id, name, status, weight, description, raw_text — docs/targets.md | Needs review |
| `targets_fts` | FTS5 on name, description, raw_text, repo | Needs review |
| `plans` | id, repo, file_path (unique), phase, content, updated_at — .planning/**/*.md | Needs review |
| `plans_fts` | FTS5 on content, repo, phase | Needs review |
| `ci_runs` | id, repo, run_id (unique), workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url | Needs review |
| `ci_runs_fts` | FTS5 on repo, workflow, branch, log_summary, conclusion | Needs review |
| `session_chains` | successor_id (PK), predecessor_id, boundary, gap_ms, confidence, mechanism, detected_at — /clear-bounded chain links | Fluid |
| `session_nonces` | nonce → session_id mapping for mnemo_self | Fluid |
| `ingest_state` | path, offset | Fluid |
| `ingest_status` | stream, last_backfill, files_indexed, files_on_disk — per-stream backfill state | Fluid |
| `decisions` | id, session_id, proposal_msg_id, confirmation_msg_id, proposal_text, confirmation_text, repo, timestamp — proposal+confirmation pairs | Fluid |
| `decisions_fts` | FTS5 on proposal_text, confirmation_text, repo | Fluid |
| `query_templates` | id, name (unique), description, query_text, param_names (JSON), created_at, updated_at | Needs review |
| `git_commits` | id, repo, commit_hash, author_name, author_email, commit_date, subject, body — UNIQUE(repo, commit_hash) | Needs review |
| `git_commits_fts` | FTS5 on subject, body, repo, author_name | Needs review |
| `github_prs` | id, repo, pr_number, title, body, state, author, created_at, updated_at, merged_at, url — UNIQUE(repo, pr_number) | Needs review |
| `github_prs_fts` | FTS5 on title, body, repo, author | Needs review |
| `github_issues` | id, repo, issue_number, title, body, state, author, created_at, updated_at, url, labels (JSON) — UNIQUE(repo, issue_number) | Needs review |
| `github_issues_fts` | FTS5 on title, body, repo, author | Needs review |
| `images` | id, content_hash (unique SHA256), bytes (BLOB), original_path, mime_type, width, height, pixel_format, byte_size, created_at | Fluid |
| `image_occurrences` | image_id (FK), entry_id (FK), message_id (FK), session_id, source_type (`inline`\|`path`), occurred_at — UNIQUE(image_id, entry_id, message_id, source_type) | Fluid |
| `image_descriptions` | image_id (unique FK), name, description, model, prompt_tokens, completion_tokens, error, created_at | Fluid |
| `image_descriptions_fts` | FTS5 on name, description | Fluid |
| `image_ocr` | image_id (PK FK), text, backend (`apple_vision`\|`tesseract`), confidence, error, created_at | Fluid |
| `image_ocr_fts` | FTS5 on text | Fluid |
| `image_embeddings` | image_id (PK FK), model, dim, vector (float32 BLOB), error, created_at | Fluid |

**Notes**: v0.9.0 added the `entries` table which stores every JSONL line
as JSONB with 15 virtual columns for high-query fields. All entry types
(user, assistant, progress, system, file-history-snapshot) are now ingested.
`messages` gained `entry_id` FK linking content blocks to their source entry.
v0.10.0 added `snapshot_files` with trigger-based extraction from
file-history-snapshot entries and FTS5 on file paths.
v0.11.0 added `memories` table for cross-project auto-memory indexing,
`mnemo_usage` for token analytics, and changed search to OR-by-default
with BM25 ranking for fuzzy matching.
v0.12.0 added five context source tables: `skills`, `claude_configs`,
`audit_entries`, `targets`, `plans` — each with FTS5 indexes. These
index non-transcript sources (skill files, CLAUDE.md configs, audit
logs, convergence targets, implementation plans) from all known repos.
v0.13.0 added three observability tools: `mnemo_who_ran` (process
attribution), `mnemo_permissions` (permission analysis), `mnemo_ci`
(CI/CD run history). Added `ci_runs` table with FTS5 for GitHub Actions
indexing. `mnemo_usage` gained hourly rate detection.
v0.16.0 (🎯T16) added session chain detection. `session_chains` table
links /clear-bounded JSONL sessions into work spans via a time-gap
heuristic (≤5s, same cwd, /clear marker in successor's first message).
Chain detection runs at ingest time and on startup (backfill). Added
`Predecessor`, `Successor`, `Chain` store methods and the `mnemo_chain`
MCP tool.
v0.17.0 brought a major expansion: decisions (🎯T9.6), mnemo_whatsup
(🎯T9.5), full-fidelity observability (🎯T9), git history indexing
(🎯T11), GitHub PR/issue indexing (🎯T12), self-improving pattern
discovery (🎯T5), query templates (🎯T7), and the full image
indexing stack — storage of decoded image BLOBs, AI descriptions via
batched `claude -p` (🎯T18), Apple Vision OCR via in-process CGO with
Tesseract fallback (🎯T19), and CLIP/SigLIP embeddings for semantic
and visual-similarity search (🎯T20). Schema bumped from 11 to 18 in
six increments. The forward path processes each new image as it
arrives with one shared semaphore throttling all sidecars at
`runtime.NumCPU()`; backfill drains existing content in batched
parallel workers.
v0.15.0 (🎯T17) made every repo-level stream self-heal on startup:
targets, audit logs, plans, CLAUDE.md, and CI polling discover repos
via filesystem walk of configured workspace roots in addition to
session_meta. Added `ingest_status` table, `streams` field on
`StatusResult` and `StatsResult`, `Config` / `LoadConfig` /
`SetWorkspaceRoots` public API, and `~/.mnemo/config.json` optional
config file with `workspace_roots` (default `[~/work]`) and
`extra_project_dirs` keys.
This surface is still evolving. `ingest_state`, `ingest_status`, and
`session_nonces` are internal implementation details.

Entry types: `user`, `assistant`, `progress`, `system`, `file-history-snapshot`.
Content types (messages): `text`, `tool_use`, `tool_result`, `thinking`.

### Output formats

Tool outputs are plain text, not structured JSON. This is intentional
for readability in agent contexts but **Fluid** — structured output may
be added or replace text output before 1.0.

### Configuration

Optional config file at `~/.mnemo/config.json` (since v0.15.0):

```json
{
  "workspace_roots": ["/Users/you/work"],
  "extra_project_dirs": []
}
```

- `workspace_roots` — filesystem roots walked for `.git` entries to
  discover repos. Defaults to `[~/work]` when absent. The workspace
  walker skips `node_modules`, `.venv`, `venv`, `target`, `build`,
  `.build`, `dist`, `.next`, `.cache`, `__pycache__`, `.tox`,
  `.mypy_cache`, and `.pytest_cache`.
- `extra_project_dirs` — reserved for cross-platform transcript ingest
  (🎯T15).

All other configuration is via CLI flags. **Stability**: Fluid — the
config file is new and its schema may grow before 1.0.

### Data storage

- Database location: `~/.mnemo/mnemo.db`
- Transcript source: `~/.claude/projects/` (JSONL files)

Both paths are hardcoded. **Stability**: Needs review — may become
configurable before 1.0.

## Gaps and prerequisites

- **Structured output**: Most MCP tools return plain text. `mnemo_recent_activity`
  and `mnemo_status` return structured JSON; `mnemo_query` with sqldeep
  syntax returns hierarchical JSON. Remaining text-output tools may
  migrate to structured output before 1.0.
- **Configurable paths**: Database and transcript paths are hardcoded.
  Should be configurable via flags or env vars.
- **Session metadata completeness**: repo, work_type, and topic are
  extracted heuristically and may be missing or inaccurate. Extraction
  quality should be audited before locking the schema.

## Out of scope for 1.0

- Multi-user / remote database support
- Authentication / access control on the HTTP endpoint
- Transcript modification or deletion tools
- Integration with non-Claude-Code transcript formats
- Summarisation (live context compaction is tracked as 🎯T10; may land before 1.0 but is not a gate)

**Delivered in v0.17.0** (removed from the 1.0 out-of-scope list):

- Semantic search (via CLIP embeddings for images; 🎯T20) — mnemo_images
  mode=semantic / mode=similar. Text corpus semantic search remains
  out-of-scope for 1.0.
- Agent-defined query templates (🎯T7) — mnemo_define / mnemo_evaluate
  / mnemo_list_templates.
