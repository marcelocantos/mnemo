# Convergence Report

Standing invariants: all green. Tests pass, CI green (v0.7.0 release succeeded).

## Movement

- 🎯T9: (new — added since last report, with 6 sub-targets)
- 🎯T3: (unchanged) not started
- 🎯T8: (unchanged) not started
- 🎯T5: (unchanged) not started
- 🎯T7: (unchanged) not started
- 🎯T1: (unchanged) not started
- 🎯T2, 🎯T4, 🎯T6: (unchanged) achieved

## Gap report

### 🎯T3 Active work dashboard data  [weight 1.6]
Gap: **not started**
No `mnemo_recent_activity` tool exists. The data needed (session recency, message counts) is available in the database via `sessions` view, but no dedicated tool exposes the structured JSON output the acceptance criteria require.

### 🎯T8 sqldeep integration  [weight 1.2]
Gap: **not started**
No sqldeep references in the codebase. `mnemo_query` accepts only plain SQL. No transpilation layer.

### 🎯T9 Full-fidelity ingest and observability tools  [weight 1.1]
Gap: **not started** (converging 0/6 sub-targets achieved)

  [ ] 🎯T9.1 Full-fidelity ingest (weight 1.6) — not started: no usage/model/stop_reason fields in store schema
  [ ] 🎯T9.2 Token usage analytics (weight 2.7) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.3 Permission prompt analysis (weight 1.7) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.4 Process attribution (weight 2.5) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.5 System correlation (weight 1.0) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.6 Cross-session decision recall (weight 1.6) — not started, blocked by 🎯T9.1

### 🎯T5 Self-improving tool discovery  [weight 1.1]
Gap: **not started** (status only)
No pattern discovery tool or workaround detection logic.

### 🎯T7 Agent-defined query templates  [weight 0.9]
Gap: **not started** (status only)
No template system. Weight < 1.0 — cost exceeds value. Consider reframing or retiring.

### 🎯T1 Broader memory beyond transcripts  [weight 0.6]
Gap: **not started** (status only)
No work beyond the design philosophy note. Weight 0.6 — cost significantly exceeds value. Consider decomposition or deferral.

### 🎯T2 Smarter session classification  [weight 1.7]
Gap: **achieved**

### 🎯T4 Individual session transcript access  [weight 1.7]
Gap: **achieved**

### 🎯T6 Session self-identification  [weight 1.2]
Gap: **achieved**

## Recommendation

Work on: **🎯T3 Active work dashboard data**
Reason: Highest effective weight (1.6) among non-achieved, unblocked targets. Clear acceptance criteria, moderate cost, and high value for cross-tool integration. The underlying data already exists in the sessions view — this is primarily a tool definition and structured output task. While 🎯T9.2 has higher weight (2.7), it is blocked by 🎯T9.1 which requires schema redesign and full re-index.

## Suggested action

Add a `mnemo_recent_activity` tool to `internal/tools/tools.go` that queries the `sessions` view grouped by repo, returning per-repo JSON with last session time, message count, and key topics. Start by defining the tool schema and Backend interface method, then implement the store query. Accept a `days` parameter (default 7) for the recency window.

<!-- convergence-deps
evaluated: 2026-04-07T12:00:00Z
sha: ebbd6f4

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

🎯T9:
  gap: not started
  assessment: "New target. No sub-targets started. 0/6 achieved."
  read:
    - internal/store/store.go

🎯T9.1:
  gap: not started
  assessment: "No usage/model/stop_reason/agentId fields in store schema. No JSONB storage."
  read:
    - internal/store/store.go

🎯T9.2:
  gap: not started
  assessment: "No mnemo_usage tool. Blocked by T9.1."
  read: []

🎯T9.3:
  gap: not started
  assessment: "No mnemo_permissions tool. Blocked by T9.1."
  read: []

🎯T9.4:
  gap: not started
  assessment: "No mnemo_who_ran tool. Blocked by T9.1."
  read: []

🎯T9.5:
  gap: not started
  assessment: "No mnemo_whatsup tool. Blocked by T9.1."
  read: []

🎯T9.6:
  gap: not started
  assessment: "No mnemo_decisions tool. Blocked by T9.1."
  read: []

🎯T7:
  gap: not started
  assessment: "No template system. Weight < 1 — consider reframing."
  read: []

🎯T1:
  gap: not started
  assessment: "No work beyond design notes. Weight 0.6 — consider decomposition."
  read: []
-->
