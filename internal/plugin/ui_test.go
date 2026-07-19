// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestUIContributionsAndReloadEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			_ = json.NewEncoder(w).Encode(ReadyBody{OK: true})
		case "/manifest":
			_ = json.NewEncoder(w).Encode(Manifest{
				ProtocolVersion: ProtocolVersionCurrent,
				Name:            "lab",
				Version:         "1.0.0",
				Description:     "test",
				UI: &UISurface{
					Label:       "Lab UI",
					Icon:        "flask",
					PreviewPath: "ui/preview",
					PagePath:    "ui/",
					Menu:        "footer",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var events []string
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	m.SetEventPublisher(func(typ string, data any) {
		mu.Lock()
		events = append(events, typ)
		mu.Unlock()
	})

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})

	contribs := m.UIContributions()
	if len(contribs) != 1 {
		t.Fatalf("contribs=%d", len(contribs))
	}
	c := contribs[0]
	if c.Label != "Lab UI" || c.PreviewURL != "/plugins/lab/ui/preview" || c.PageURL != "/plugins/lab/ui" {
		// path.Join collapses trailing slash on page — accept either
		if c.PageURL != "/plugins/lab/ui" && c.PageURL != "/plugins/lab/ui/" {
			t.Fatalf("contrib: %+v", c)
		}
	}
	if c.PreviewURL != "/plugins/lab/ui/preview" {
		t.Fatalf("preview_url: %q", c.PreviewURL)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, e := range events {
		if e == "plugin.reload" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected plugin.reload event, got %v", events)
	}
}

func TestPluginPublicURL(t *testing.T) {
	if got := pluginPublicURL("lab", "ui/preview"); got != "/plugins/lab/ui/preview" {
		t.Fatalf("got %q", got)
	}
	if got := pluginPublicURL("lab", ""); got != "/plugins/lab/" {
		t.Fatalf("root: %q", got)
	}
}
