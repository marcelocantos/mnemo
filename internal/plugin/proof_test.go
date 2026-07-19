// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// 🎯T102.11: ship the echo-check example as an in-process plugin and
// assert the real check endpoint shape matches diag health semantics.
func TestProofOfSurfaceEchoCheck(t *testing.T) {
	// Resolve examples/plugins/echo-check/main.js relative to this file.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "..", "examples", "plugins", "echo-check", "main.js")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("proof plugin missing: %v", err)
	}
	// Walkthrough doc must exist.
	doc := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "design", "automation-liveness-walkthrough.md")
	if _, err := os.Stat(doc); err != nil {
		t.Fatalf("walkthrough missing: %v", err)
	}

	m := NewManager(t.TempDir(), nil, testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "echo-check", Enabled: true,
		Transport: store.PluginTransportInProcess, Script: script,
	}})
	snap, _ := m.Get("echo-check")
	if snap.State != StateReady {
		t.Fatalf("proof plugin: state=%s err=%q", snap.State, snap.Err)
	}

	rr := httptest.NewRecorder()
	ProxyHandler(m).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/plugins/echo-check/check", nil))
	if rr.Code != 200 {
		t.Fatalf("check status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Severity string `json:"severity"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Severity != "ok" || body.Detail == "" {
		t.Fatalf("check body: %+v", body)
	}
	// UI contribution present (popup menu surface).
	found := false
	for _, c := range m.UIContributions() {
		if c.Name == "echo-check" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected UI contribution for echo-check")
	}
}
