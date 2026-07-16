// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// proxyBackend serves ready/manifest for connect attach plus custom
// routes used by proxy tests.
func proxyBackend(t *testing.T, name string, onRequest func(r *http.Request)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r)
		}
		switch r.URL.Path {
		case "/ready":
			_ = json.NewEncoder(w).Encode(ReadyBody{OK: true})
		case "/manifest":
			_ = json.NewEncoder(w).Encode(Manifest{
				ProtocolVersion: ProtocolVersionCurrent,
				Name:            name,
				Version:         "0.1.0",
			})
		case "/hello":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello-from-plugin"))
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte("data: ping\n\n"))
		case "/echo-upgrade":
			// Reflect upgrade-related headers so the proxy test can assert
			// they were forwarded without completing a full WS handshake.
			w.Header().Set("X-Got-Upgrade", r.Header.Get("Upgrade"))
			w.Header().Set("X-Got-Connection", r.Header.Get("Connection"))
			_, _ = w.Write([]byte("ok"))
		default:
			if strings.HasPrefix(r.URL.Path, "/api/") {
				_, _ = w.Write([]byte("path=" + r.URL.Path + " q=" + r.URL.RawQuery))
				return
			}
			http.NotFound(w, r)
		}
	})
}

func readyManager(t *testing.T, backendURL string, client *http.Client) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), client, testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "lab",
		Enabled:   true,
		Transport: store.PluginTransportConnect,
		URL:       backendURL,
	}})
	snap, ok := m.Get("lab")
	if !ok || snap.State != StateReady {
		t.Fatalf("setup: lab not ready: ok=%v state=%v err=%q", ok, snap.State, snap.Err)
	}
	return m
}

func TestProxyHandlerForwardsPathAndBody(t *testing.T) {
	var sawPath string
	backend := httptest.NewServer(proxyBackend(t, "lab", func(r *http.Request) {
		if r.URL.Path == "/hello" {
			sawPath = r.URL.Path
		}
	}))
	t.Cleanup(backend.Close)

	m := readyManager(t, backend.URL, backend.Client())
	front := httptest.NewServer(ProxyHandler(m))
	t.Cleanup(front.Close)

	resp, err := front.Client().Get(front.URL + "/plugins/lab/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if string(body) != "hello-from-plugin" {
		t.Fatalf("body=%q", body)
	}
	if sawPath != "/hello" {
		t.Fatalf("backend path=%q want /hello (prefix must be stripped)", sawPath)
	}
}

func TestProxyHandlerStripsPrefixForNestedPathAndQuery(t *testing.T) {
	backend := httptest.NewServer(proxyBackend(t, "lab", nil))
	t.Cleanup(backend.Close)
	m := readyManager(t, backend.URL, backend.Client())
	front := httptest.NewServer(ProxyHandler(m))
	t.Cleanup(front.Close)

	resp, err := front.Client().Get(front.URL + "/plugins/lab/api/v1/items?limit=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	want := "path=/api/v1/items q=limit=2"
	if string(body) != want {
		t.Fatalf("body=%q want %q", body, want)
	}
}

func TestProxyHandlerUnknownName404(t *testing.T) {
	m := NewManager(t.TempDir(), nil, testLogger())
	t.Cleanup(m.Close)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugins/nope/hello", nil)
	ProxyHandler(m).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
}

func TestProxyHandlerNilManager404(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugins/lab/hello", nil)
	ProxyHandler(nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
}

func TestProxyHandlerDisabled404(t *testing.T) {
	backend := httptest.NewServer(proxyBackend(t, "lab", nil))
	t.Cleanup(backend.Close)
	m := readyManager(t, backend.URL, backend.Client())
	// Disable → stopped, BaseURL cleared.
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "lab",
		Enabled:   false,
		Transport: store.PluginTransportConnect,
		URL:       backend.URL,
	}})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugins/lab/hello", nil)
	ProxyHandler(m).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled: status=%d want 404", rr.Code)
	}
}

func TestProxyHandlerNotReady404(t *testing.T) {
	// Launch transport stays configured with empty BaseURL (T102.4 pending).
	m := NewManager(t.TempDir(), nil, testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "lab",
		Enabled:   true,
		Transport: store.PluginTransportLaunch,
		Command:   "/opt/bin/plugin",
	}})
	snap, _ := m.Get("lab")
	if snap.State != StateConfigured {
		t.Fatalf("setup state=%s", snap.State)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugins/lab/hello", nil)
	ProxyHandler(m).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("not ready: status=%d want 404", rr.Code)
	}
}

func TestProxyHandlerErrorState404(t *testing.T) {
	// Connect failure keeps BaseURL for diag but StateError must not proxy.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // no /ready
	}))
	t.Cleanup(backend.Close)
	m := NewManager(t.TempDir(), backend.Client(), testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: backend.URL,
	}})
	snap, _ := m.Get("lab")
	if snap.State != StateError || snap.BaseURL == "" {
		t.Fatalf("setup: state=%s base=%q", snap.State, snap.BaseURL)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugins/lab/hello", nil)
	ProxyHandler(m).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("error state: status=%d want 404", rr.Code)
	}
}

func TestProxyHandlerSSEContentType(t *testing.T) {
	backend := httptest.NewServer(proxyBackend(t, "lab", nil))
	t.Cleanup(backend.Close)
	m := readyManager(t, backend.URL, backend.Client())
	front := httptest.NewServer(ProxyHandler(m))
	t.Cleanup(front.Close)

	resp, err := front.Client().Get(front.URL + "/plugins/lab/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	if !strings.Contains(string(body), "data: ping") {
		t.Fatalf("body=%q", body)
	}
}

func TestProxyHandlerUpgradeHeadersPassThrough(t *testing.T) {
	backend := httptest.NewServer(proxyBackend(t, "lab", nil))
	t.Cleanup(backend.Close)
	m := readyManager(t, backend.URL, backend.Client())
	front := httptest.NewServer(ProxyHandler(m))
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodGet, front.URL+"/plugins/lab/echo-upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Got-Upgrade"); got != "websocket" {
		t.Fatalf("Upgrade forwarded: got %q", got)
	}
	// Connection may be canonicalized; require Upgrade token present.
	if got := resp.Header.Get("X-Got-Connection"); !strings.Contains(strings.ToLower(got), "upgrade") {
		t.Fatalf("Connection forwarded: got %q", got)
	}
}

func TestSplitPluginPath(t *testing.T) {
	cases := []struct {
		in       string
		name     string
		rest     string
		ok       bool
	}{
		{"/plugins/lab", "lab", "/", true},
		{"/plugins/lab/", "lab", "/", true},
		{"/plugins/lab/manifest", "lab", "/manifest", true},
		{"/plugins/lab/a/b", "lab", "/a/b", true},
		{"/plugins/", "", "", false},
		{"/plugins", "", "", false},
		{"/other/lab", "", "", false},
		{"/plugins//x", "", "", false},
	}
	for _, tc := range cases {
		name, rest, ok := splitPluginPath(tc.in)
		if ok != tc.ok || name != tc.name || rest != tc.rest {
			t.Errorf("splitPluginPath(%q)=(%q,%q,%v) want (%q,%q,%v)",
				tc.in, name, rest, ok, tc.name, tc.rest, tc.ok)
		}
	}
}
