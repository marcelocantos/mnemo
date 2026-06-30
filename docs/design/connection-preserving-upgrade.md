# Connection-Preserving Self-Upgrade

*Status: design note — 2026-06-30. Anchors 🎯T97. Revisits the literal
"single daemon / no proxy" framing of 🎯T27 while preserving its real
invariants (see "Relationship to T27").*

**Tracking.** Design source for 🎯T97.

> ⚠️ **Superseded in large part by a 2026-06-30 spike — read "Spike
> outcome" first.** The edge proxy and background-work lease below are
> dropped; the sections are retained as original rationale + fallback.

---

## Spike outcome (2026-06-30) — the proxy and lease are dropped

A mark3labs v0.47 spike collapsed most of this design. The #27142 break
is a **configuration choice**: mnemo's `WithStateful(true)` selects
`InsecureStatefulSessionIdManager`, which validates session-id
*existence* against an in-memory map that's empty after a restart → 404.
The library default `StatelessGeneratingSessionIdManager` validates
**format only** (*"allows cross-instance operation"*). Proven
empirically — stateful → **404**, stateless-generating → **200 + `pong`**
— a fresh process accepts a prior session id, so the MCP session survives
an in-place restart. POST sessions are also ephemeral (no re-`initialize`
needed) and `SendNotificationToAllClients(tools/list_changed)` is built
in and automatic.

**Therefore the edge proxy ("The shape") and the background-work lease
("The background-work lease") below are DROPPED.** The live design is:

- a **cross-instance session-id manager** (ideally SQLite-backed, so
  explicit `Terminate` survives a restart and an *adopted* id flags a
  pre-upgrade session — the T97.6 banner hook),
- **graceful drain + `wal_checkpoint(TRUNCATE)`** (unchanged),
- **fast in-place restart** (no proxy, no overlap, no lease),
- **detect + notify** via the T83 pipeline, and **opt-in apply**.

**One open unknown:** whether Claude Code's client cleanly rides out the
brief restart *gap* (connection-refused → backoff retry → resume on the
cached id). Verifiable only with a **live** session once the id-manager
change lands; with the accepting manager there's no 404, so #27142's
trigger never fires. **Zero-gap fallback** if the live test disappoints:
`SO_REUSEPORT` dual-bind + the bg-lease — still **not** an L7 proxy,
since both processes honor any format-valid id.

Read everything below through this lens.

---

## TL;DR

mnemo should upgrade its own binary, and live Claude Code agents should
keep working across the upgrade without losing their MCP session.

The hard part is **not** swapping bytes — it's that Claude Code caches
the `Mcp-Session-Id` and does **not** re-`initialize` when the server
restarts ([anthropics/claude-code#27142]; the HTTP transport also
degrades after ~89 min, [#60949]). A naïve in-place restart therefore
leaves connected sessions hung on a stale session id until the user
runs `/mcp`. We cannot fix the client, so the design routes around it.

Three facts shape everything:

1. **An MCP session is not a TCP connection.** Streamable-HTTP runs
   many POSTs plus a long-lived GET/SSE stream, all sharing one
   `Mcp-Session-Id`, over whatever TCP connections the client's pool
   chooses. So a pure L4 router cannot keep a session affixed to one
   backend — a new POST on an existing session can land on a fresh
   connection and get routed to the wrong process. **Session affinity
   is an L7 (header) concern.**
2. **mnemo is already crash-only.** WAL + `synchronous = NORMAL`
   (`store.go:522`), resumable + idempotent ingest (`ingest_state`
   offset at `store.go:2991`; `INSERT OR IGNORE` on `(session_id,
   uuid)` at `store.go:2835`), convergence-driven derived streams
   (🎯T68), and a stale-connection sweeper (🎯T60) mean a hard kill
   costs nothing durable. **Graceful drain is about letting in-flight
   calls finish, not about protecting state.**
3. **The background work is a singleton.** Ingest, compaction,
   mirrors, and the image pools must run in exactly one process. Any
   design that overlaps two mnemo processes (which connection-preserving
   upgrade requires) must lease that role to exactly one of them.

The architecture that falls out: a **thin, stable edge process** owns
the listen socket and client connections and routes by `Mcp-Session-Id`
to a **swappable backend**; the singleton background role is guarded by
a **single-holder lease**. Backend upgrades (≈99% of mnemo's churn) are
connection-preserving. Edge upgrades (rare) are the one case that still
drops connections, and we make that explicit.

---

## The shape

```
                 ┌─────────────────────────────────────────────┐
   Claude Code   │  EDGE  (thin, stable, rarely upgraded)        │
   sessions ─────┤  • owns :19419 listener + all client conns    │
   (HTTP MCP)    │  • routes by Mcp-Session-Id  → backend         │
                 │  • transparent: forwards standard MCP/HTTP     │
                 │  • supervises backends; drives the swap        │
                 └───────┬───────────────────────┬───────────────┘
                         │ plain MCP-over-HTTP    │ (loopback / UDS)
                 ┌───────▼──────────┐    ┌────────▼───────────┐
                 │ BACKEND vN  (old)│    │ BACKEND vN+1 (new) │
                 │ • mark3labs MCP  │    │ • mark3labs MCP    │
                 │ • Store / tools  │    │ • Store / tools    │
                 │ • holds bg-lease?│ ⇄  │ • holds bg-lease?  │  ← exactly one
                 └────────┬─────────┘    └────────┬───────────┘
                          └──────────┬────────────┘
                                     ▼
                              ~/.mnemo/mnemo.db   (WAL, multi-process safe)
```

- **Edge ↔ backend is plain MCP-over-HTTP** on loopback or a Unix
  socket. **No custom protocol** — the edge reads one header
  (`Mcp-Session-Id`) to pick a backend and forwards the request
  verbatim. It never terminates MCP, never rehydrates session state,
  never parses tool calls.
- **Session affinity, with backend draining.** Each session is pinned
  to the backend that handled its `initialize`. On upgrade the edge
  spawns the new backend, sends **new** `initialize`s to it, keeps
  routing **existing** sessions to the old backend, and reaps the old
  backend once its last session ends. **A session never moves between
  binaries** → nothing is yanked, no in-flight call is interrupted,
  #27142 never fires.
- **Backends share the DB** via WAL (multi-process safe; one writer at
  a time, kernel-serialized). The default `wal_autocheckpoint` keeps
  the WAL bounded during overlap.

Because the edge is the only thing that owns client connections, and it
keeps running the *old* edge binary across a backend swap, the client's
TCP connection and session id stay valid throughout. The client never
observes a session invalidation, so the cached-`Mcp-Session-Id` bug has
nothing to trigger on.

---

## The background-work lease

Overlapping two backends means two processes could each try to ingest,
compact, and reconcile. That must not happen. A **single-holder lease**
(a row in SQLite with an owner pid + heartbeat, or an OS file lock)
gates the singleton role:

- At most one backend holds the lease and runs ingest / compaction /
  mirrors / image pools.
- On upgrade, the **old backend releases the lease immediately** when
  told to drain; the **new backend acquires it**. Sub-second handoff;
  these workers are not latency-sensitive, so a brief pause is fine.
- The draining old backend keeps **serving its pinned sessions** (read
  + tool calls against the shared DB) but does **no** background work.
- Bonus: the lease also prevents an accidental double-daemon in normal
  operation, independent of upgrades.

This is why the role split is real and not cosmetic: serving wants to
be momentarily multi-process (for drain), background work must stay
singleton. The lease is the seam between the two.

---

## Upgrade orchestration

1. **Detect.** A low-frequency checker shells out to `gh release list
   --repo marcelocantos/mnemo` (same pattern as the PR/CI backfill,
   `store.go`) and compares the latest tag to the `version` constant.
   Cache the result; surface it through the existing 🎯T83 health →
   notifier → dashboard pipeline (a `diag.Check` named
   `upgrade.available`). On by default, opt out via
   `disable_upgrade_check`.
2. **Acquire the binary.** Spawn a **detached** helper
   (`brew upgrade mnemo`) that outlives the daemon — an in-process byte
   swap would fight Homebrew's file/symlink ownership. Only the backend
   role is swapped; the edge keeps running.
3. **Wait for a safe window.** Default trigger: **30 minutes of MCP
   traffic quiescence**. (With session-affinity draining this is about
   *when to start*, not about connection survival — existing sessions
   finish on their original backend regardless.)
4. **Swap.** Edge spawns the new backend from the current on-disk
   binary (the brew symlink, now repointed), routes new `initialize`s
   to it, signals the old backend to drain.
5. **Drain + checkpoint + reap (old backend exit).**
   - Stop accepting new requests for its sessions; let in-flight finish.
   - Release the background lease (new backend acquires it).
   - Stop background workers; quiesce the read pool.
   - `PRAGMA wal_checkpoint(TRUNCATE)` on the writer; verify the
     returned `(busy, log, ckpt)` shows `busy = 0`, `log == ckpt`.
     This is **hygiene, not safety** (the DB is crash-safe and the
     `VACUUM INTO` backup at `internal/backup/backup.go:124` is already
     WAL-aware) — it just leaves an empty WAL for the next open. A
     full TRUNCATE only completes once the old backend is the *last*
     connection, which is exactly its exit moment.
   - Exit. **Hard-kill fallback on a deadline** (≈5–10 s): if drain
     hangs, SIGKILL — safe precisely because of the crash-only property.

`opt-in` gate: detection + notification ship on by default; *applying*
the upgrade (steps 2–5) is opt-in via `auto_upgrade.enabled`, because
restarting a backend many sessions depend on is too big a default to
flip silently. When enabled, it uses the quiescence policy above.

---

## Telling the agents

Reliable model-visible signalling is constrained: `notifications/message`
is received but **not displayed** by Claude Code; tool-result text is
the only channel the model reliably reads. So:

- **One-time banner.** When a session that initialized under vN sends
  its first request after being routed to vN+1 (the affinity model
  makes this rare — only sessions that outlive their original backend's
  reap, e.g. via a forced edge-level migration), the edge/backend
  prepends a concise `mnemo upgraded vN → vN+1` notice (with a pointer
  to `mnemo_doctor` / changelog) to that tool result, once.
- **`tools/list_changed`.** Best-effort push so Claude Code re-lists
  tools when a version adds/changes them. Verify mark3labs v0.47
  exposes a server→client push over streamable-HTTP; if not, the
  client re-lists on its next reconnect anyway.
- **`serverInfo.version`** already reflects the running backend; visible
  via `/mcp` (informational; Claude Code does not act on version diffs).

Under the affinity-drain model the banner is mostly an edge-case
courtesy — most sessions live entirely within one backend version and
simply see the new tool set when they next connect fresh. We keep the
mechanism for the outlier and for any future forced-migration path.

---

## Degradation & the one honest gap

- **Non-Homebrew installs** (raw `go install`, manual binary) and
  **Windows** (installer/service): detect "not brew-managed" by
  resolving `os.Executable()` against the Homebrew prefix; degrade to
  **notify-only**. Self-replacing a running Windows service exe needs
  its own helper + service stop — deferred.
- **Edge upgrades drop connections.** When the *edge* binary itself
  changes (rare — only when the proxy/supervisor code changes), there
  is no way to hand off owned connections in place without an even
  heavier dance. We keep the edge deliberately tiny and near-frozen so
  this is infrequent, and document it as the single connection-dropping
  case. Everything in the churny backend upgrades transparently.

---

## Relationship to 🎯T27

T27 ("single HTTP MCP daemon with **no proxy and no custom protocol**")
was about deleting the bespoke **UDS protocol** in `internal/rpc/` that
coupled a stdio proxy to a daemon and carried `cwd` / `session_id` /
`connection_id` *implicitly*. This design **preserves every real
invariant T27 established**:

- **No custom protocol.** Edge ↔ backend is standard MCP-over-HTTP.
- **Identity via the `Mcp-Session-Id` header** — the edge routes on
  exactly the identifier T27 standardized; `daemon_connections` /
  `connection_sessions` keep their meaning.
- **Standard registration** (`claude mcp add --transport http …`) is
  unchanged; clients still speak plain HTTP MCP to one endpoint.

What it revisits is only T27's *literal* "single process / no proxy":
there is now a transparent internal edge proxy in front of a swappable
backend. That is a different thing from the bespoke proxy T27 removed.
Rather than rewrite T27's achieved record (which would falsify its
2026-04-18 achieved date), the reconciliation is documented here and in
🎯T97's context; T27 stays Achieved and unmodified.

---

## Decomposition (🎯T97)

- **T97.1** — Foreground daemon (the backend role) drains gracefully on
  SIGTERM/SIGINT: stop intake → release bg-lease → stop workers →
  quiesce reads → `wal_checkpoint(TRUNCATE)` → exit, with a hard-kill
  deadline fallback. *(Foundation; independently valuable — today the
  foreground path has no signal handler at all, `main.go:189`.)*
- **T97.2** — A newer release is detected via `gh` and surfaced through
  the T83 health/notification/dashboard pipeline; opt out via
  `disable_upgrade_check`. *(Independent; ships first.)*
- **T97.3** — A thin edge process owns the listener + client connections
  and routes by `Mcp-Session-Id` to a backend over plain MCP/HTTP, with
  session-affinity draining. *(Depends on T97.1.)*
- **T97.4** — Singleton background work is guarded by a single-holder
  lease with clean handoff between backend instances.
- **T97.5** — Opt-in auto-apply: at 30-min quiescence, detached
  `brew upgrade`, spawn new backend, route new sessions, drain + reap
  old, all client connections preserved. *(Integration node — depends
  on T97.1–T97.4.)*
- **T97.6** — Sessions spanning a swap learn of it (one-time banner +
  best-effort `tools/list_changed`). *(Depends on T97.3.)*

## Open risks / spike

- **mark3labs v0.47 behavior** on an unknown `Mcp-Session-Id` and
  whether it can push `tools/list_changed` over streamable-HTTP — a
  short spike before T97.3/T97.6 lock.
- **Edge transport reuse.** Ideally the edge reuses mark3labs's
  streamable-HTTP transport for the client side and a plain HTTP client
  toward the backend, so we don't reimplement MCP framing. Confirm the
  GET/SSE stream proxies cleanly (it's a long-lived response body to
  splice).
- **Lease storage.** SQLite row vs OS file lock — the file lock is
  simpler and process-crash-released by the OS; the SQLite row is
  observable via `mnemo_doctor`. Likely the file lock, surfaced in a
  health check.

[anthropics/claude-code#27142]: https://github.com/anthropics/claude-code/issues/27142
[#60949]: https://github.com/anthropics/claude-code/issues/60949
