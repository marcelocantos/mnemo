# Convergence Targets

## Active

### 🎯T1 Broader memory beyond transcripts

- **Value**: 8
- **Cost**: 13
- **Weight**: 0.6 (value 8 / cost 13)
- **Status**: identified
- **Discovered**: 2026-04-06

**Desired state:** mnemo evolves from transcript search into a general
memory system for Claude Code sessions — remembering decisions,
preferences, and project context across sessions. Not just "what was
said" but "what was decided" and "what matters for next time."

**Note:** 🎯T10 (live context compaction) addresses the core of this
target — a summarisation layer that distills sessions into key facts,
available instantly across /clear boundaries. Once 🎯T10 is achieved,
reassess whether 🎯T1 has residual scope or should be retired.

**Open questions:**
- What's the data model beyond raw transcript messages?
- Should it extract and index decisions, action items, code changes?
- How does it relate to Claude's auto-memory (MEMORY.md files)?
- Is there a summarisation layer that distills sessions into key facts? → See 🎯T10.

### 🎯T2 Smarter session classification

- **Value**: 5
- **Cost**: 3
- **Weight**: 1.7 (value 5 / cost 3)
- **Status**: achieved
- **Discovered**: 2026-04-06
- **Achieved**: 2026-04-06

**Desired state:** Session classification goes beyond path-based
heuristics. mnemo understands what a session was *about* — which
repo, what kind of work (bug fix, feature, review, refactor), key
topics discussed.

**Current:** Classification is purely path-based (interactive/subagent/
worktree/ephemeral). No awareness of content or purpose.

**Acceptance criteria:**
- Sessions tagged with repo(s) they operated on.
- Sessions tagged with work type (feature, bugfix, review, etc.).
- Key topics extracted and searchable.
- `mnemo_sessions` supports filtering by repo and work type.

### 🎯T3 Active work dashboard data

- **Value**: 8
- **Cost**: 5
- **Weight**: 1.6 (value 8 / cost 5)
- **Status**: achieved
- **Discovered**: 2026-04-06
- **Achieved**: 2026-04-07
- **Related**: jevon 🎯T16.1 (active work dashboard)

**Desired state:** mnemo exposes an API for cross-referencing recent
transcript sessions with external signals (dirty working trees, open
PRs) to produce a unified view of where active work is happening.

**Acceptance criteria:**
- `mnemo_recent_activity` tool returns per-repo summary of recent
  session activity (last session time, message count, key topics).
- Output is structured (JSON) so consumers (jevon, other tools) can
  merge it with git/GitHub signals.
- Configurable recency window (default: 7 days).

### 🎯T4 Individual session transcript access

- **Value**: 5
- **Cost**: 3
- **Weight**: 1.7 (value 5 / cost 3)
- **Status**: achieved
- **Discovered**: 2026-04-06
- **Achieved**: 2026-04-06

**Desired state:** mnemo can read and search within individual session
transcripts. Absorbs jevon's `transcript_read` functionality.

Transcript files are permanent archaeological records — mnemo never
modifies or truncates them. Future work may add compressed archival
with mnemo preserving the index, but the raw files stay intact.

**Acceptance criteria:**
- `mnemo_read_session` tool returns messages from a specific session ID.
- Supports filtering by role, offset, limit.
- Works on raw JSONL files, not just the indexed database.
- No mutation of transcript files.

### 🎯T5 Self-improving tool discovery

- **Value**: 9
- **Cost**: 8
- **Weight**: 1.1 (value 9 / cost 8)
- **Status**: identified
- **Discovered**: 2026-04-06

**Desired state:** mnemo can mine its own transcript index to discover
patterns that suggest missing features — sessions that read JSONL files
directly, grep through `~/.claude/projects/`, use manual workarounds,
or run repeated `mnemo_query` shapes. This feeds the feedback loop:
general facility → observe usage → promote patterns to dedicated tools.

**Acceptance criteria:**
- A tool (e.g., `mnemo_discover_patterns`) that identifies sessions
  where agents worked around mnemo's limitations.
- Detects: direct JSONL file reads, grep/rg over transcript dirs,
  repeated query shapes, manual transcript analysis.
- Output: candidate features with evidence (session IDs, frequency,
  what the agent was trying to accomplish).
- Integrates with the define/evaluate template system (🎯T7) to
  suggest promotable query patterns.

### 🎯T6 Session self-identification

- **Value**: 6
- **Cost**: 5
- **Weight**: 1.2 (value 6 / cost 5)
- **Status**: achieved
- **Achieved**: 2026-04-06
- **Discovered**: 2026-04-06

**Desired state:** A session can easily discover its own transcript and
query it. An agent should be able to ask "what did I say earlier in
this session?" without knowing its session ID upfront.

**Open questions:**
- MCP requests don't carry session identity. How does mnemo know which
  session is calling? Options: agent passes ID (but doesn't know it),
  timestamp/content heuristic (fragile), Claude Code injects session
  context into MCP requests (upstream change), or mnemo resolves it
  from the active JSONL file being written to.
- Could mnemo watch for the currently-being-written JSONL file and
  expose it as "current session"?

**Acceptance criteria:**
- An agent can retrieve its own session's messages in a single tool
  call without prior knowledge of its session ID.
- Works reliably (not a fragile heuristic).

### 🎯T7 Agent-defined query templates

- **Value**: 7
- **Cost**: 8
- **Weight**: 0.9 (value 7 / cost 8)
- **Status**: identified
- **Discovered**: 2026-04-06
- **Related**: 🎯T5 (pattern discovery feeds template suggestions)

**Desired state:** Agents can define reusable parameterised query
templates (`mnemo_define`) and call them later (`mnemo_evaluate`).
Templates persist in the database across sessions. Complex queries
that prove useful get saved once and reused, rather than being
reinvented each session.

**Acceptance criteria:**
- `mnemo_define` stores a named, parameterised query template.
- `mnemo_evaluate` executes a template by name with parameters.
- `mnemo_list_templates` shows available templates.
- Templates persist in SQLite across sessions.
- `mnemo_query` nudges agents to define templates when query complexity
  exceeds a threshold (e.g., multiple joins/subqueries).

### 🎯T9 Full-fidelity ingest and observability tools

- **Value**: 9
- **Cost**: 8
- **Weight**: 1.1 (value 9 / cost 8)
- **Status**: identified
- **Discovered**: 2026-04-07
- **Related**: 🎯T5 (pattern discovery), 🎯T3 (dashboard data)
- **Census**: `/tmp/field-census.txt` (1.3M entries, 10,766 paths, 3.4 GB)

**Desired state:** mnemo ingests all JSONL fields (not just
user/assistant message content) and exposes observability tools built
on the full data.

**Context:** A field census (scripts/field-census.py) revealed that
mnemo discards ~70% of JSONL data: token usage (366k assistant
entries), progress events (623k entries — bash output, hook events,
agent progress), tool results with structured patches, model info,
stop reasons, Claude Code version, agent IDs, and more.

**Sub-targets:**

#### 🎯T9.1 Full-fidelity ingest

- **Value**: 8
- **Cost**: 5
- **Weight**: 1.6 (value 8 / cost 5)
- **Status**: achieved
- **Achieved**: 2026-04-07
- **Parent**: 🎯T9

Ingest all top-level JSONL fields and message sub-fields. Key
additions: `usage` (token counts per response), `model`, `stop_reason`,
`version`, `slug`, `agentId`, `data.*` (progress events),
`toolUseResult.*` (structured tool results). Store the full entry as
JSONB where practical; add virtual columns for high-query fields.
Requires schema version bump (full re-index).

**Implementation:** New `entries` table stores every JSONL line as
JSONB with 15 virtual columns. All entry types ingested (progress,
system, file-history-snapshot). Messages linked via `entry_id` FK.
Schema version 5.

#### 🎯T9.2 Token usage analytics (`mnemo_usage`)

- **Value**: 8
- **Cost**: 3
- **Weight**: 2.7 (value 8 / cost 3)
- **Status**: identified
- **Parent**: 🎯T9
- **Gates**: 🎯T9.1

Report token consumption by day, repo, session, model — with cost
estimates at current pricing. Data comes from `message.usage` fields
(input_tokens, output_tokens, cache_read, cache_creation). Should
support: daily totals, per-repo breakdown, per-model breakdown,
hourly rate detection ("am I spending too fast?").

#### 🎯T9.3 Permission prompt analysis (`mnemo_permissions`)

- **Value**: 5
- **Cost**: 3
- **Weight**: 1.7 (value 5 / cost 3)
- **Status**: identified
- **Parent**: 🎯T9
- **Gates**: 🎯T9.1

Identify most-used tools and frequent approval patterns from
tool_use/tool_result message pairs. Suggest `allowedTools` rules
for settings.json. "You approved Bash 90k times — consider adding
`Bash(~/work/**)` to your allowed list."

#### 🎯T9.4 Process attribution (`mnemo_who_ran`)

- **Value**: 5
- **Cost**: 2
- **Weight**: 2.5 (value 5 / cost 2)
- **Status**: identified
- **Parent**: 🎯T9
- **Gates**: 🎯T9.1

Given a command pattern (e.g., "make", "clang", "python"), find which
session(s) ran it recently. Answers "which session is hogging CPU?"
by matching against `tool_command` in recent Bash tool_use entries.

#### 🎯T9.5 System correlation (`mnemo_whatsup`)

- **Value**: 5
- **Cost**: 5
- **Weight**: 1.0 (value 5 / cost 5)
- **Status**: identified
- **Parent**: 🎯T9
- **Gates**: 🎯T9.1

Correlate current system state (high CPU, fan spinning, disk I/O)
with active mnemo sessions. Runs `top`/`ps` to find heavy processes,
cross-references PIDs and command patterns against recent session
activity. Answers "what's eating CPU?" with "session X in repo Y
has been running a make build for 3 minutes."

#### 🎯T9.6 Cross-session decision recall (`mnemo_decisions`)

- **Value**: 8
- **Cost**: 5
- **Weight**: 1.6 (value 8 / cost 5)
- **Status**: identified
- **Parent**: 🎯T9
- **Gates**: 🎯T9.1

Surface past decisions across all sessions. During ingest, detect
decision patterns: user confirmation ("yes", "lgtm", "go", "do it",
"that works") following a substantive assistant proposal. Store the
pair (proposal + confirmation) in a `decisions` table with FTS5
indexing on the proposal text.

`mnemo_decisions` searches the decisions table — "relay protocol"
finds the proposal where the assistant laid out the relay design and
the user confirmed, even though "decide" never appears in the text.

No RAG/embeddings needed for v1. The detection heuristic + dedicated
FTS table covers the common case. Embeddings can upgrade ranking
later if keyword search proves insufficient.

**Acceptance criteria:**
- Field census shows 0 unindexed high-frequency fields (> 1% of entries).
- `mnemo_usage` returns daily token breakdown with cost estimates.
- `mnemo_permissions` suggests concrete allowedTools rules.
- `mnemo_who_ran "make"` returns session + repo + timestamp.
- `mnemo_whatsup` correlates system load with session activity.
- `mnemo_decisions "relay protocol"` returns the proposal + confirmation
  with session context.

### 🎯T11 Git history indexing

- **Value**: 8
- **Cost**: 5
- **Weight**: 1.6 (value 8 / cost 5)
- **Status**: identified
- **Discovered**: 2026-04-07
- **Related**: 🎯T3 (dashboard data), 🎯T9.6 (decision recall)

**Desired state:** mnemo indexes Git commit history from all repos
that appear in session transcripts and exposes cross-repo, corpus-level
queries. Single-repo git operations (log, blame, diff) are already
well-served by Claude Code's built-in tools — mnemo's value is in
indexed search across the entire corpus and session-commit correlation.

Agents can ask "which repos had auth-related commits this week?",
"show all commits across all projects by this author", or "what was
the session context when this change was made?" — queries that span
repos and join code history with conversation history.

**Architecture sketch:**

The daemon discovers repos from `session_meta.cwd` paths and
periodically runs `git log` to ingest commit metadata into a `commits`
table. Cross-referencing uses timestamp overlap, branch name matching,
and cwd correlation — a commit on branch `fix/auth-bug` at 10:05 likely
came from the session on the same branch that was active at 10:05.

Key fields: hash, repo, author, timestamp, branch, message, files
changed, insertions, deletions. Stored alongside a `commit_files`
table for per-file change tracking.

**Tools:**

- `mnemo_commits` — cross-repo commit search with filters (repo glob,
  author, date range, file path pattern, message FTS). Returns commit
  metadata with correlated session IDs. The cross-repo and FTS
  capabilities are the differentiators vs built-in `git log`.

**Acceptance criteria:**
- Commits from repos in `session_meta` are indexed automatically.
- `mnemo_commits` supports cross-repo queries (repo glob, date range).
- FTS5 index on commit messages enables keyword search across corpus.
- Commit data queryable via `mnemo_query` (joins with sessions/entries).
- Incremental — only fetches new commits since last ingest.

### 🎯T12 GitHub activity indexing

- **Value**: 8
- **Cost**: 5
- **Weight**: 1.6 (value 8 / cost 5)
- **Status**: identified
- **Discovered**: 2026-04-07
- **Related**: 🎯T11 (git history), 🎯T9.6 (decision recall)

**Desired state:** mnemo indexes GitHub activity (PRs, issues, reviews,
comments) from repos that appear in session transcripts. Agents can
search across the full corpus — "which PRs across all my repos are
stale?", "what did the reviewer say about the auth approach?", "find
all issues mentioning performance regression."

The `gh` CLI queries one repo at a time and returns ephemeral results.
mnemo's value is corpus-level FTS search and cross-referencing with
session context and git history.

**Architecture sketch:**

The daemon periodically polls GitHub via `gh api` for repos discovered
from `session_meta`. Ingests PRs (title, body, state, author, reviewers,
merge status), PR reviews and comments, and issues into dedicated tables.
FTS5 on PR/issue bodies and comments.

**Tools:**

- `mnemo_prs` — cross-repo PR search with filters (repo glob, state,
  author, reviewer, date range, body/title FTS). Returns PR metadata
  with correlated session IDs and commit hashes.
- `mnemo_issues` — cross-repo issue search with similar filters.

**Acceptance criteria:**
- PRs and issues from repos in `session_meta` indexed automatically.
- `mnemo_prs` supports cross-repo queries with FTS on title/body.
- PR reviews and comments indexed and searchable.
- Correlated with sessions (by repo + time overlap) and commits (by merge SHA).
- Queryable via `mnemo_query` (joins with sessions/entries/commits).
- Incremental — only fetches activity since last poll.

### 🎯T13 CI/CD history and statistics

- **Value**: 8
- **Cost**: 3
- **Weight**: 2.7 (value 8 / cost 3)
- **Status**: identified
- **Discovered**: 2026-04-07
- **Related**: 🎯T12 (GitHub activity), 🎯T11 (git history)

**Desired state:** mnemo indexes CI/CD run history (GitHub Actions)
and exposes cross-repo queries over build outcomes. Agents can ask
"has this test failed before? what fixed it?", "which repos have been
red this week?", "what's my CI success rate by repo?"

GitHub Actions logs are ephemeral (90-day retention) and per-repo.
mnemo preserves them permanently and makes them searchable across the
full corpus, correlated with the sessions and commits that triggered
them.

**Architecture sketch:**

The daemon polls `gh api` for workflow runs from repos in `session_meta`.
Stores run metadata (workflow name, status, conclusion, duration, trigger
event, head SHA, branch) in a `ci_runs` table. For failed runs, fetches
and stores the failed job log summary. FTS5 on failure messages.

**Tools:**

- `mnemo_ci` — cross-repo CI query with filters (repo glob, workflow,
  conclusion, date range, branch). Returns run metadata with correlated
  sessions and commits.
- Failure pattern detection: "this test has failed 3 times this week
  in 2 different repos."

**Acceptance criteria:**
- CI runs from repos in `session_meta` indexed automatically.
- `mnemo_ci` supports cross-repo queries with status/conclusion filters.
- Failed run logs indexed with FTS for "has this failed before?" queries.
- Correlated with commits (by head SHA) and sessions (by repo + time).
- Queryable via `mnemo_query`.
- Incremental polling with configurable interval.

### 🎯T14 File-history-snapshot surfacing

- **Value**: 5
- **Cost**: 2
- **Weight**: 2.5 (value 5 / cost 2)
- **Status**: identified
- **Discovered**: 2026-04-07
- **Related**: 🎯T9.1 (full-fidelity ingest — snapshots already stored)

**Desired state:** The `file-history-snapshot` entries already stored
in the `entries` table (26k+ entries) are surfaced as a queryable tool.
Agents can track which files existed at session boundaries and how the
working tree evolved across sessions, without running per-file git
queries.

This is low-cost because the data is already ingested — it just needs
extraction logic and a dedicated tool or view.

**Architecture sketch:**

Extract file lists from `entries.raw` where `type = 'file-history-snapshot'`
into a `file_snapshots` table (session_id, timestamp, file_path, status).
Or expose via a view/virtual columns on the existing entries table if
the JSON structure is simple enough.

**Tools:**

- `mnemo_files` — query file presence/modification across sessions.
  "Which sessions touched store.go?", "What files were in the working
  tree during session X?"

**Acceptance criteria:**
- File-history-snapshot data queryable via dedicated tool or view.
- Cross-session file tracking: "which sessions touched this file?"
- Queryable via `mnemo_query`.
- No additional ingest needed — data already in `entries` table.

### 🎯T10 Live context compaction

- **Value**: 10
- **Cost**: 8
- **Weight**: 1.25 (value 10 / cost 8)
- **Status**: identified
- **Discovered**: 2026-04-07
- **Related**: 🎯T1 (subsumes "broader memory"), 🎯T9.6 (decision recall becomes a compaction output)
- **Depends**: jevon `claude.Process` / `manager.Manager` for Claude instance lifecycle

**Desired state:** mnemo maintains a live compacted context for each
active session. When a session `/clear`s (or a new session starts in
the same project), the compacted context is available instantly via
`mnemo_restore` — no multi-round search/summarize needed. The /clear
firewall becomes nearly free.

**Architecture:**

The mnemo daemon spawns a Sonnet summarizer instance (via jevon's
`Manager`/`Process` API) per active session. The summarizer:

1. Receives fixed-size batches of new transcript lines as they appear.
2. Maintains a rolling compacted context in its conversation head —
   decisions made, files touched, reasoning chains, open threads,
   target progress, blockers.
3. On `/clear` detection (command message in JSONL), notes the boundary
   but keeps its accumulated understanding.
4. When `mnemo_restore` is called, ingests any final increments and
   emits the compacted context as structured output.
5. After idle timeout (configurable, e.g. 10 min), the summarizer
   instance is reaped. On next activity, a new one bootstraps from
   raw transcript (or a persisted checkpoint).

**Recursion guard:** Summarizer sessions are spawned with a known
marker (e.g., `--system-prompt` tag or registry metadata). mnemo
excludes these session IDs from summarizer spawning. The jevon
`disallow_tools` mechanism strips `Agent`, `TeamCreate`, etc. to
prevent summarizers from spawning further processes.

**Session continuity:** `/clear` does not change the session ID —
the JSONL file and UUID persist. A single summarizer instance tracks
the full session lifecycle across multiple /clear cycles.

**Compaction output structure** (v1):
```json
{
  "session_id": "...",
  "project": "...",
  "repo": "...",
  "targets_active": ["🎯T3", "🎯T10"],
  "targets_progressed": {"🎯T10": "target created, architecture designed"},
  "decisions": [
    {"what": "Use jevon Process API for summarizer lifecycle", "why": "..."}
  ],
  "files_touched": ["docs/targets.md", "internal/tools/tools.go"],
  "open_threads": ["Need to design checkpoint persistence format"],
  "next_steps": ["Implement mnemo_restore tool", "Add summarizer spawn logic"],
  "key_context": "Free-text summary of important reasoning and context"
}
```

**Acceptance criteria:**
- Summarizer spawns automatically for active sessions (not for its own sessions).
- Compacted context available within 2s of `mnemo_restore` call.
- Compaction survives `/clear` boundaries within a session.
- Idle reaping cleans up summarizer instances.
- `mnemo_restore` in a fresh post-clear segment returns useful context
  covering the pre-clear work.
- Token cost of summarizer < 10% of the session it tracks (Sonnet
  on fixed-size batches keeps this lean).

### 🎯T8 sqldeep integration

- **Value**: 6
- **Cost**: 5
- **Weight**: 1.2 (value 6 / cost 5)
- **Status**: achieved
- **Discovered**: 2026-04-06
- **Achieved**: 2026-04-07
- **Related**: 🎯T7 (templates benefit from expressive query syntax)

**Desired state:** `mnemo_query` accepts sqldeep syntax (JSON5-like
nested queries) in addition to plain SQL. Agents can write natural
nested JSON queries without hand-rolling `json_group_array`/`json_object`.

**Acceptance criteria:**
- `mnemo_query` transparently transpiles sqldeep syntax to SQL.
- Plain SQL continues to work unchanged.
- sqldeep JSON helper functions registered on the SQLite connection.
- Tool description documents the available syntax.
