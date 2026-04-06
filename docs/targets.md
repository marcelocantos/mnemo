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
