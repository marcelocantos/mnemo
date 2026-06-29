# mnemo's native macOS head: extending the menu-bar UI beyond threads

A design catalog for bullseye target **T86** — "the Threads popup surfaces a
curated set of high-value mnemo-backed signals and actions beyond the basics."
T85 shipped the menu-bar app (`Mnemo.app`) as a thread navigator. This doc
reframes that app as mnemo's **native presence on macOS** and triages what to
build on it next.

## 1. The reframe

The daemon is headless, cross-platform, and owns all data and logic. The
menu-bar app is the *only* part of mnemo that is **resident in the GUI login
session, always-on, code-signed with a stable TCC identity, and able to call
AppKit / UserNotifications / App Intents / WidgetKit**. That is its reason to
exist. Today it spends that identity on one surface (the thread popover).

The design principle from the original threads-navigator doc still holds —
*business logic in Go, the shim is a thin view* — we only widen "view" to
**native-affordance broker**. The sharp filter for every candidate enhancement:

> **Does this need the native head, or is it just a view of `/api`?**

- *Just a view* (status counts, usage, activity dashboards) — real but low
  unique value; the web dashboard at `localhost:19419/` already renders these.
  Worth a **lean** native panel plus an "Open full dashboard" link, not a
  reimplementation.
- *Needs the native head* — notifications, global hotkeys, the status-item
  glyph, App Intents, a URL scheme, widgets. This is the shim's actual
  frontier, and the capabilities the daemon structurally cannot provide.

## 2. Candidate catalog

### 2a. Native-only frontier (the shim's reason to exist)

| Capability | macOS API | mnemo trigger / data | Value | Cost |
|---|---|---|---|---|
| **Proactive notifications** | `UNUserNotificationCenter` | health-fail transition (`diag`); TODO crossing due/overdue; important thread gone stale; ingest behind / breaker tripped; inbox note arrived (T65) | high | med |
| **Ambient status glyph + badge** | `NSStatusItem` | worst health severity → tinted glyph; overdue-TODO / unread-inbox count → badge | high | low |
| **Live status dashboard panel** | AppKit window | `/health` report + `/api/stats`,`/api/dbstats`,`/api/activity` | high | med |
| **Global quick-search / command palette** | global hotkey + borderless panel | `mnemo_search` over all transcripts/sessions/threads → act | high | high |
| **`mnemo://` URL scheme** | `CFBundleURLTypes` | deep-link `mnemo://thread/x`, `mnemo://session/y` — glue for notifications, web "open in app", Shortcuts | med | low |
| **Scriptable mnemo** | App Intents / Shortcuts | "go to thread X", "what's hot", "search mnemo" as Siri/Shortcuts actions | med | high |
| **Launch at login** | `SMAppService` | the always-on guarantee the ambient role depends on | med | low |

### 2b. Per-thread popover enrichments (the transient surface)

| Capability | mnemo source | Affordance | Value | Cost |
|---|---|---|---|---|
| "Where I left off" | `mnemo_compacted_session` | append last session's compaction summary under the CLAUDE.md preview | high | med |
| Due-soon TODO column | `mnemo_todos` (due-soon-N) | a third count column beside active/overdue | med | low |
| Staleness nudge | activity + marker | flag important-but-quiet threads | med | low |
| Per-thread decisions / search-this-thread | `mnemo_decisions`, `mnemo_search` | row context-menu actions | med | med |

## 3. The structural spine: a daemon→shim event stream

Don't bolt N poll loops onto the shim. The mechanism that makes the UI
*extensible* (rather than a pile of one-off polls) is **one server-sent-events
endpoint** the shim subscribes to:

```
GET /api/events   →  text/event-stream
```

The daemon emits typed events; the shim fans each one out to up to three
consumers — a **notification**, the **ambient glyph/badge**, and **live UI**.
A new capability later is a new event type + a new consumer, not a new poll
loop. The daemon already runs an HTTP server, so this is cheap, and it removes
the polling the dashboard panel and glyph would otherwise need.

**Event types (this slice ships the health ones; the schema is open):**

- `health` — a `diag.Report` snapshot, published on every scheduler pass
  (startup, every fast tick, hourly full). Drives the dashboard panel and the
  status glyph live.
- `alert` — a health *transition* (`{name, severity, detail, remediation,
  kind: fail|recovery}`), published by the existing `diag.Notifier` whenever it
  decides (with its dedup + cooldown) that a notification is warranted. Drives a
  native `UNNotification`.

Future event types (out of scope here): `todo_due`, `thread_stale`,
`inbox_note`, `backfill_done`.

### Avoiding double notifications

The daemon already fires OS notifications itself (`diag/notify.go` → `osascript`
/ `notify-send`). With the native shim now rendering richer notifications, both
firing would double them. Resolution, preserving the headless/Linux path:

- The `Notifier`'s delivery becomes a pluggable callback that receives a
  structured `Alert` (not a pre-formatted title/body), so the shim can format
  natively. The *decision* logic (fail/recovery, dedup, cooldown) stays in Go,
  unchanged.
- `main.go` wires that callback to: **if the `/api/events` hub has ≥1
  subscriber (the native shim is connected), publish an `alert` event; else
  fall back to `osSend`.** A headless or Linux daemon with no shim keeps exactly
  today's behaviour.

## 4. Triage & selected first slice

The catalog is large; T86 asks for the highest-value slice implemented end to
end. **Selected: the live status dashboard panel + the health notification,
both fed by the `/api/events` SSE spine** (plus the status-item glyph as a cheap
third consumer of the same stream). Rationale:

- It builds directly on an *existing, proven* feature — the `diag` health
  subsystem (T83/T84) already computes the report, the transitions, the dedup,
  and a deep-link to `/#health`. We are giving it a native head, not inventing a
  signal.
- It establishes the SSE spine that every later native capability reuses.
- The data is almost entirely already served (`/health`, `/api/stats`,
  `/api/dbstats`, `/api/activity`), so the daemon work is small.
- It makes the menu bar earn its place: "is mnemo healthy and current?" at a
  glance, with one click to *why*.

### 4a. Slice components

**Daemon (Go):**

1. `internal/api/events.go` — an SSE hub (subscriber registry, `text/event-stream`,
   `http.Flusher`, per-client buffered channel, keepalive ping) behind
   `GET /api/events`. Exposes `Publish(event)` and `HasSubscribers()`.
2. Publish a `health` event from `diag.Scheduler.runOnce` after each run.
3. Refactor `diag.Notifier` delivery to a structured `Alert` callback; wire it
   in `main.go` to publish `alert` events when the hub has subscribers, else
   `osSend`.

**Shim (Swift):**

4. `EventStream.swift` — an SSE client (URLSession streaming task, line parser,
   reconnect-with-backoff). Decodes `health` / `alert` events.
5. `DashboardWindowController.swift` — a standalone window mirroring the web
   `#health` page natively (overall badge, ok/warn/fail counts, per-check rows
   with detail + remediation + tier), live-updated from `health` events, with an
   "Open full dashboard" button → web. A standalone window (not a Settings tab)
   so it is reusable as a notification deep-link target and openable from the
   status item.
6. Native notifications — `UNUserNotificationCenter` authorization + a category
   with an "Open dashboard" action; post on `alert` events; clicking opens the
   dashboard window.
7. Status-item glyph reflects worst severity from the latest `health` event.

### 4b. Explicitly deferred

The per-thread popover enrichments (§2b), quick-search palette, URL scheme, App
Intents, and the other event types are **out of scope for this slice** but
recorded here as the backlog the same spine unlocks.

## 5. Human gate

T86's acceptance carries a human gate: *the user confirms the selected set is
the right direction before broad implementation.* Confirmed in-session
(2026-06-19): build the dashboard panel + a related health notification, fed by
daemon-push SSE.
