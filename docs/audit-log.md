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
