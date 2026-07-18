// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pluginHandler serves /ready + /manifest for connect-mode tests (🎯T102.3).
func pluginHandler(t *testing.T, name string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			_ = json.NewEncoder(w).Encode(ReadyBody{OK: true})
		case "/manifest":
			_ = json.NewEncoder(w).Encode(Manifest{
				ProtocolVersion: ProtocolVersionCurrent,
				Name:            name,
				Version:         "0.1.0",
				Facets:          Facets{Check: true},
				UI:              &UISurface{Label: "Test", PreviewPath: "ui/preview"},
				ConfigSchema: map[string]any{
					"type": "object",
				},
			})
		default:
			http.NotFound(w, r)
		}
	})
}

func TestManagerReconcileConnectStartsAndStops(t *testing.T) {
	srv := httptest.NewServer(pluginHandler(t, "lab"))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	m := NewManager(home, srv.Client(), testLogger())
	t.Cleanup(m.Close)
	ctx := context.Background()

	m.Reconcile(ctx, []store.PluginEntry{{
		Name:      "lab",
		Enabled:   true,
		Transport: store.PluginTransportConnect,
		URL:       srv.URL,
		Params:    map[string]any{"grace_multiple": 2.0},
	}})

	snap, ok := m.Get("lab")
	if !ok {
		t.Fatal("expected lab instance")
	}
	if snap.State != StateReady {
		t.Fatalf("state: got %s want %s (err=%q)", snap.State, StateReady, snap.Err)
	}
	if snap.Manifest == nil || snap.Manifest.Version != "0.1.0" {
		t.Fatalf("manifest: %+v", snap.Manifest)
	}
	if snap.Manifest.UI == nil || snap.Manifest.UI.Label != "Test" {
		t.Fatalf("ui metadata missing: %+v", snap.Manifest.UI)
	}
	wantHome := filepath.Join(home, ".mnemo", "plugins", "lab")
	if snap.Home != wantHome {
		t.Errorf("home: got %q want %q", snap.Home, wantHome)
	}

	// Disable tears down without removing the config-shaped tracking
	// only when we re-reconcile with enabled=false — instance may be
	// deleted if entry is gone; with enabled=false entry stays tracked
	// as stopped.
	m.Reconcile(ctx, []store.PluginEntry{{
		Name:      "lab",
		Enabled:   false,
		Transport: store.PluginTransportConnect,
		URL:       srv.URL,
	}})
	snap, ok = m.Get("lab")
	if !ok {
		t.Fatal("disabled entry should remain tracked as stopped")
	}
	if snap.State != StateStopped {
		t.Fatalf("after disable: state=%s want stopped", snap.State)
	}
	if snap.Manifest != nil || snap.BaseURL != "" {
		t.Fatalf("after disable: metadata should be cleared: %+v", snap)
	}

	// Remove entry entirely.
	m.Reconcile(ctx, nil)
	if _, ok := m.Get("lab"); ok {
		t.Fatal("removed entry should not be tracked")
	}
}

func TestManagerReconcileInProcessIsConfiguredPending(t *testing.T) {
	// 🎯T102.6 not wired yet — inprocess stays configured-pending.
	home := t.TempDir()
	m := NewManager(home, nil, testLogger())
	t.Cleanup(m.Close)

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "tiny",
		Enabled:   true,
		Transport: store.PluginTransportInProcess,
		Script:    "main.js",
	}})
	snap, ok := m.Get("tiny")
	if !ok {
		t.Fatal("expected instance")
	}
	if snap.State != StateConfigured {
		t.Fatalf("inprocess pending: state=%s want configured", snap.State)
	}
	if snap.BaseURL != "" {
		t.Fatalf("inprocess pending should have no base URL yet: %q", snap.BaseURL)
	}
}

func TestManagerConnectNameMismatch(t *testing.T) {
	srv := httptest.NewServer(pluginHandler(t, "other"))
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "lab",
		Enabled:   true,
		Transport: store.PluginTransportConnect,
		URL:       srv.URL,
	}})
	snap, _ := m.Get("lab")
	if snap.State != StateError {
		t.Fatalf("state=%s want error", snap.State)
	}
	if snap.Err == "" {
		t.Fatal("expected error message")
	}
}

func TestManagerConnectReadyFailure(t *testing.T) {
	// Manifest-only server: no /ready → connect fails with clear error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			_ = json.NewEncoder(w).Encode(Manifest{
				ProtocolVersion: ProtocolVersionCurrent,
				Name:            "lab",
				Version:         "1",
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "lab",
		Enabled:   true,
		Transport: store.PluginTransportConnect,
		URL:       srv.URL,
	}})
	snap, _ := m.Get("lab")
	if snap.State != StateError {
		t.Fatalf("state=%s want error (err=%q)", snap.State, snap.Err)
	}
	if snap.BaseURL == "" {
		t.Fatal("error state should retain attempted BaseURL for diag")
	}
}

func TestAttachConnectAndDiag(t *testing.T) {
	srv := httptest.NewServer(pluginHandler(t, "lab"))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	att, err := AttachConnect(ctx, srv.Client(), srv.URL, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if att.BaseURL == "" || att.Manifest == nil || att.Manifest.Name != "lab" {
		t.Fatalf("attach result: %+v", att)
	}

	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	checks := m.DynamicChecks()
	byName := map[string]bool{}
	for _, c := range checks {
		byName[c.Name] = true
	}
	if !byName["plugin.lab.ready"] {
		t.Fatalf("missing ready check: %+v", checks)
	}
	// Ready + Facets.Check → check facet is registered alongside ready.
	if !byName["plugin.lab.check"] {
		t.Fatalf("expected check facet when ready+Facets.Check: %+v", checks)
	}
	var resOK string
	for _, c := range checks {
		if c.Name == "plugin.lab.ready" {
			resOK = c.Run(ctx).Severity.String()
			break
		}
	}
	if resOK != "ok" {
		t.Fatalf("diag ready: %s", resOK)
	}

	// Unreachable URL → fail severity on ready probe; facet drops (not ready).
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect,
		URL: "http://127.0.0.1:1",
	}})
	checks = m.DynamicChecks()
	if len(checks) != 1 || checks[0].Name != "plugin.lab.ready" {
		t.Fatalf("error-state checks: %+v", checks)
	}
	res := checks[0].Run(ctx)
	if res.Severity.String() != "fail" {
		t.Fatalf("unreachable should fail diag: %+v", res)
	}
	if res.Remediation == "" {
		t.Fatal("expected remediation")
	}
}

func TestFetchManifest(t *testing.T) {
	srv := httptest.NewServer(pluginHandler(t, "x"))
	t.Cleanup(srv.Close)
	man, err := FetchManifest(context.Background(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if man.Name != "x" || man.ProtocolVersion != ProtocolVersionCurrent {
		t.Fatalf("unexpected manifest: %+v", man)
	}
}

func TestFetchManifestBadProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol_version": 99,
			"name":             "x",
			"version":          "1",
			"facets":           map[string]bool{},
		})
	}))
	t.Cleanup(srv.Close)
	_, err := FetchManifest(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected protocol error")
	}
}

func TestProbeReady(t *testing.T) {
	srv := httptest.NewServer(pluginHandler(t, "x"))
	t.Cleanup(srv.Close)
	if err := ProbeReady(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
}

func TestExpandPluginPath(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "me")
	// Use a real absolute path for the platform (Windows rejects bare "/abs/p").
	abs := filepath.Join(t.TempDir(), "abs", "p")
	ph := store.PluginHome(home, "foo")
	if got := store.ExpandPluginPath("~/bin/p", home, ph); got != filepath.Join(home, "bin", "p") {
		t.Errorf("tilde: %q", got)
	}
	if got := store.ExpandPluginPath("bin/p", home, ph); got != filepath.Join(ph, "bin", "p") {
		t.Errorf("relative: %q", got)
	}
	if got := store.ExpandPluginPath(abs, home, ph); got != abs {
		t.Errorf("abs: %q want %q", got, abs)
	}
}
