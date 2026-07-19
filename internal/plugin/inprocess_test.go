// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

const sampleJSPlugin = `
function handle(req) {
  var p = req.path;
  if (p === "/ready" || p === "/ready/") {
    return { status: 200, headers: {"Content-Type":"application/json"}, body: '{"ok":true}' };
  }
  if (p === "/manifest" || p === "/manifest/") {
    return {
      status: 200,
      headers: {"Content-Type":"application/json"},
      body: JSON.stringify({
        protocol_version: 1,
        name: pluginName,
        version: "0.1.0",
        description: "in-process sample",
        facets: { check: true, signal: false, reconcile: false, notify: false, mcp: false },
        ui: { label: "InProc", preview_path: "ui", menu: "footer" }
      })
    };
  }
  if (p === "/ui" || p === "/ui/") {
    return { status: 200, headers: {"Content-Type":"text/html"}, body: "<html><body>inproc</body></html>" };
  }
  if (p === "/check" || p === "/check/") {
    return { status: 200, body: JSON.stringify({severity:"ok", detail:"in-process healthy"}) };
  }
  return { status: 404, body: "not found" };
}
`

func TestInProcessStartsProxyAndTeardown(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "main.js")
	if err := os.WriteFile(script, []byte(sampleJSPlugin), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.TempDir(), nil, testLogger())
	t.Cleanup(m.Close)

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "inproc",
		Enabled:   true,
		Transport: store.PluginTransportInProcess,
		Script:    script,
	}})
	snap, ok := m.Get("inproc")
	if !ok || snap.State != StateReady {
		t.Fatalf("state=%v ok=%v err=%q", snap.State, ok, snap.Err)
	}
	if snap.BaseURL == "" || snap.Manifest == nil || snap.Manifest.Name != "inproc" {
		t.Fatalf("attach: %+v", snap)
	}

	// Proxy identity: same /plugins path as external plugins.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugins/inproc/check", nil)
	ProxyHandler(m).ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("proxy check status=%d body=%s", rr.Code, rr.Body.String())
	}
	contribs := m.UIContributions()
	if len(contribs) != 1 || contribs[0].Label != "InProc" {
		t.Fatalf("ui: %+v", contribs)
	}

	// Disable tears down local server — subsequent proxy 404s.
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "inproc", Enabled: false, Transport: store.PluginTransportInProcess, Script: script,
	}})
	rr = httptest.NewRecorder()
	ProxyHandler(m).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/plugins/inproc/check", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("after disable status=%d", rr.Code)
	}
	if len(m.UIContributions()) != 0 {
		t.Fatal("ui should be empty after disable")
	}
}
