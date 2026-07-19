// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/marcelocantos/mnemo/internal/plugin"
)

// PluginUILister is the subset of *plugin.Manager used by GET /api/plugins
// (🎯T102.9). Defined as an interface so tests can stub it.
type PluginUILister interface {
	UIContributions() []plugin.UIContribution
}

// SetPluginUILister wires the plugin manager into the REST handler.
// Call once during startup; without it GET /api/plugins returns an empty list.
func (h *Handler) SetPluginUILister(l PluginUILister) { h.plugins = l }

// registerPluginRoutes attaches GET /api/plugins.
func (h *Handler) registerPluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/plugins", getOnly(h.pluginsList))
}

// pluginsList serves GET /api/plugins — data-driven UI contributions for
// the menu-bar popup. The Swift shim builds footer rows (and live
// WKWebView preview URLs) from this list; nothing is hardcoded.
func (h *Handler) pluginsList(w http.ResponseWriter, r *http.Request) {
	var items []plugin.UIContribution
	if h.plugins != nil {
		items = h.plugins.UIContributions()
	}
	if items == nil {
		items = []plugin.UIContribution{}
	}
	writeJSON(w, map[string]any{
		"count":   len(items),
		"plugins": items,
	})
}
