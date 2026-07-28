// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/marcelocantos/mnemo/internal/sessiongo"
)

// registerSessionRoutes wires the session-reopen endpoint (🎯T125).
func (h *Handler) registerSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/session/go", postOnly(h.sessionGo))
}

// sessionGo serves POST /api/session/go?session= — the daemon-owned entry
// point for reopening a past conversation, and the twin of
// /api/thread/go.
//
// The CLI posts here rather than driving iTerm2 itself for the same reason
// `mnemo thread go` does: the daemon holds the single Automation TCC grant.
// A CLI that opened tabs directly would carry the invoking terminal's TCC
// identity and prompt for its own separate grant.
func (h *Handler) sessionGo(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("session")
	if ref == "" {
		ref = r.FormValue("session")
	}

	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// An empty ref is legitimate — it means "the most recent session" — so
	// it is passed through rather than rejected.
	res, err := sessiongo.Open(r.Context(), mem, ref)
	if err != nil {
		var ue *sessiongo.UserError
		if errors.As(err, &ue) {
			http.Error(w, ue.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}
