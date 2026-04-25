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

- **Commit**: `8a777d1`
- **Outcome**: Released v0.21.0. Windows native support (🎯T22): mnemo
  daemon builds and runs on Windows amd64 and arm64 alongside the
  existing darwin-arm64, linux-amd64, linux-arm64 targets. Platform-
  specific code split into `internal/store/store_unix.go` /
  `store_windows.go`. No CLI or MCP surface change. Also identified
  four new data-mined introspection targets (🎯T28–🎯T31) and
  decomposed 🎯T15 (federated queries) into five leaf sub-targets
  (🎯T15.1–🎯T15.5). Homebrew formula updated.

## 2026-04-20 — /release v0.22.0

- **Commit**: `0663dc6`
- **Outcome**: Released v0.22.0. Windows double-click installer
  (🎯T32 groundwork): Inno Setup `.exe` produced in CI on every
  release, bundling mnemo.exe plus a native Windows Service mode
  (auto-start, restart-on-failure, Event Log source) via
  `golang.org/x/sys/windows/svc`. Four new cross-platform subcommands
  (`register-mcp`, `unregister-mcp`, `install-service`,
  `uninstall-service`) let the installer patch `%USERPROFILE%\.claude.json`
  and register the service without the user opening a terminal.
  `runServe` now takes a context and shuts down gracefully via HTTP
  Shutdown on SCM Stop. New `internal/mcpconfig/` package with full
  unit coverage handles atomic config patching (preserves other keys
  and MCP entries). Decomposed 🎯T32 into T32.1 (service), T32.2
  (registration subcommand), T32.3 (installer + CI). Code signing
  deliberately deferred to a follow-up target — SmartScreen will
  warn on first run until an EV cert is added. Homebrew formula
  updated.

## 2026-04-22 — /release v0.23.0

- **Commit**: `ce98e11`
- **Outcome**: Released v0.23.0. Two-part Windows fix rolled into
  one release. **(1) Critical indexing fix**: v0.22.0 ran mnemo as
  a Windows Service (LocalSystem), so `os.UserHomeDir()` pointed at
  the LocalSystem profile and the indexer found zero transcripts —
  mnemo effectively did nothing on Windows. v0.23.0 switches to a
  per-user Scheduled Task triggered AtLogon, which runs in the
  user's session with the correct `USERPROFILE`. The installer now
  invokes `mnemo install-agent` / `uninstall-agent` (new
  subcommands replacing the SCM-based `install-service` /
  `uninstall-service`). `install-agent` also tears down any
  v0.22.0-era Service of the same name for clean upgrades in
  place. `service_windows.go` / `service_other.go` removed;
  replaced by `agent_windows.go` / `agent_other.go` (no SCM
  dependency — shells out to `schtasks.exe` and `sc.exe`).
  **(2) Windows ARM64 parity**: release.yml gained a
  `windows-11-arm` matrix leg that produces a native arm64
  mnemo.exe zip plus a matching
  `mnemo-<version>-windows-arm64-setup.exe` Inno Setup installer.
  The shared `.iss` now takes a `/DArch=...` preprocessor flag that
  drives `ArchitecturesInstallIn64BitMode`, `ArchitecturesAllowed`,
  and `OutputBaseFilename`. The installer build step auto-installs
  Inno Setup via Chocolatey when iscc.exe is missing, so it works
  uniformly on both windows-latest (amd64) and windows-11-arm
  (arm64) runners. Validated via a v0.23.0-rc.1 prerelease before
  cutting the real tag, per the new release-workflow-touch signal
  in the /release skill. Homebrew formula updated.

## 2026-04-25 — /release v0.28.0

- **Commit**: `7e280ed`
- **Outcome**: Released v0.28.0. New `mnemo diagnose` subcommand —
  manual single-screen health report covering nine dimensions:
  (1) daemon process (PID, listening port, inherited PATH from
  ps eww / /proc/PID/environ); (2) HTTP MCP endpoint with an
  initialize handshake + RTT; (3) external tools (gh, git, claude,
  uv, pdftotext, mutool, lsof, brew) checked against BOTH the
  calling shell's PATH and the daemon's inherited PATH — the
  [d only] flag surfaces the most common silent-failure mode where
  the user's shell can see a tool but the launchd-spawned daemon
  cannot; (4) filesystem (~/.claude/projects/ readability + JSONL
  count, ~/.mnemo/ writability, db file size + mtime); (5) database
  opened read-only via SQLite ?mode=ro so it runs alongside the
  live daemon, reporting schema version, 17 table row counts, and
  per-stream ingest_status recency; (6) index freshness (newest
  JSONL mtime vs newest indexed message timestamp, with drift
  classification: <5min healthy / <1h lagging / >=1h not keeping up);
  (7) configuration snapshot showing workspace_roots /
  extra_project_dirs / synthesis_roots with each path's existence;
  (8) Claude Code integration — reads ~/.claude.json, recognises
  the mcpbridge wrapper pattern (extracts the wrapped --url for
  validation), validates ?user=<name> presence; (9) recent
  ERROR/WARN lines from the mnemo log (auto-located by platform).
  Exit code 1 on any FAIL for scripted health probes. Self-test
  on author's machine immediately surfaced that the brew-services
  daemon's launchd-inherited PATH is the spartan default
  /usr/bin:/bin:/usr/sbin:/sbin, hiding gh/claude/uv/pdftotext/brew
  — explaining why ci_runs/github_prs/github_issues are all 0.
  ~530 lines in a new diagnose.go; no impact on existing code paths.
  Homebrew formula updated.

## 2026-04-25 — /release v0.27.0

- **Commit**: `d0eb33e`
- **Outcome**: Released v0.27.0. Trim `mnemo_status` defaults so
  routine calls stay inline in Claude Code (under 25KB tool-result
  threshold). User reported a 74KB response from `mnemo_status`
  with default args on a moderately busy machine, which Claude
  Code persisted to disk and showed only a 2KB preview. Cause:
  defaults were `max_sessions=3`, `max_excerpts=20`,
  `truncate_len=200` and truncation applied only to assistant
  messages — user pastes (logs, command output, code) escaped the
  cap entirely. Defaults are now `max_sessions=2`,
  `max_excerpts=6`, `truncate_len=160`, and truncation applies to
  every excerpt regardless of role. Estimated response-size
  reduction: ~10× on the reported workload (74KB → ~6KB). All
  three knobs remain caller-overridable for users who actually
  want the verbose form. Pure UX/default change in
  `internal/store/store.go` and `internal/tools/tools.go`. No
  schema or protocol impact. Homebrew formula updated.

## 2026-04-25 — /release v0.26.0

- **Commit**: `aabd560`
- **Outcome**: Released v0.26.0. Auto-migrate stdio holdovers:
  when mnemo is launched with stdin piped (legacy
  `claude mcp add --transport stdio mnemo` registrations from
  pre-v0.20.0 installs), the binary now rewrites the user's
  `~/.claude.json` mnemo entry to the HTTP+`?user=<name>` shape
  via the existing `mcpconfig.URLForUser` helper, best-effort
  starts the daemon via `brew services start mnemo` if the port
  is free, and exits with a friendly "restart this session" hint.
  The previous behaviour was to print a 3-step manual fix and
  exit (`claude mcp remove` → `claude mcp add` → restart). Falls
  back to the manual hint on any failure path so users always
  have a recovery route. Replaces the `stdioMigrationMessage`
  constant with `stdioMigrationManualHint`. Pure UX change — no
  protocol or schema impact. Homebrew formula updated.

## 2026-04-25 — /release v0.25.0

- **Commit**: `b135e46`
- **Outcome**: Released v0.25.0. Major architectural shift: the
  daemon now routes every MCP request to a per-user Store via a
  new `internal/registry/` package, keyed by a `?user=<name>` query
  parameter on the MCP URL. First request for each user lazily
  creates that user's Store and per-user background workers
  (ingest, watcher, compactor, CI poll); `Registry.Close` drains
  them on shutdown. Tools package refactored: `Handler` holds a
  resolver injected by main; per-call `callHandler` (built inside
  `Handler.Call`) owns the resolved `store.Backend` for one
  invocation — 31 method receivers moved from `*Handler` to
  `*callHandler`, `mnemo_self` now reads its session ID from
  `h.cc`. HTTPContextFunc wired on the StreamableHTTPServer pulls
  `?user=<name>` off the URL into a ctx value consumed by
  RegisterTools. New `internal/store/homedir.go` resolves
  username → home directory (via `os/user.Lookup` on all platforms);
  `DefaultUsername()` returns `ErrNoDefaultUser` on Windows
  Services so ambiguous requests fail loudly rather than silently
  indexing LocalSystem's profile. Replaces the v0.23.0 Scheduled
  Task (died overnight on battery/sleep) and v0.24.0
  `-H=windowsgui` workaround. `service_windows.go` restored with
  install-service/uninstall-service (plus legacy Scheduled Task
  cleanup for in-place upgrades); `agent_*.go` removed. The
  installer's `register-mcp` step now writes an MCP URL with
  `?user=<current-user>` so the daemon can route correctly from
  the first call. Will be validated via a v0.25.0-rc.1 prerelease
  per the release-workflow-touch signal. Homebrew formula updated.

## 2026-04-22 — /release v0.24.0

- **Commit**: `46126f6`
- **Outcome**: Released v0.24.0. Fixes the visible console window
  the Windows Scheduled Task was popping at logon. v0.23.0 shipped
  mnemo.exe with the default console subsystem; when `schtasks`
  launches it, Windows creates a console and shows it. Switched
  the Windows build to `-H=windowsgui` so Windows never attaches a
  console. Added `console_windows.go` with an init shim that calls
  `AttachConsole(ATTACH_PARENT_PROCESS)` and reopens CONOUT$/CONIN$
  so `mnemo --version` and other CLI invocations from PowerShell /
  cmd still show output. Headless launches (Scheduled Task,
  Explorer double-click) stay silent as intended. No code change
  beyond the subsystem flag + the init shim. Homebrew formula
  updated.

## 2026-04-25 — /release v0.29.0

- **Commit**: `b23dcc9`
- **Outcome**: Released v0.29.0. Federation across linked mnemo
  instances (🎯T15, all five sub-targets shipped). Adds
  `internal/endpoint/` (mTLS material under
  `~/.mnemo/endpoint/{cert.pem,key.pem}`, key 0600, ECDSA P-256,
  10-year validity, regen on corruption/expiry; trusted peer certs
  under `~/.mnemo/peers/<name>.pem` with malformed-skip), new
  `linked_instances` config field with strict validation
  (https-only URLs, unique names, resolvable peer cert by name OR
  inline PEM), second mTLS MCP listener on `:19420`
  (`--federated-addr` flag, default `:19420`, empty disables) via
  `internal/federation/server.go`, federation client in
  `internal/federation/client.go` with per-peer pinned mTLS,
  persistent http.Client + HTTP/2 + 90s keep-alive per peer, 5s
  default timeout, and seven typed error sentinels
  (`ErrUnknownInstance`, `ErrConnectionRefused`, `ErrConnectFailed`,
  `ErrTLSHandshake`, `ErrTimeout`, `ErrServerError`,
  `ErrMalformedResponse`), and fan-out + merge in
  `internal/federation/fanout.go` (`Fanout(ctx, toolName, args)`
  parallel-calls every peer; `MergePeerResults` wraps local +
  peers + warnings into a `FanoutEnvelope`). 16 read-shaped tools
  participate in fan-out; write- and control-shaped tools bypass
  federation by enumeration. Backwards-compatible: when
  `linked_instances` is empty the wrapper returns the local text
  verbatim. New CLI subcommands: `print-endpoint`,
  `print-federated-addr`, `ping-peer <name>`. 16 federation tests
  cover happy path, cert mismatch, connection refused, timeout,
  server error, malformed response, fan-out merge attribution,
  graceful degradation under timeout, error-kind classification,
  backwards-compat pass-through, and the federated-server tool-set
  boundary. Skill correction during this release: the release
  skill's Ahead-N rule was internally inconsistent with squash-only
  repos; updated `discover.sh` to emit `merge_strategy` and
  branched the SKILL.md handling accordingly. Homebrew formula
  updated.
