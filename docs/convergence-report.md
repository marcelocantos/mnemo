# Convergence Report

Standing invariants: all green. Tests pass, CI green (v0.8.0 release succeeded).

## Movement

- 🎯T3: not started → **achieved** (mnemo_recent_activity tool implemented and released in v0.8.0)
- 🎯T8: not started → **achieved** (sqldeep integration merged and released in v0.8.0)
- 🎯T10: (new — live context compaction target added)
- 🎯T9.1: (unchanged) not started
- 🎯T5: (unchanged) not started
- 🎯T7: (unchanged) not started
- 🎯T1: (unchanged) not started
- 🎯T2, 🎯T4, 🎯T6: (unchanged) achieved

## Gap report

### 🎯T9.1 Full-fidelity ingest  [weight 1.6]
Gap: **not started**
No usage, model, stop_reason, agentId, or version fields in the store schema. Messages table stores only id, session_id, project, role, text, block_type, tool_name, tool_id, timestamp, seq. No JSONB storage. Full schema redesign and re-index required.

### 🎯T10 Live context compaction  [weight 1.25]
Gap: **not started**
No mnemo_restore tool, no summarizer spawning logic, no compaction infrastructure. References to "compaction" and "summarizer" exist only in docs/targets.md. Depends on jevon `claude.Process` / `manager.Manager` which is an external dependency.

### 🎯T5 Self-improving tool discovery  [weight 1.1]
Gap: **not started** (status only)
No pattern discovery tool or workaround detection logic in the codebase.

### 🎯T9 Full-fidelity ingest and observability tools  [weight 1.1]
Gap: **not started** (converging 0/6 sub-targets achieved)

  [ ] 🎯T9.1 Full-fidelity ingest (weight 1.6) — not started
  [ ] 🎯T9.2 Token usage analytics (weight 2.7) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.3 Permission prompt analysis (weight 1.7) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.4 Process attribution (weight 2.5) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.5 System correlation (weight 1.0) — not started, blocked by 🎯T9.1
  [ ] 🎯T9.6 Cross-session decision recall (weight 1.6) — not started, blocked by 🎯T9.1

### 🎯T7 Agent-defined query templates  [weight 0.9]
Gap: **not started** (status only)
No template system. Weight < 1.0 — cost exceeds value. Consider reframing or retiring.

### 🎯T1 Broader memory beyond transcripts  [weight 0.6]
Gap: **not started** (status only)
No work beyond design notes. Weight 0.6 — cost significantly exceeds value. 🎯T10 subsumes the core of this target; reassess after 🎯T10 is achieved.

### 🎯T2 Smarter session classification  [weight 1.7]
Gap: **achieved**

### 🎯T3 Active work dashboard data  [weight 1.6]
Gap: **achieved**

### 🎯T4 Individual session transcript access  [weight 1.7]
Gap: **achieved**

### 🎯T6 Session self-identification  [weight 1.2]
Gap: **achieved**

### 🎯T8 sqldeep integration  [weight 1.2]
Gap: **achieved**

## Recommendation

Work on: **🎯T9.1 Full-fidelity ingest**

Both the markdown ranking and bullseye agree: 🎯T9.1 is the highest-leverage next step. It has the highest effective weight among unblocked non-achieved targets (markdown: 1.6, bullseye: 2) and it is the critical-path gate for 5 downstream targets (🎯T9.2 through 🎯T9.6), several of which have very high weights (🎯T9.2 at 2.7, 🎯T9.4 at 2.5). Completing 🎯T9.1 unlocks the entire observability tools suite.

While 🎯T10 has the highest raw value (10), it depends on an external API (jevon `claude.Process`) and has no downstream dependents to unblock. 🎯T9.1 is purely internal work with a clear path and outsized multiplier effect.

## Suggested action

Design the new schema for full-fidelity ingest. Read `internal/store/store.go` to understand the current schema, then extend the `messages` table (or add a new `entries` table) to store the full JSONB entry alongside extracted virtual columns for `model`, `stop_reason`, `usage.input_tokens`, `usage.output_tokens`, `agentId`, and `version`. Bump the schema version constant to trigger a full re-index. Start with schema changes and the ingest path — tools come in subsequent sub-targets.

## Bullseye scorecard

**Ranking**:        +1
**Blocking**:       +1
**Data quality**:   -1
**Overall**:        0
**Markdown rec**:   🎯T9.1 Full-fidelity ingest
**Bullseye rec**:   🎯T9.1 Full-fidelity ingest
**Notes**: Ranking +1: bullseye's integer weights gave a clearer separation between T9.1 (weight 2) and T10 (weight 1) than markdown's 1.6 vs 1.25. Blocking +1: bullseye correctly identified all 5 T9.x sub-targets as blocked by T9.1, matching markdown's Gates analysis. Data quality -1: fresh bootstrap — all targets imported but the T10 dependency on external jevon API is not modeled as a depends_on edge in bullseye (markdown captures it as a note). Also, bullseye weights use integer rounding which loses some fidelity (T10 and T5 both show weight 1 despite different ratios). Overall 0: equivalent recommendation this run; bullseye's blocking analysis is slightly more structured, but data was just bootstrapped so hasn't been tested over time.

<!-- convergence-deps
evaluated: 2026-04-07T18:00:00Z
sha: 6a54f25

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
  gap: achieved
  assessment: "mnemo_recent_activity implemented and released in v0.8.0."
  read:
    - internal/tools/tools.go

🎯T6:
  gap: achieved
  assessment: "mnemo_self implements two-phase nonce protocol. Reliable."
  read:
    - internal/tools/tools.go

🎯T8:
  gap: achieved
  assessment: "sqldeep integration merged. mnemo_query transparently transpiles. Released in v0.8.0."
  read:
    - internal/tools/tools.go

🎯T9.1:
  gap: not started
  assessment: "No usage/model/stop_reason/agentId fields in store schema. No JSONB storage. Full schema redesign needed."
  read:
    - internal/store/store.go

🎯T10:
  gap: not started
  assessment: "No mnemo_restore tool, no summarizer logic, no compaction infrastructure. External dependency on jevon."
  read:
    - internal/tools/tools.go

🎯T5:
  gap: not started
  assessment: "No pattern discovery tool or workaround detection logic."
  read: []

🎯T9:
  gap: not started
  assessment: "0/6 sub-targets achieved."
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
  assessment: "No work beyond design notes. Weight 0.6 — T10 subsumes core."
  read: []

bullseye:
  ranking: 1
  blocking: 1
  data_quality: -1
  overall: 0
  markdown_rec: T9.1
  bullseye_rec: T9.1
-->
