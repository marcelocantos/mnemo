// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package terminal opens a thread or session in the user's preferred
// terminal (🎯T126). Call sites (thread go, session resume) go through
// Go rather than assuming iTerm2; the backend is selected from
// ~/.mnemo/config.json (`terminal.backend`).
package terminal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// Action reports what Go did.
type Action string

const (
	// Focused means an existing tagged tab/workspace was found and selected.
	Focused Action = "focused"
	// Spawned means a new tab/workspace was created.
	Spawned Action = "spawned"
)

// GoArgs parameterises the focus-or-spawn operation. Shared by every
// backend so call sites stay backend-agnostic.
type GoArgs struct {
	// Path is the absolute directory to open in.
	Path string
	// Name is the display name used for badges / workspace titles.
	Name string
	// NoResume forces a fresh, untagged tab/workspace (plain `claude`),
	// deliberately ephemeral so a later Go cannot re-match it.
	NoResume bool

	// Command replaces the default `claude --continue || claude` run in
	// the new session — e.g. `claude --resume <id>`. Empty keeps the
	// thread behaviour. Must contain no single quotes (shell wrapping).
	Command string

	// TagKey overrides the identity used for focus-or-spawn matching.
	// Empty means "tag by Path" (threads). Sessions pass "session:<id>"
	// so siblings sharing a cwd each get their own tab.
	TagKey string
}

// Result is the outcome of Go.
type Result struct {
	Action Action `json:"action"`
	Path   string `json:"path"`
}

// Backend drives one terminal application.
type Backend interface {
	Go(ctx context.Context, args GoArgs) (Result, error)
}

// loadConfig is overridable in tests so dispatch can be exercised
// without touching ~/.mnemo/config.json.
var loadConfig = store.LoadConfig

// lookupBackend resolves a backend name to an implementation. Overridable
// in tests.
var lookupBackend = defaultLookupBackend

// automationTimeout bounds a whole terminal interaction when the caller
// supplied no deadline. Same posture as internal/iterm.
const automationTimeout = 20 * time.Second

// Go focuses the tagged tab/workspace for args, or spawns a new one,
// using the terminal backend named in config (default: iterm2).
func Go(ctx context.Context, args GoArgs) (Result, error) {
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, fmt.Errorf("empty thread path")
	}
	if strings.Contains(args.Command, "'") {
		return Result{}, fmt.Errorf("command may not contain a single quote: %q", args.Command)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, automationTimeout)
		defer cancel()
	}

	cfg, err := loadConfig()
	if err != nil {
		return Result{}, fmt.Errorf("read terminal config: %w", err)
	}
	name := cfg.Terminal.EffectiveBackend()
	b, err := lookupBackend(name)
	if err != nil {
		return Result{}, err
	}
	return b.Go(ctx, args)
}

func defaultLookupBackend(name string) (Backend, error) {
	switch name {
	case store.TerminalBackendITerm2:
		return iterm2Backend{}, nil
	case store.TerminalBackendCmux:
		return cmuxBackend{}, nil
	default:
		return nil, fmt.Errorf(
			"unsupported terminal.backend %q — configure \"iterm2\" or \"cmux\" in ~/.mnemo/config.json",
			name)
	}
}

// tagKey returns the focus-or-spawn identity for args.
func tagKey(args GoArgs) string {
	if args.TagKey != "" {
		return args.TagKey
	}
	return args.Path
}
