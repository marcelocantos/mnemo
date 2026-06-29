// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"path/filepath"
	"time"

	"github.com/marcelocantos/mnemo/internal/iterm"
	"github.com/marcelocantos/mnemo/internal/render"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/threads"
)

// registerThreadRoutes attaches the /api/thread/* endpoints. These back the
// menu-bar shim (Integration §0.4) and read the threads root from live config
// each call, so a threads_root change via mnemo_config takes effect with no
// restart. Reads are GET; mutations are POST.
func (h *Handler) registerThreadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/thread/list", getOnly(h.threadList))
	mux.HandleFunc("/api/thread/show", getOnly(h.threadShow))
	mux.HandleFunc("/api/thread/preview", getOnly(h.threadPreview))
	mux.HandleFunc("/api/thread/search", getOnly(h.threadSearch))
	mux.HandleFunc("/api/thread/new", postOnly(h.threadNew))
	mux.HandleFunc("/api/thread/archive", postOnly(h.threadArchive))
	mux.HandleFunc("/api/thread/go", postOnly(h.threadGo))
	mux.HandleFunc("/api/thread/set_marker", postOnly(h.threadSetMarker))
	mux.HandleFunc("/api/thread/markers", getOnly(h.threadMarkers))
	mux.HandleFunc("/api/thread/config", h.threadConfig)
}

// threadMarkers serves GET /api/thread/markers — the marker catalog (value,
// emoji, label, pinned). A UI builds its marker menu from this rather than
// hardcoding the vocabulary, so adding a marker daemon-side (e.g. a third tag)
// propagates to every head with no client change. The catalog is static, so no
// manager lookup is needed.
func (h *Handler) threadMarkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"markers": threads.MarkerCatalog()})
}

// threadConfig reads (GET) or sets (POST) the threads root folder. The shim's
// settings gear uses this to let the user choose where threads live, rather
// than assuming a default parent. The threads root is read from live config on
// every thread call, so the change takes effect with no restart.
func (h *Handler) threadConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := store.LoadConfig()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{
			"threads_root": cfg.ResolvedThreadsRoot(),
			"default":      store.DefaultThreadsRoot(),
		})
	case http.MethodPost:
		root := r.URL.Query().Get("root")
		if root == "" {
			root = r.FormValue("root")
		}
		if root == "" {
			http.Error(w, "root required", http.StatusBadRequest)
			return
		}
		cfg, err := store.LoadConfig()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cfg.ThreadsRoot = root
		if err := store.WriteConfig(cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"threads_root": cfg.ResolvedThreadsRoot()})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// postOnly rejects non-POST requests with 405, mirroring getOnly for the
// mutating thread endpoints.
func postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// threadManager builds a threads.Manager from the live config. Defined in the
// api package (rather than shared with tools) to keep the two adapter
// packages independent; both delegate to the same threads core.
func threadManager() (*threads.Manager, error) {
	home, err := store.EffectiveHome()
	if err != nil {
		return nil, err
	}
	cfg, err := store.LoadConfig()
	if err != nil {
		return nil, err
	}
	return &threads.Manager{Root: cfg.ResolvedThreadsRoot(), Home: home}, nil
}

func (h *Handler) threadList(w http.ResponseWriter, r *http.Request) {
	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ts, err := m.ListSorted()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"root":    m.Root,
		"count":   len(ts),
		"threads": threads.Views(ts, time.Now()),
	})
}

func (h *Handler) threadShow(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := m.Get(name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	body, _ := m.ReadContext(name)
	files, err := m.Files(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"name":             name,
		"path":             m.Root + "/" + name,
		"context_markdown": body,
		"files":            files,
	})
}

// threadPreview serves GET /api/thread/preview?name=&theme=light|dark — the
// self-contained, inline-styled HTML the menu-bar shim feeds to
// NSAttributedString(html:) (Integration §0.8). A synthetic H1 (the thread's
// directory name) is prepended at render time. Returns text/html, not JSON.
func (h *Handler) threadPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	theme := render.ThemeDark
	if q.Get("theme") == "light" {
		theme = render.ThemeLight
	}
	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := m.Get(name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	body, _ := m.ReadContext(name)
	html, err := render.HTML(body, render.Options{
		Theme:       theme,
		SyntheticH1: name,
		Standalone:  true,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// threadSearch serves GET /api/thread/search?q= — the deep channel: thread
// names whose content matches q (Integration §0.5 reuse; here a live grep).
func (h *Handler) threadSearch(w http.ResponseWriter, r *http.Request) {
	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := m.Search(r.URL.Query().Get("q"))
	if names == nil {
		names = []string{}
	}
	writeJSON(w, map[string]any{"names": names})
}

func (h *Handler) threadNew(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.FormValue("name")
	}
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	t, err := m.New(threads.NewArgs{Name: name})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, t.View(time.Now()))
}

// threadGo serves POST /api/thread/go?name=&no_resume= — the daemon-owned
// iTerm2 entry point (Integration §0.3). It resolves a bare name or path to an
// absolute thread directory and focuses-or-spawns its tagged tab. The daemon
// holds the single Automation TCC grant; the CLI and shim POST here.
func (h *Handler) threadGo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref := q.Get("name")
	if ref == "" {
		ref = r.FormValue("name")
	}
	if ref == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	noResume := q.Get("no_resume") == "1" || q.Get("no_resume") == "true"

	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	path, err := m.Resolve(ref)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := iterm.Go(r.Context(), iterm.GoArgs{
		Path:     path,
		Name:     filepath.Base(path),
		NoResume: noResume,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}

// threadSetMarker serves POST /api/thread/set_marker?name=&marker= — set a
// thread's marker ("" or "normal" clears it; "important" pins it). Returns the
// updated thread view.
func (h *Handler) threadSetMarker(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		name = r.FormValue("name")
	}
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	markerArg := q.Get("marker")
	if markerArg == "" {
		markerArg = r.FormValue("marker")
	}
	marker := threads.ParseMarker(markerArg)
	// "normal" is an accepted alias for the default; ParseMarker maps any
	// unknown value (including "normal") to MarkerNormal already.

	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := m.SetMarker(name, marker); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t, err := m.Get(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t.View(time.Now()))
}

func (h *Handler) threadArchive(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.FormValue("name")
	}
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	m, err := threadManager()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := m.Archive(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"archived": name})
}
