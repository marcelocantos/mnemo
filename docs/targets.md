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

**Open questions:**
- What's the data model beyond raw transcript messages?
- Should it extract and index decisions, action items, code changes?
- How does it relate to Claude's auto-memory (MEMORY.md files)?
- Is there a summarisation layer that distills sessions into key facts?

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
- **Status**: identified
- **Discovered**: 2026-04-06
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

Ingest all top-level JSONL fields and message sub-fields. Key
additions: `usage` (token counts per response), `model`, `stop_reason`,
`version`, `slug`, `agentId`, `data.*` (progress events),
`toolUseResult.*` (structured tool results). Store the full entry as
JSONB where practical; add virtual columns for high-query fields.
Requires schema version bump (full re-index).

#### 🎯T9.2 Token usage analytics (`mnemo_usage`)

Report token consumption by day, repo, session, model — with cost
estimates at current pricing. Data comes from `message.usage` fields
(input_tokens, output_tokens, cache_read, cache_creation). Should
support: daily totals, per-repo breakdown, per-model breakdown,
hourly rate detection ("am I spending too fast?").

#### 🎯T9.3 Permission prompt analysis (`mnemo_permissions`)

Identify most-used tools and frequent approval patterns from
tool_use/tool_result message pairs. Suggest `allowedTools` rules
for settings.json. "You approved Bash 90k times — consider adding
`Bash(~/work/**)` to your allowed list."

#### 🎯T9.4 Process attribution (`mnemo_who_ran`)

Given a command pattern (e.g., "make", "clang", "python"), find which
session(s) ran it recently. Answers "which session is hogging CPU?"
by matching against `tool_command` in recent Bash tool_use entries.

#### 🎯T9.5 System correlation (`mnemo_whatsup`)

Correlate current system state (high CPU, fan spinning, disk I/O)
with active mnemo sessions. Runs `top`/`ps` to find heavy processes,
cross-references PIDs and command patterns against recent session
activity. Answers "what's eating CPU?" with "session X in repo Y
has been running a make build for 3 minutes."

#### 🎯T9.6 Cross-session decision recall (`mnemo_decisions`)

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

### 🎯T8 sqldeep integration

- **Value**: 6
- **Cost**: 5
- **Weight**: 1.2 (value 6 / cost 5)
- **Status**: identified
- **Discovered**: 2026-04-06
- **Related**: 🎯T7 (templates benefit from expressive query syntax)

**Desired state:** `mnemo_query` accepts sqldeep syntax (JSON5-like
nested queries) in addition to plain SQL. Agents can write natural
nested JSON queries without hand-rolling `json_group_array`/`json_object`.

**Acceptance criteria:**
- `mnemo_query` transparently transpiles sqldeep syntax to SQL.
- Plain SQL continues to work unchanged.
- sqldeep JSON helper functions registered on the SQLite connection.
- Tool description documents the available syntax.
