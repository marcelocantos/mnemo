// echo-check — proof-of-surface plugin (🎯T102.11)
//
// Re-expresses a trivial health/diag check as an in-process plugin with
// no mnemo rebuild. Config:
//
//   "plugins": [{
//     "name": "echo-check",
//     "enabled": true,
//     "transport": "inprocess",
//     "script": "examples/plugins/echo-check/main.js"
//   }]
//
// Then: GET /plugins/echo-check/check → {severity, detail}
// and plugin_echo-check appears under GET /api/plugins when ui is set.

function handle(req) {
  var p = req.path;
  if (p === "/ready" || p === "/ready/") {
    return json(200, { ok: true });
  }
  if (p === "/manifest" || p === "/manifest/") {
    return json(200, {
      protocol_version: 1,
      name: pluginName,
      version: "0.1.0",
      description: "Proof-of-surface: trivial health check as a plugin",
      facets: { check: true, signal: false, reconcile: false, notify: false, mcp: false },
      ui: {
        label: "Echo check",
        icon: "checkmark.circle",
        preview_path: "ui",
        page_path: "ui",
        menu: "footer"
      }
    });
  }
  if (p === "/check" || p === "/check/") {
    // Mirrors a core diag.Healthy result shape used by mnemo_doctor.
    return json(200, {
      severity: "ok",
      detail: "echo-check plugin healthy (proof-of-surface 🎯T102.11)"
    });
  }
  if (p === "/ui" || p === "/ui/") {
    return {
      status: 200,
      headers: { "Content-Type": "text/html; charset=utf-8" },
      body: "<!DOCTYPE html><html><body style=\"font:14px system-ui;padding:12px\">" +
        "<h1>Echo check</h1><p>In-process proof plugin. Core path is redundant for this check.</p>" +
        "</body></html>"
    };
  }
  return { status: 404, body: "not found" };
}

function json(status, obj) {
  return {
    status: status,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(obj)
  };
}
