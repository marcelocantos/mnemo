# Convergence Report

Standing invariants: all green. Tests pass, CI green (v0.7.0 release succeeded).

## Gap report

### 🎯T2 Smarter session classification  [weight 1.7]
Gap: **achieved**
Sessions are tagged with repo, work type, and topic. `mnemo_sessions` supports filtering by repo and work type.

### 🎯T4 Individual session transcript access  [weight 1.7]
Gap: **achieved**
`mnemo_read_session` returns messages for a session ID with role/offset/limit filtering. No transcript mutation.

### 🎯T3 Active work dashboard data  [weight 1.6]
Gap: **not started**
No `mnemo_recent_activity` tool exists. No per-repo activity summary endpoint. The data needed (session recency, message counts) is available in the database via `sessions` view, but no dedicated tool exposes the structured JSON output the acceptance criteria require.

### 🎯T6 Session self-identification  [weight 1.2]
Gap: **achieved**
`mnemo_self` implements a two-phase nonce protocol. Reliable, not heuristic-based.

### 🎯T8 sqldeep integration  [weight 1.2]
Gap: **not started**
No sqldeep references in the codebase beyond targets.md and STABILITY.md. `mnemo_query` accepts only plain SQL. No transpilation layer.

### 🎯T5 Self-improving tool discovery  [weight 1.1]
Gap: **not started**
No `mnemo_discover_patterns` tool or pattern detection logic exists. The general query facility (`mnemo_query`) is present, but no automated analysis of agent workaround patterns.

### 🎯T7 Agent-defined query templates  [weight 0.9]
Gap: **not started** (status only)
No template storage, `mnemo_define`, `mnemo_evaluate`, or `mnemo_list_templates` tools. Weight < 1.0 -- cost exceeds value. Consider reframing or retiring.

### 🎯T1 Broader memory beyond transcripts  [weight 0.6]
Gap: **not started** (status only)
No work beyond the design philosophy note. Weight 0.6 -- cost significantly exceeds value. Consider reframing into smaller sub-targets or deferring until the codebase matures.

## Recommendation

Work on: **🎯T3 Active work dashboard data**
Reason: Highest effective weight (1.6) among non-achieved targets. Clear acceptance criteria, moderate cost, and high value for cross-tool integration (jevon dashboard). The underlying data already exists in the sessions view -- this is primarily a tool definition and structured output task.

## Suggested action

Add a `mnemo_recent_activity` tool to `internal/tools/tools.go` that queries the `sessions` view grouped by repo, returning per-repo JSON with last session time, message count, and key topics. Start by defining the tool schema and Backend interface method, then implement the store query. Accept a `days` parameter (default 7) for the recency window.

<!-- convergence-deps
evaluated: 2026-04-06T16:39:12Z
sha: 4001ab9

🎯T2:
  gap: achieved
  assessment: "All acceptance criteria met. Sessions tagged with repo, work type, topic. Filtering works."
  read:
    - internal/tools/tools.go

🎯T4:
  gap: achieved
  assessment: "mnemo_read_session fully implemented with role/offset/limit. No transcript mutation."
  read:
    - internal/tools/tools.go

🎯T3:
  gap: not started
  assessment: "No mnemo_recent_activity tool. Data exists in sessions view but no structured API."
  read:
    - internal/tools/tools.go

🎯T6:
  gap: achieved
  assessment: "mnemo_self implements two-phase nonce protocol. Reliable."
  read:
    - internal/tools/tools.go

🎯T8:
  gap: not started
  assessment: "No sqldeep references in codebase. mnemo_query accepts plain SQL only."
  read:
    - internal/tools/tools.go

🎯T5:
  gap: not started
  assessment: "No pattern discovery tool or workaround detection logic."
  read:
    - internal/tools/tools.go

🎯T7:
  gap: not started
  assessment: "No template system. Weight < 1 — consider reframing."
  read: []

🎯T1:
  gap: not started
  assessment: "No work beyond design notes. Weight 0.6 — consider decomposition."
  read: []
-->
