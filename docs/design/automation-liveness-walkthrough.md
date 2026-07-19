# Automation-liveness on the plugin surface (zero core edits)

*🎯T102.11 walkthrough. This document proves the automation-liveness
use-case maps entirely onto the mnemo plugin surface shipped under
🎯T102 — without shipping the product feature itself.*

## Premise

A 9-day silent stall (ytt / brew services) motivated a liveness
watcher: observe a signal, reconcile toward a fixed point, surface
health, notify, and show a UI. That consumer is **not** implemented
in mnemo core here. It is re-raised later as a separate target that
**depends on 🎯T102**.

## Surface inventory (all config-driven)

| Need | Plugin / config surface | Target |
|------|-------------------------|--------|
| Declare watcher | `plugins[]` entry in `~/.mnemo/config.json` | T102.2 |
| Run out-of-process | `transport: "launch"` + `command` | T102.4 |
| Or attach existing | `transport: "connect"` + `url` | T102.3 |
| Or lightweight JS | `transport: "inprocess"` + `script` | T102.6 |
| HTTP contract | `/ready`, `/manifest`, facets under `/plugins/<name>/` | T102.1, T102.5 |
| Periodic tick | manifest `facets.reconcile` → POST `/reconcile` via StreamReconciler | T102.7 |
| Health | `facets.check` → GET `/check` → `plugin.<name>.check` / ready diag | T102.7, T102.3 |
| Notify | `facets.notify` → POST `/notify` from diag OnAlert | T102.7 |
| Pure signals (no process) | `signal_sources[]` (file_mtime, launchd, newest_artifact, last_commit) | T102.8 |
| Popup UI | manifest `ui` → GET `/api/plugins` + WKWebView preview | T102.9 |
| Agent tools | manifest `mcp` + `/mcp/tools` + `/mcp/call` → `plugin_<name>__*` | T102.10 |
| Tear-down | disable plugin → process/handler stopped, proxy 404, tools cleared | T102.12 |

## Example config (no rebuild)

```json
{
  "plugins": [
    {
      "name": "liveness-ytt",
      "enabled": true,
      "transport": "launch",
      "command": "/opt/homebrew/bin/mnemo-plugin-liveness",
      "params": { "grace_multiple": 2, "job": "homebrew.mxcl.ytt" }
    }
  ],
  "signal_sources": [
    {
      "name": "ytt-log",
      "kind": "file_mtime",
      "path": "~/Library/Logs/ytt/heartbeat",
      "cadence": "15m",
      "grace_multiple": 2
    }
  ]
}
```

Hot-reload via `mnemo_config` starts/stops the plugin and re-evaluates
signals — **zero** mnemo source changes, **zero** Claude reconnect.

## Proof-of-surface today

`examples/plugins/echo-check/main.js` is an in-process plugin that
re-expresses a trivial health check. Enable it:

```json
{
  "plugins": [{
    "name": "echo-check",
    "enabled": true,
    "transport": "inprocess",
    "script": "<repo>/examples/plugins/echo-check/main.js"
  }]
}
```

Observations that match core behaviour:

- `GET /plugins/echo-check/check` → `{severity:"ok", detail:…}` (same shape as diag)
- `GET /api/plugins` lists the UI contribution
- `plugin.echo-check.ready` appears on `mnemo_doctor` / `/health`
- Disable clears proxy route, UI list entry, and facets

The bespoke core path for this trivial check is **shown redundant**:
any new health probe of this shape can ship as a plugin script instead
of a `diag.Check` registration in `internal/registry/diagchecks.go`.

## Boundaries

- Plugin homes under `~/.mnemo/plugins/` sit inside the loop-safety
  exclusion fence (T52) when registered at reconcile (T102.12).
- Facets use the single scheduler / diag registry — plugins do not
  start their own tick loops.
- External egress remains opt-in at the daemon; a launch plugin dials
  as its own process.

## Conclusion

Automation-liveness is **implementable entirely as config + a plugin
binary/script** on the T102 surface. Shipping that product remains a
separate objective; this walkthrough is the acceptance evidence for
🎯T102.11.
