// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marcelocantos/mnemo/internal/plugin"
)

type stubPluginUI struct {
	items []plugin.UIContribution
}

func (s stubPluginUI) UIContributions() []plugin.UIContribution { return s.items }

func TestPluginsListEmpty(t *testing.T) {
	mux := http.NewServeMux()
	handler := &Handler{}
	handler.RegisterRoutes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/plugins", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var env struct {
		Count   int                     `json:"count"`
		Plugins []plugin.UIContribution `json:"plugins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Count != 0 || env.Plugins == nil {
		t.Fatalf("empty list: %+v", env)
	}
}

func TestPluginsListReturnsUI(t *testing.T) {
	mux := http.NewServeMux()
	handler := &Handler{}
	handler.SetPluginUILister(stubPluginUI{items: []plugin.UIContribution{{
		Name:       "lab",
		Label:      "Lab",
		Icon:       "flask",
		PreviewURL: "/plugins/lab/ui/preview",
		PageURL:    "/plugins/lab/ui/",
		Menu:       "footer",
	}}})
	handler.RegisterRoutes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/plugins", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var env struct {
		Count   int                     `json:"count"`
		Plugins []plugin.UIContribution `json:"plugins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Count != 1 || env.Plugins[0].Name != "lab" || env.Plugins[0].PreviewURL == "" {
		t.Fatalf("got %+v", env)
	}
}
