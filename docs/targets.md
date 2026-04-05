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
- **Status**: identified
- **Discovered**: 2026-04-06

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
- **Status**: identified
- **Discovered**: 2026-04-06

**Desired state:** mnemo can read, search within, and truncate
individual session transcripts. Absorbs jevon's `transcript_read`
and `transcript_rewind` functionality.

**Acceptance criteria:**
- `mnemo_read_session` tool returns messages from a specific session ID.
- Supports filtering by role, offset, limit.
- `mnemo_truncate_session` tool truncates a session's JSONL file
  (keeping the last N turns). Useful for context management.
- Works on raw JSONL files, not just the indexed database.
