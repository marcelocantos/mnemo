// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// facetPlugin serves ready/manifest plus optional reconcile/check/notify.
func facetPlugin(t *testing.T, name string, facets Facets, handlers map[string]http.HandlerFunc) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.URL.Path]; ok {
			h(w, r)
			return
		}
		switch r.URL.Path {
		case "/ready":
			_ = json.NewEncoder(w).Encode(ReadyBody{OK: true})
		case "/manifest":
			_ = json.NewEncoder(w).Encode(Manifest{
				ProtocolVersion: ProtocolVersionCurrent,
				Name:            name,
				Version:         "0.2.0",
				Facets:          facets,
			})
		default:
			http.NotFound(w, r)
		}
	})
}

func TestStreamReconcilersPOST(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Reconcile: true}, map[string]http.HandlerFunc{
		"/reconcile": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method: %s", r.Method)
			}
			var body ReconcileBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Now == "" {
				t.Error("expected now in reconcile body")
			}
			got.Add(1)
			_ = json.NewEncoder(w).Encode(ReconcileResult{Changed: 3})
		},
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	ctx := context.Background()
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})

	srs := m.StreamReconcilers()
	if len(srs) != 1 {
		t.Fatalf("reconcilers: got %d want 1", len(srs))
	}
	if srs[0].Name() != "plugin.lab.reconcile" {
		t.Fatalf("name: %s", srs[0].Name())
	}
	if srs[0].Interval() != DefaultReconcileInterval {
		t.Fatalf("interval: %v", srs[0].Interval())
	}
	n, err := srs[0].Reconcile(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("changed: got %d want 3", n)
	}
	if got.Load() != 1 {
		t.Fatalf("POST count: %d", got.Load())
	}

	// No reconcile facet → empty list.
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	// Same server still has Reconcile true in manifest — list stays non-empty.
	// Disable to clear.
	m.Reconcile(ctx, nil)
	if len(m.StreamReconcilers()) != 0 {
		t.Fatal("expected no reconcilers after remove")
	}
}

func TestStreamReconcilersSkipsWithoutFacet(t *testing.T) {
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Check: true}, nil))
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	if len(m.StreamReconcilers()) != 0 {
		t.Fatal("reconcile facet off → no StreamReconcilers")
	}
	if len(m.FacetChecks()) != 1 || m.FacetChecks()[0].Name != "plugin.lab.check" {
		t.Fatalf("check facet: %+v", m.FacetChecks())
	}
}

func TestCheckFacetMapsSeverity(t *testing.T) {
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Check: true}, map[string]http.HandlerFunc{
		"/check": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method: %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(CheckBody{
				Severity:    "warn",
				Detail:      "stale signal",
				Remediation: "touch the watched file",
			})
		},
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	ctx := context.Background()
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})

	checks := m.FacetChecks()
	if len(checks) != 1 {
		t.Fatalf("checks: %d", len(checks))
	}
	res := checks[0].Run(ctx)
	if res.Severity.String() != "warn" {
		t.Fatalf("severity: %+v", res)
	}
	if res.Detail != "stale signal" || res.Remediation != "touch the watched file" {
		t.Fatalf("body map: %+v", res)
	}

	// DynamicChecks includes ready + check.
	dyn := m.DynamicChecks()
	names := map[string]bool{}
	for _, c := range dyn {
		names[c.Name] = true
	}
	if !names["plugin.lab.ready"] || !names["plugin.lab.check"] {
		t.Fatalf("dynamic: %v", names)
	}
}

func TestCheckFacetHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Check: true}, map[string]http.HandlerFunc{
		"/check": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	}))
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	ctx := context.Background()
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	res := m.FacetChecks()[0].Run(ctx)
	if res.Severity.String() != "fail" {
		t.Fatalf("want fail: %+v", res)
	}
}

func TestNotifyPOST(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Notify: true}, map[string]http.HandlerFunc{
		"/notify": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method: %s", r.Method)
			}
			var p NotifyPayload
			_ = json.NewDecoder(r.Body).Decode(&p)
			got.Store(p)
			w.WriteHeader(http.StatusNoContent)
		},
	}))
	t.Cleanup(srv.Close)

	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	ctx := context.Background()
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})

	payload := NotifyPayload{Title: "mnemo", Body: "check failed", URL: "http://localhost/#health"}
	if err := m.Notify(ctx, "lab", payload); err != nil {
		t.Fatal(err)
	}
	p, _ := got.Load().(NotifyPayload)
	if p.Title != payload.Title || p.Body != payload.Body || p.URL != payload.URL {
		t.Fatalf("payload: %+v", p)
	}

	if err := m.NotifyAll(ctx, payload); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyRejectsWithoutFacet(t *testing.T) {
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Check: true}, nil))
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	if err := m.Notify(context.Background(), "lab", NotifyPayload{Title: "x", Body: "y"}); err == nil {
		t.Fatal("expected error when notify facet absent")
	}
}

func TestReconcileTimeoutIsolation(t *testing.T) {
	// Hung /reconcile must not block past the adapter timeout.
	// release unblocks the handler so httptest.Close does not wait on it.
	release := make(chan struct{})
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Reconcile: true}, map[string]http.HandlerFunc{
		"/reconcile": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		},
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	// Client without global Timeout so only the adapter context deadline applies.
	client := &http.Client{}
	adapter := &ReconcileAdapter{
		name:    "lab",
		baseURL: srv.URL,
		client:  client,
		timeout: 200 * time.Millisecond,
	}
	start := time.Now()
	_, err := adapter.Reconcile(context.Background(), time.Now())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout isolation failed: took %v (hung plugin wedged caller)", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("returned too fast (%v); expected ~200ms timeout", elapsed)
	}
}

func TestCheckFacetTimeoutIsolation(t *testing.T) {
	// Hang /check; short client Timeout proves isolation without waiting FacetHTTPTimeout.
	release := make(chan struct{})
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Check: true}, map[string]http.HandlerFunc{
		"/check": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		},
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	// Attach with default client, then run check via short-timeout client.
	m := NewManager(t.TempDir(), DefaultHTTPClient(), testLogger())
	t.Cleanup(m.Close)
	ctx := context.Background()
	m.Reconcile(ctx, []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	m.mu.Lock()
	m.client = &http.Client{Timeout: 200 * time.Millisecond}
	m.mu.Unlock()

	start := time.Now()
	res := m.FacetChecks()[0].Run(context.Background())
	elapsed := time.Since(start)
	if res.Severity.String() != "fail" {
		t.Fatalf("want fail on timeout: %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("check timeout isolation failed: took %v", elapsed)
	}
}

func TestReconcileHTTPError(t *testing.T) {
	srv := httptest.NewServer(facetPlugin(t, "lab", Facets{Reconcile: true}, map[string]http.HandlerFunc{
		"/reconcile": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusBadGateway)
		},
	}))
	t.Cleanup(srv.Close)
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	_, err := m.StreamReconcilers()[0].Reconcile(context.Background(), time.Now())
	if err == nil {
		t.Fatal("expected HTTP error")
	}
}
