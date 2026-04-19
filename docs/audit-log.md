# Audit Log

Chronological record of audits, releases, documentation passes, and other
maintenance activities. Append-only — newest entries at the bottom.

## 2026-04-06 — /release v0.1.0

- **Commit**: `e955013`
- **Outcome**: Released v0.1.0 (darwin-arm64, linux-amd64, linux-arm64). Added README, agents-guide, LICENSE, --version/--help-agent flags, release CI workflow, STABILITY.md. Homebrew formula pending HOMEBREW_TAP_TOKEN secret setup.

## 2026-04-06 — /release v0.2.0

- **Commit**: `e288d82`
- **Outcome**: Released v0.2.0 (darwin-arm64, linux-amd64, linux-arm64). Added Homebrew service definition (brew services start/stop), end-to-end setup instructions in README and agents-guide, verification step.

## 2026-04-06 — /release v0.3.0

- **Commit**: `d93265e`
- **Outcome**: Released v0.3.0 (darwin-arm64, linux-amd64, linux-arm64). Bimodal architecture (stdio MCP proxy + persistent daemon over UDS). Full content block indexing (tool_use, tool_result, thinking). Performance overhaul (WAL, materialised sessions, lock yielding). Search context, repo filter, mnemo_self, read-only query enforcement. 9 tests.

## 2026-04-06 — /release v0.4.0

- **Commit**: `1539d1e`
- **Outcome**: Released v0.4.0 (darwin-arm64, linux-amd64, linux-arm64). Binary hash handshake for version mismatch detection. Parallel ingest pipeline (42% faster). 15 virtual computed columns for all tool_input fields. 20 tests.

## 2026-04-06 — /release v0.5.0

- **Commit**: `7ae6d5b`
- **Outcome**: Released v0.5.0 (darwin-arm64, linux-amd64, linux-arm64). Fixed FTS5 optimize causing 100% CPU for 10+ minutes. Defer-safe writer cleanup. RPC performance logging with adaptive severity. Performance test assertions (200ms max per operation). Schema version rebuild approach.

## 2026-04-07 — /release v0.6.0

- **Commit**: `69ba833`
- **Outcome**: Released v0.6.0 (darwin-arm64, linux-amd64, linux-arm64). Fixed search query deadlocking daemon for 3+ minutes (two-phase FTS search, 5ms). Silenced deleted-file log spam in watcher.

## 2026-04-07 — /release v0.7.0

- **Commit**: `e8b5f65`
- **Outcome**: Released v0.7.0 (darwin-arm64, linux-amd64, linux-arm64). Added mnemo_repos tool for repo discovery. Dumb proxy architecture via mcpbridge — tool definitions and handling moved to daemon. Auto-reconnect on daemon restart. Protocol versioning replaces binary hash.

## 2026-04-07 — /release v0.9.0

- **Commit**: `a593481`
- **Outcome**: Released v0.9.0 (darwin-arm64, linux-amd64, linux-arm64). Full-fidelity ingest (🎯T9.1): new entries table stores every JSONL line as JSONB with 15 virtual columns. All entry types ingested (progress, system, file-history-snapshot). Messages linked via entry_id FK. Schema version 5 (triggers re-index). Unblocks 🎯T9.2–T9.6.

## 2026-04-07 — /release v0.10.0

- **Commit**: `736594c`
- **Outcome**: Released v0.10.0 (darwin-arm64, linux-amd64, linux-arm64). File-history-snapshot indexing (🎯T14): snapshot_files table with FTS5 auto-extracted via SQL trigger. Schema version 6. New targets: 🎯T11 git history, 🎯T12 GitHub activity, 🎯T13 CI/CD history.

## 2026-04-08 — /release v0.11.0

- **Commit**: `05fe3b4`
- **Outcome**: Released v0.11.0 (darwin-arm64, linux-amd64, linux-arm64). New tools: mnemo_memories (cross-project memory search), mnemo_usage (token analytics). Fuzzy OR-by-default search. Schema v7.

## 2026-04-09 — /release v0.12.0

- **Commit**: `77964e2`
- **Outcome**: Released v0.12.0 (darwin-arm64, linux-amd64, linux-arm64). Five new context source tools: mnemo_skills, mnemo_configs, mnemo_audit, mnemo_targets, mnemo_plans. Schema v8. Homebrew formula updated.

## 2026-04-10 — /release v0.13.0

- **Commit**: `f599813`
- **Outcome**: Released v0.13.0 (darwin-arm64, linux-amd64, linux-arm64). Three new observability tools: mnemo_who_ran (process attribution), mnemo_permissions (tool usage analysis), mnemo_ci (CI/CD run history with FTS). mnemo_usage gained hourly rate detection. Schema v9. Homebrew formula updated.

## 2026-04-11 — /release v0.14.0

- **Commit**: `ac93bc6`
- **Outcome**: Released v0.14.0 (darwin-arm64, linux-amd64, linux-arm64). Real-time file watching for all context sources (CLAUDE.md, audit logs, targets, plans). Homebrew formula updated.

## 2026-04-12 — /release v0.15.0

- **Commit**: `3112ab2`
- **Outcome**: Released v0.15.0 (darwin-arm64, linux-amd64, linux-arm64). Self-healing repo-level ingest (🎯T17): workspace-root filesystem walk discovers repos independently of session metadata. New ~/.mnemo/config.json with workspace_roots. Per-stream backfill status in mnemo_status/mnemo_stats. Schema v10. 15 new tests. Homebrew formula updated.

## 2026-04-13 — /release v0.16.0

- **Commit**: `b7a15b4`
- **Outcome**: Released v0.16.0 (darwin-arm64, linux-amd64, linux-arm64). Session chains (🎯T16): new session_chains table and mnemo_chain tool link /clear-bounded transcripts into work spans via time-gap heuristic. Session liveness (🎯T9.5.1): mnemo_sessions annotates live sessions with [LIVE pid=NNNNN] via lsof detection. Stats streams rendering in mnemo_stats text output. Schema v11. 15 new tests. Homebrew formula updated.

## 2026-04-15 — /release v0.17.0

- **Commit**: `66bf6cd`
- **Outcome**: Released v0.17.0 (darwin-arm64, linux-amd64, linux-arm64). Major expansion: decisions (🎯T9.6), mnemo_whatsup (🎯T9.5), full-fidelity observability parent (🎯T9 closed), git history (🎯T11), GitHub PRs/issues (🎯T12), self-improving pattern discovery (🎯T5), query templates (🎯T7), and the complete image stack — storage, Apple Vision OCR via CGO/ObjC with Tesseract fallback (🎯T19), batched `claude -p` AI descriptions (🎯T18), and CLIP/SigLIP embeddings with semantic+visual similarity search (🎯T20). Describer moved off ANTHROPIC_API_KEY to claude-p / OAuth. Image sidecars now process on arrival (no poll) with one shared NumCPU semaphore. Schema 11 → 18. Golden-image system tests added (vellum + pdftoppm pipeline, LFS-tracked). Homebrew formula updated.

## 2026-04-16 — /release v0.18.0

- **Commit**: `226b3b7`
- **Outcome**: Released v0.18.0. Live context compaction lands
  (🎯T10): per-connection background summariser + mnemo_restore +
  /c skill + token budget guard. MCP connection identity across
  /clear (🎯T25): definitive session-chain detection via peer-PID +
  connection_id, heuristic inference demoted to query-time only.
  New tool mnemo_docs (🎯T21) for markdown/txt/PDF across repos.
  mnemo_whatsup gains cwd/transcript enrichment and postmortem
  mode (🎯T24). Debounced file-watch handlers (🎯T23).
  extra_project_dirs config wired (partial 🎯T15). Schema 18 → 20,
  protocol 1 → 2. mcpbridge vendored into internal/bridge/.
  Ingest-time chain heuristic deleted.

## 2026-04-16 — /release v0.19.0

- **Commit**: `29e48d3`
- **Outcome**: Released v0.19.0. Per-file and progress logging during
  ingest: each changed file logs session ID, entry/message counts;
  periodic progress summary every 100 files with rate and ETA. Only
  files that grew since last ingest are logged. Homebrew formula updated.

## 2026-04-18 — /release v0.20.0

- **Commit**: `ff5aae6`
- **Outcome**: Released v0.20.0. Architectural collapse (🎯T27): mnemo
  is now a single HTTP MCP daemon. Stdio proxy and custom UDS
  JSON-RPC protocol removed; mark3labs/mcp-go StreamableHTTP handles
  clients directly (−2,231 lines net). connection_id sourced from
  Mcp-Session-Id header; compactor / mnemo_restore / chain detection
  continue to work. Stale stdio registrations get a migration hint on
  launch. Registration command changes to
  `claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp`.
  Homebrew formula updated.

## 2026-04-19 — /release v0.21.0

- **Commit**: `pending`
- **Outcome**: Released v0.21.0. Windows native support (🎯T22): mnemo
  daemon builds and runs on Windows amd64 and arm64 alongside the
  existing darwin-arm64, linux-amd64, linux-arm64 targets. Platform-
  specific code split into `internal/store/store_unix.go` /
  `store_windows.go`. No CLI or MCP surface change. Also identified
  four new data-mined introspection targets (🎯T28–🎯T31) and
  decomposed 🎯T15 (federated queries) into five leaf sub-targets
  (🎯T15.1–🎯T15.5). Homebrew formula updated.
