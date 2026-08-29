// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package sessiongo reopens a past conversation: it resolves a loose
// reference to one session, then opens a terminal in the directory that
// session ran in and resumes it there (🎯T125).
//
// It exists as its own package because three callers need identical
// behaviour — the MCP tool, the daemon's HTTP endpoint, and (through that
// endpoint) the `mnemo resume` CLI. The checks below are policy, not
// plumbing: which failures are worth naming, and what a caller is told when
// a conversation cannot be reopened. Duplicating them per caller is how the
// three drift apart.
package sessiongo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/terminal"
)

// Result describes the tab that was focused or spawned, plus enough about
// the session for a caller to confirm it got the conversation it meant.
type Result struct {
	Action    string `json:"action"`
	Path      string `json:"path"`
	SessionID string `json:"session_id"`
	Repo      string `json:"repo,omitempty"`
	Topic     string `json:"topic,omitempty"`
	LastMsg   string `json:"last_msg,omitempty"`
	Command   string `json:"command"`
}

// UserError is a failure the caller can do something about: an unresolvable
// reference, a session whose directory is gone, a source with no known
// resume command. Callers map it to a 4xx or a plain message; anything else
// is an internal fault and maps to a 5xx.
type UserError struct{ msg string }

func (e *UserError) Error() string { return e.msg }

func userErrorf(format string, a ...any) *UserError {
	return &UserError{msg: fmt.Sprintf(format, a...)}
}

// Open resolves ref to exactly one session and reopens it in its own
// working directory.
//
// The directory matters as much as the resume. A conversation is about a
// working tree, and reopening it somewhere else — the daemon's cwd, or
// wherever the caller happens to be — gives the agent a context that
// silently contradicts everything in its own transcript.
func Open(ctx context.Context, mem store.Backend, ref string) (Result, error) {
	hit, err := mem.ResolveSessionRef(ref)
	if err != nil {
		return Result{}, &UserError{msg: err.Error()}
	}

	if hit.CWD == "" {
		return Result{}, userErrorf(
			"session %s has no recorded working directory, so there is nowhere to reopen it",
			hit.SessionID)
	}
	// A session's directory outlives neither a deleted repo nor a temp
	// checkout. Say which, rather than opening a terminal somewhere
	// arbitrary and letting the agent discover the mismatch itself.
	if fi, statErr := os.Stat(hit.CWD); statErr != nil || !fi.IsDir() {
		return Result{}, userErrorf(
			"session %s ran in %s, which no longer exists — the checkout was moved, deleted, "+
				"or was a temporary directory", hit.SessionID, hit.CWD)
	}

	cmd, err := hit.ResumeCommand()
	if err != nil {
		return Result{}, &UserError{msg: err.Error()}
	}

	res, err := terminal.Go(ctx, terminal.GoArgs{
		Path: hit.CWD,
		Name: filepath.Base(hit.CWD),
		// Tag by session, not by path: several past conversations can share
		// one working directory, and each should get its own tab rather
		// than stealing whichever sibling was opened first.
		TagKey:  "session:" + hit.SessionID,
		Command: cmd,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		Action:    string(res.Action),
		Path:      res.Path,
		SessionID: hit.SessionID,
		Repo:      hit.Repo,
		Topic:     hit.Topic,
		LastMsg:   hit.LastMsg,
		Command:   cmd,
	}, nil
}
