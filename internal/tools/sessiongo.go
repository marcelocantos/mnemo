// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/marcelocantos/mnemo/internal/iterm"
)

// sessionGo reopens a past conversation: resolve a loose reference to one
// session, then open a terminal in the directory that session ran in and
// resume it there (🎯T125).
//
// The directory matters as much as the resume. A conversation is about a
// working tree, and reopening it somewhere else — the daemon's cwd, or
// wherever the caller happens to be — gives the agent a context that
// silently contradicts everything in its own transcript.
func (h *callHandler) sessionGo(args map[string]any) (string, bool, error) {
	ref, _ := args["session"].(string)

	hit, err := h.mem.ResolveSessionRef(ref)
	if err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}

	if hit.CWD == "" {
		return fmt.Sprintf(
			"session %s has no recorded working directory, so there is nowhere to reopen it",
			hit.SessionID), true, nil
	}
	// A session's directory can outlive neither a deleted repo nor a temp
	// checkout. Say which, rather than opening a terminal somewhere
	// arbitrary and letting the agent discover the mismatch itself.
	if fi, statErr := os.Stat(hit.CWD); statErr != nil || !fi.IsDir() {
		return fmt.Sprintf(
			"session %s ran in %s, which no longer exists — the checkout was moved, deleted, "+
				"or was a temporary directory", hit.SessionID, hit.CWD), true, nil
	}

	cmd, err := hit.ResumeCommand()
	if err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}

	res, err := iterm.Go(h.ctx, iterm.GoArgs{
		Path: hit.CWD,
		Name: filepath.Base(hit.CWD),
		// Tag by session, not by path: several past conversations can share
		// one working directory, and each should get its own tab rather
		// than stealing whichever sibling was opened first.
		TagKey:  "session:" + hit.SessionID,
		Command: cmd,
	})
	if err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}
	return jsonResult(map[string]any{
		"action":     res.Action,
		"path":       res.Path,
		"session_id": hit.SessionID,
		"repo":       hit.Repo,
		"topic":      hit.Topic,
		"last_msg":   hit.LastMsg,
		"command":    cmd,
	})
}
