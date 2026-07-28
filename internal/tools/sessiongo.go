// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"github.com/marcelocantos/mnemo/internal/sessiongo"
)

// sessionGo reopens a past conversation (🎯T125). The behaviour lives in
// internal/sessiongo, shared with the daemon's /api/session/go endpoint —
// which is what `mnemo resume` drives — so the MCP and CLI routes cannot
// drift apart in what they check or what they say when it fails.
func (h *callHandler) sessionGo(args map[string]any) (string, bool, error) {
	ref, _ := args["session"].(string)

	res, err := sessiongo.Open(h.ctx, h.mem, ref)
	if err != nil {
		return err.Error(), true, nil
	}
	return jsonResult(map[string]any{
		"action":     res.Action,
		"path":       res.Path,
		"session_id": res.SessionID,
		"repo":       res.Repo,
		"topic":      res.Topic,
		"last_msg":   res.LastMsg,
		"command":    res.Command,
	})
}
