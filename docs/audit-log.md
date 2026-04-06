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
