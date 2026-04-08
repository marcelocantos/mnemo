# Convergence Report

Standing invariants: all green. Tests pass (3 packages), CI green (v0.10.0 release succeeded).

## Movement

- 🎯T9.1: not started → **achieved** (entries table with JSONB + 15 virtual columns, schema v5)
- 🎯T14: (new) → **achieved** (snapshot_files table + FTS + trigger, queryable via mnemo_query)
- 🎯T9: not started → **converging** (1/6 sub-targets achieved)
- 🎯T9.2–T9.6: now **unblocked** (gate 🎯T9.1 satisfied)
- 🎯T11, 🎯T12, 🎯T13: (new targets added — identified)
- 🎯T10, 🎯T5, 🎯T7, 🎯T1: (unchanged)

## Gap report

### 🎯T13 CI/CD history and statistics  [weight 2.7]
Gap: **not started**
No CI-related tables, no polling logic, no mnemo_ci tool. Requires new ingest infrastructure for GitHub Actions API.

### 🎯T9.2 Token usage analytics (`mnemo_usage`)  [weight 2.7]
Gap: **not started**
Token usage data is already ingested in the entries table (input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens virtual columns). No dedicated mnemo_usage tool yet — needs aggregation queries (daily totals, per-repo, per-model breakdown, cost estimates).

### 🎯T9.4 Process attribution (`mnemo_who_ran`)  [weight 2.5]
Gap: **not started**
Tool command data available via entries table (tool_name, tool_command columns). No dedicated mnemo_who_ran tool yet.

### 🎯T9.3 Permission prompt analysis (`mnemo_permissions`)  [weight 1.7]
Gap: **not started**
Tool use/result pairs in entries table. No analysis tool.

### 🎯T11 Git history indexing  [weight 1.6]
Gap: **not started** (status only)
No git-related tables or ingest logic. Requires new daemon polling infrastructure.

### 🎯T12 GitHub activity indexing  [weight 1.6]
Gap: **not started** (status only)
No GitHub-related tables or polling logic.

### 🎯T9.6 Cross-session decision recall (`mnemo_decisions`)  [weight 1.6]
Gap: **not started**
No decision detection heuristic or decisions table.

### 🎯T10 Live context compaction  [weight 1.25]
Gap: **not started** (status only)
No mnemo_restore tool, no summarizer logic. Blocked on external jevon dependency.

### 🎯T9 Full-fidelity ingest and observability tools  [weight 1.1]
Gap: **converging** (1/6 sub-targets achieved)

  [x] 🎯T9.1 Full-fidelity ingest — achieved
  [ ] 🎯T9.2 Token usage analytics — not started (unblocked)
  [ ] 🎯T9.3 Permission prompt analysis — not started (unblocked)
  [ ] 🎯T9.4 Process attribution — not started (unblocked)
  [ ] 🎯T9.5 System correlation — not started (unblocked)
  [ ] 🎯T9.6 Cross-session decision recall — not started (unblocked)

### 🎯T5 Self-improving tool discovery  [weight 1.1]
Gap: **not started** (status only)
No pattern discovery tool or workaround detection logic.

### 🎯T9.5 System correlation (`mnemo_whatsup`)  [weight 1.0]
Gap: **not started**
No system correlation logic.

### 🎯T7 Agent-defined query templates  [weight 0.9]
Gap: **not started** (status only)
Weight < 1.0 — cost exceeds value. Consider reframing or retiring.

### 🎯T1 Broader memory beyond transcripts  [weight 0.6]
Gap: **not started** (status only)
Weight 0.6 — cost significantly exceeds value. 🎯T10 subsumes the core use case.

## Recommendation

Work on: **🎯T9.2 Token usage analytics (`mnemo_usage`)**

Both the markdown ranking and bullseye agree: 🎯T13 and 🎯T9.2 are tied at the top by weight (2.7 in markdown, 3 in bullseye). Between equal weights, 🎯T9.2 has a smaller effective gap — the token usage data is already fully ingested in the entries table with dedicated virtual columns (input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens). The work is purely tool construction: aggregation queries and a new mnemo_usage tool definition. 🎯T13, by contrast, requires entirely new ingest infrastructure (GitHub Actions API polling, new tables, new daemon loop). 🎯T9.2 is cheaper to close and delivers immediate value — agents can understand their token spend.

## Suggested action

Add a `mnemo_usage` tool in `internal/tools/tools.go` with a corresponding `Usage` method on the store backend. The tool should accept filters (repo, date range, model) and return aggregated token counts with cost estimates. Start by writing the SQL aggregation query against the entries table: `SELECT date(timestamp), model, SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), SUM(cache_creation_tokens) FROM entries WHERE type='assistant' GROUP BY date(timestamp), model`. Add cost-per-token multipliers for known models.

## Bullseye scorecard

**Ranking**:        0
**Blocking**:       +1
**Data quality**:   0
**Overall**:        0
**Markdown rec**:   🎯T9.2 Token usage analytics
**Bullseye rec**:   🎯T9.2 Token usage analytics (tied with 🎯T13)
**Notes**: Ranking 0: both systems produce the same T13/T9.2 tie at the top with identical relative ordering below. Bullseye's integer rounding collapses some distinctions (T9.4 weight 2.5 in markdown rounds to 2 in bullseye, same as T9.3 at 1.7) but doesn't change the recommendation. Blocking +1: bullseye correctly shows all T9.x sub-targets as unblocked now that T9.1 is achieved, and correctly shows no blocked targets — matching markdown. The frontier output (12 targets ready) is a useful addition that markdown doesn't provide as a single list. Data quality 0: bullseye has all 7 achieved targets properly marked, active targets match markdown, no missing edges. The T10 external dependency on jevon is still not modeled as a depends_on edge in either system. Overall 0: equivalent recommendation; both agree on T9.2 as highest-leverage after applying the tie-breaking rule.

<!-- convergence-deps
evaluated: 2026-04-07T22:00:00Z
sha: 6256f86

🎯T9.1:
  gap: achieved
  assessment: "Entries table with JSONB + 15 virtual columns. Schema v5. Full re-index works."
  read:
    - internal/store/store.go

🎯T14:
  gap: achieved
  assessment: "snapshot_files table + FTS + trigger extracting from file-history-snapshot entries. Queryable via mnemo_query."
  read:
    - internal/store/store.go
    - internal/store/store_test.go

🎯T9.2:
  gap: not started
  assessment: "Token data ingested in entries table (virtual columns). No mnemo_usage tool yet."
  read:
    - internal/tools/tools.go
    - internal/store/store.go

🎯T9.4:
  gap: not started
  assessment: "Tool command data in entries table. No mnemo_who_ran tool."
  read:
    - internal/tools/tools.go

🎯T9.3:
  gap: not started
  assessment: "Tool use/result pairs in entries table. No mnemo_permissions tool."
  read:
    - internal/tools/tools.go

🎯T9.6:
  gap: not started
  assessment: "No decision detection heuristic or decisions table."
  read: []

🎯T9.5:
  gap: not started
  assessment: "No system correlation logic."
  read: []

🎯T13:
  gap: not started
  assessment: "No CI tables, polling, or mnemo_ci tool. New infrastructure required."
  read: []

🎯T11:
  gap: not started
  assessment: "No git history tables or ingest logic."
  read: []

🎯T12:
  gap: not started
  assessment: "No GitHub activity tables or polling."
  read: []

🎯T9:
  gap: converging
  assessment: "1/6 sub-targets achieved (T9.1). T9.2-T9.6 unblocked."
  read:
    - internal/store/store.go

🎯T10:
  gap: not started
  assessment: "No mnemo_restore tool, no summarizer logic. External jevon dependency."
  read: []

🎯T5:
  gap: not started
  assessment: "No pattern discovery tool or workaround detection logic."
  read: []

🎯T7:
  gap: not started
  assessment: "No template system. Weight < 1 — consider reframing."
  read: []

🎯T1:
  gap: not started
  assessment: "No work beyond design notes. Weight 0.6 — T10 subsumes core."
  read: []

🎯T2:
  gap: achieved
  assessment: "All acceptance criteria met."
  read: []

🎯T3:
  gap: achieved
  assessment: "mnemo_recent_activity implemented and released."
  read: []

🎯T4:
  gap: achieved
  assessment: "mnemo_read_session fully implemented."
  read: []

🎯T6:
  gap: achieved
  assessment: "mnemo_self nonce protocol reliable."
  read: []

🎯T8:
  gap: achieved
  assessment: "sqldeep integration merged and released."
  read: []

bullseye:
  ranking: 0
  blocking: 1
  data_quality: 0
  overall: 0
  markdown_rec: T9.2
  bullseye_rec: T9.2
-->
