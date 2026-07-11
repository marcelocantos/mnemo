# mnemo plugin system

*Status: design note — 2026-07-10. Anchors 🎯T102 (parent) and 🎯T102.1
(this document). Mechanism decisions from the 2026-07-01 design
discussion; motivating consumer is automation-liveness (exploration
sketch on `automation-liveness-plugin`, former framing T98).*

**Tracking.** Design source for 🎯T102.1. Downstream targets T102.2–T102.12
implement the contract this note pins. Nothing here requires a rebuild of
mnemo to add a plugin.

---

## TL;DR

Plugins are **out-of-process HTTP servers** (or an in-process interpreter
that *presents* as one). mnemo never loads Go `.so` plugins and never
ships a compiled-in registry of extension types. One wire contract —
manifest + facets + UI under `/plugins/<name>/` — covers launch-mode,
connect-mode, and interpreted hosts. Backend facets adapt into the
existing single scheduler (🎯T68.7), diag registry (🎯T83), and notifier;
UI contributions render data-driven in the menu-bar popup via a live
WKWebView. Trust posture is **keep-open**: plugin pages share the local
origin and may call `/api/*`.

---

## 1. Mechanism fork (resolved)

### Chosen model: out-of-process by default

A plugin is a process (or an interpreter-hosted HTTP handler that
behaves identically) that mnemo talks to over HTTP. Adding, updating, or
removing a plugin is a config change plus a binary/script on disk — **not
a mnemo rebuild, not a relink, not a restart of Claude Code clients**.

Two transports, MCP-style:

| Transport | How it starts | Lifecycle owner | Default? |
|---|---|---|---|
| **launch** | mnemo spawns an executable; child prints its listen port on stdout | mnemo (restart/backoff/reap) | yes |
| **connect** | mnemo attaches to a configured base URL | external (mnemo only probes) | alternative |

Plus a third *presentation* path that reuses the same HTTP contract:

| Host | How it starts | Lifecycle owner |
|---|---|---|
| **in-process interpreted** (goja/Lua) | mnemo loads a script and mounts it as `http.Handler` at `/plugins/<name>/` | mnemo |

The interpreted path is not a second protocol. It is a mount source that
satisfies the same manifest + facet + UI endpoints so proxy, UI, and
facet adapters stay single-path.

### Explicitly rejected

| Alternative | Why rejected |
|---|---|
| **Go `-buildmode=plugin` (`.so`)** | No Windows support (🎯T89 / Windows head is a first-class platform). CGO + `sqlite_fts5` forces ABI lock-step with the host binary. Exact Go version lock between host and plugin. No unload — a bad plugin poisons the process for the rest of the daemon lifetime. |
| **Compiled-in Go registries** (`RegisterPlugin(...)` linked into `main`)** | Requires rebuilding mnemo for every extension. Contradicts "parameterised entirely through `~/.mnemo/config.json`". Turns every experiment into a core PR. |
| **gRPC / custom binary RPC as the primary contract** | UI already forces every plugin to speak HTTP (live WKWebView pages). A second RPC stack doubles the surface for no gain; facets reuse the plugin's HTTP server. |
| **MCP-only plugins with no HTTP** | MCP is a *facet* (tool bridge via T15 federation machinery, 🎯T102.10), not the host contract. UI + reconcile + check still need HTTP. |

---

## 2. URL contract

Every enabled plugin is addressable under a single, transport-agnostic
prefix on the mnemo daemon:

```
http://localhost:19419/plugins/<name>/...
```

Properties:

- **Transport-agnostic.** Launch, connect, and in-process mounts all
  appear at the same path. Clients (browser, Swift shim, facet adapters)
  never learn the child port or the connect URL.
- **Same origin as the dashboard.** Plugin pages load from `:19419`, so
  they share the dashboard/API origin. No CORS. Cookies and
  `fetch('/api/...')` work without special headers.
- **Subtree ownership.** Everything under `/plugins/<name>/` belongs to
  the plugin: its HTML/JS, its conforming facet endpoints, and any
  custom APIs it chooses to expose.
- **Unknown / disabled names → clean 404.** Never a proxy panic or a
  hung upstream dial.

mnemo reverse-proxies (or serves locally for interpreted plugins)
`/plugins/<name>/*` to the instance. WebSocket and SSE upgrades are
forwarded so streaming UIs work (🎯T102.5).

---

## 3. Wire contract (HTTP)

Protocol version is negotiated via the manifest. A breaking change to
this section bumps the protocol version; mnemo refuses to activate an
incompatible plugin and surfaces a diag failure.

### 3.1 Readiness probe

```
GET /plugins/<name>/ready
```

- **200** with a small JSON body (`{"ok": true}`) means the instance is
  ready to serve facets and UI.
- Any other status, timeout, or dial error → instance not ready; diag
  check `plugin.<name>.ready` reports fail/warn.
- mnemo polls this after launch/connect before registering facets or
  advertising UI contributions.

(Plugins may implement readiness at their root `/ready` and rely on the
proxy path; the public address is always under `/plugins/<name>/`.)

### 3.2 Manifest / describe

```
GET /plugins/<name>/manifest
```

Required JSON shape (field names fixed; unknown fields ignored for
forward compatibility):

```json
{
  "protocol_version": 1,
  "name": "automation-liveness",
  "version": "0.1.0",
  "description": "Cross-automation staleness watch",
  "facets": {
    "signal": true,
    "reconcile": true,
    "check": true,
    "notify": true,
    "mcp": false
  },
  "ui": {
    "label": "Liveness",
    "icon": "heart",
    "preview_path": "ui/preview",
    "page_path": "ui/",
    "menu": "footer"
  },
  "config_schema": {
    "type": "object",
    "properties": {
      "grace_multiple": { "type": "number", "default": 2 }
    }
  },
  "mcp": {
    "transport": "http",
    "path": "mcp"
  }
}
```

| Field | Required | Role |
|---|---|---|
| `protocol_version` | yes | Integer; mnemo accepts a known set (starts at `1`). |
| `name` | yes | Must match the config key / URL segment. |
| `version` | yes | Plugin's own semver (informational + diag). |
| `description` | no | Human one-liner. |
| `facets.*` | yes | Booleans declaring which facet endpoints exist. |
| `ui` | no | If present, contributes a popup menu entry (🎯T102.9). |
| `config_schema` | no | JSON Schema for per-plugin params in config; validated on write. |
| `mcp` | no | If present, declares an MCP endpoint to bridge (🎯T102.10). |

**Metadata is discovered from the manifest, not duplicated in config.**
Config only names the plugin, its transport, enable flag, and params
(🎯T102.2).

### 3.3 Facet endpoints

Facets are the backend extension points. Each is optional; the manifest
declares which are present. Adapters in mnemo (🎯T102.7) turn them into
existing core primitives — **plugins never register their own
scheduler or notifier loop**.

| Facet | Method + path | Core adapter | Semantics |
|---|---|---|---|
| **signal** | `GET …/signal` | declarative signal-source feed / reconciler input | Current observation (e.g. last progress timestamp, raw readings). Idempotent read. |
| **reconcile** | `POST …/reconcile` | `StreamReconciler.Reconcile` (🎯T68.7) | Drive the plugin one tick toward its fixed point. Body may carry a small context envelope (time, cursors). |
| **check** | `GET …/check` | `diag.Check` (🎯T83) | Health snapshot: `{severity, detail, remediation?}`. Flows to `mnemo_doctor`, OS notification, `/health`. |
| **notify** | `POST …/notify` | notifier sink | Deliver a notification the core has already decided to fire (title/body/url). Plugin may no-op if it only *produces* checks. |
| **mcp** | per manifest | T15 federation bridge | Tools appear on mnemo's MCP list, namespaced to the plugin. |

**Failure isolation.** A slow or broken facet degrades to a diag
warning/failure and trips the 🎯T84 circuit-breaker. It must not wedge
the single scheduler or the reconciler dispatcher. Timeouts are
enforced on the mnemo side of every HTTP call.

### 3.4 UI routes

If `ui` is present in the manifest:

- `preview_path` — loaded in the popup's WKWebView via `URLRequest`
  (live document, not `loadHTMLString`).
- `page_path` — full page (menu open-in-browser / future tab).
- Both resolve relative to `/plugins/<name>/`.

JS on those pages may call same-origin `/api/*` and
`/plugins/<name>/*`. Streaming uses WS/SSE through the proxy.

Force-reload: mnemo emits a `plugin.reload` event on the existing
T86 SSE hub; the Swift shim reloads the affected WKWebView (🎯T102.9).

---

## 4. Launch handshake

For **launch** transport:

1. mnemo starts the configured executable with a clean env plus
   plugin params as env/flags (exact injection shape is an
   implementation detail of T102.4; must be documented there).
2. Child binds `127.0.0.1:<ephemeral>` and writes a single handshake
   line to stdout, then serves HTTP:
   ```
   MNEMO_PLUGIN_PORT <port>\n
   ```
3. mnemo reads the port, builds `http://127.0.0.1:<port>`, and treats
   the instance identically to connect-mode thereafter.
4. On crash: restart with backoff; persistent failure → T84 breaker,
   diag fail, UI contribution withdrawn.
5. On disable / daemon drain: SIGTERM the child, wait, then SIGKILL;
   unmount proxy routes; deregister facets.

This reuses the subprocess-supervision patterns already present
(`shimSupervisor`, claudia workers) rather than inventing a second
supervisor.

---

## 5. Config surface (preview; 🎯T102.2 owns the schema)

```json
{
  "plugins": [
    {
      "name": "automation-liveness",
      "enabled": true,
      "transport": "launch",
      "command": "/opt/homebrew/bin/mnemo-plugin-liveness",
      "args": [],
      "params": { "grace_multiple": 2 }
    },
    {
      "name": "lab-ui",
      "enabled": true,
      "transport": "connect",
      "url": "http://127.0.0.1:9091"
    },
    {
      "name": "tiny-check",
      "enabled": true,
      "transport": "inprocess",
      "script": "~/.mnemo/plugins/tiny-check/main.js"
    }
  ]
}
```

Hot-reload mirrors `vault_path`: enable starts an instance, disable
tears one down, param changes restart or re-POST config per plugin
policy. No daemon restart, no Claude reconnect.

Optional on-disk home: `~/.mnemo/plugins/<name>/`. Config may point
anywhere.

---

## 6. Trust posture: keep-open (explicit decision)

**Decision.** Plugin pages share the mnemo dashboard origin
(`http://localhost:19419`) and **may call the auth-less `/api/*`
surface**.

**Rationale.**

- mnemo is a single-user, localhost daemon. The dashboard and MCP
  endpoint are already reachable without auth on loopback.
- Same-origin is what makes live WKWebView previews useful (JS +
  `fetch('/api/threads')` + SSE) without inventing a second auth or
  CORS layer.
- Isolating plugins onto a separate origin would force either a
  token-passing scheme or a crippled UI; both cost more than they buy
  on a single-user machine.

**Not an oversight.** This is a conscious single-user trade-off. If
mnemo ever grows multi-user auth on `/api/*`, plugin pages must adopt
the same credentials; until then, **any code running in a plugin page
is as trusted as the dashboard itself**. Plugins are therefore
user-installed software, not untrusted third-party web content.

**Boundaries that still hold:**

- Loop-safety exclusion fence (🎯T52): plugin-generated content under
  `~/.mnemo/` / vault paths is not re-ingested into a feedback loop
  (🎯T102.12).
- External egress remains opt-in at the daemon level; a plugin that
  dials the network does so as its own process (user-installed), not
  by inheriting new mnemo egress defaults.
- Additive-only schema: plugin persistence, if any, is new
  tables/columns — never destructive migrations.

---

## 7. How facets map onto core (summary for implementers)

```
config hot-reload
    │
    ▼
plugin registry ──launch/connect/inprocess──► instance (base URL or local handler)
    │                                              │
    │ manifest                                     │
    ▼                                              ▼
facet adapters                            reverse proxy /plugins/<name>/*
    │                                              │
    ├─ reconcile ──► StreamReconciler (T68.7)      ├─ UI (WKWebView)
    ├─ check     ──► diag.Check (T83)              └─ custom APIs
    ├─ notify    ──► notifier
    ├─ signal    ──► signal-source / reconcile in
    └─ mcp       ──► federation tool bridge (T15)
```

No plugin introduces its own tick loop. The single scheduler owns time;
adapters own HTTP.

---

## 8. Declarative signal sources (🎯T102.8, non-process)

The common liveness signals — file/log mtime, launchd
`LastExitStatus`+PID, newest-artifact timestamp, last commit — are
expressible as pure config stanzas evaluated by mnemo, feeding the same
scheduler/health/notifier surface **without a plugin process**. That
path is a first-class peer of process plugins for the automation-
liveness consumer: poll-first, zero work for the automation.

This document does not specify the stanza schema; T102.8 does. It is
called out here so the mechanism is understood as "HTTP plugins +
declarative signals", not "HTTP plugins only".

---

## 9. Proof obligations (downstream, not this note)

| Target | Obligation |
|---|---|
| 🎯T102.11 | Re-express one existing capability as a plugin with identical behaviour; written walkthrough that automation-liveness maps onto signal+reconcile+check+notify+UI with zero core edits. |
| 🎯T102.12 | Fence, additive schema, opt-in egress, single scheduler, clean teardown on disable/hot-reload. |

---

## 10. Non-goals (for T102.1 / the mechanism)

- Shipping automation-liveness itself (re-raised after T102 under a
  fresh id; T98 is unusable due to git-history collision).
- Multi-tenant auth, remote plugin marketplaces, signed plugin
  packages.
- Windows system-tray specifics for plugin menus (🎯T89 consumes the
  same `/api/plugins` enumeration when it lands).
- Changing the append-only schema policy or the opt-in egress posture.

---

## 11. Acceptance checklist (🎯T102.1)

- [x] Mechanism fork resolved in writing; Go `-buildmode=plugin` and
  compiled-in registries rejected with reasons (§1).
- [x] One HTTP wire contract: manifest/describe, facet conventions
  (signal/reconcile/check/notify), readiness probe (§3).
- [x] URL contract fixed: `/plugins/<name>/`, transport-agnostic (§2).
- [x] Keep-open trust posture documented as an explicit decision (§6).

Implementation of transports, proxy, facets, UI, MCP bridge, proof, and
boundary enforcement is deliberately **out of scope** for this note —
owned by 🎯T102.2–T102.12.
