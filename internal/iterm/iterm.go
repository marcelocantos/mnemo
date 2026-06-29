// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package iterm drives iTerm2 for the Threads feature's `go` verb (🎯T85.2,
// Integration §0.2–0.3): focus the tab tagged for a thread, or spawn and tag
// a new one.
//
// Transport. The original proposal specified iTerm2's protobuf Python API
// over its Unix-domain socket. This implementation instead drives iTerm2 via
// AppleScript (osascript), which iTerm2 3.3+ supports for exactly the
// operations `go` needs: get/set a session variable (`variable named` /
// `set variable named`), create a tab/window with a named profile, send text,
// and select a session/tab/window. AppleScript is dramatically less code than
// rebuilding the protobuf+WebSocket+cookie stack, uses the same Automation
// TCC grant the daemon already needs, and — crucially for the minimal-
// installation-friction requirement — does NOT require the user to enable
// iTerm2's Python API. The daemon owns the single Automation identity; the
// CLI and shim delegate here over HTTP (§0.3).
package iterm

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Profile is the iTerm2 profile used for thread tabs (hardcoded per the
// design). Creation falls back to the default profile when it is absent.
const Profile = "Thread"

// ThreadVar is the iTerm2 session variable that tags a tab with its thread.
// Its value is base64 of the thread's absolute path.
const ThreadVar = "user.thread"

// Badge is the emoji prefix shown on a tagged tab's badge.
const Badge = "🪡"

// Action reports what Go did.
type Action string

const (
	// Focused means an existing tagged tab was found and selected.
	Focused Action = "focused"
	// Spawned means a new tab/window was created (and tagged unless NoResume).
	Spawned Action = "spawned"
)

// GoArgs parameterises the `go` operation.
type GoArgs struct {
	// Path is the thread's absolute directory path.
	Path string
	// Name is the display name used for the tab badge.
	Name string
	// NoResume forces a fresh, untagged tab (plain `claude`, no user.thread),
	// deliberately ephemeral so a later Go cannot re-match it.
	NoResume bool
}

// Result is the outcome of Go.
type Result struct {
	Action Action `json:"action"`
	Path   string `json:"path"`
}

// runner executes an AppleScript and returns its trimmed stdout. Overridable
// in tests so the script builders can be exercised without a live iTerm2.
var runner = runOsascript

// Go focuses the tab tagged for args.Path, or spawns and tags a new one. With
// NoResume it always spawns a fresh untagged tab.
func Go(ctx context.Context, args GoArgs) (Result, error) {
	if strings.TrimSpace(args.Path) == "" {
		return Result{}, fmt.Errorf("empty thread path")
	}
	tag := base64.StdEncoding.EncodeToString([]byte(args.Path))

	if !args.NoResume {
		out, err := runner(ctx, findScript(tag))
		if err != nil {
			return Result{}, fmt.Errorf("iterm find: %w", err)
		}
		if strings.TrimSpace(out) == "found" {
			return Result{Action: Focused, Path: args.Path}, nil
		}
	}

	if _, err := runner(ctx, spawnScript(args, tag)); err != nil {
		return Result{}, fmt.Errorf("iterm spawn: %w", err)
	}
	return Result{Action: Spawned, Path: args.Path}, nil
}

// findScript walks every session in every window, comparing user.thread to
// tag; on a match it selects the session, its tab, and its window and returns
// "found", else "notfound".
func findScript(tag string) []string {
	return []string{
		`tell application "iTerm2"`,
		`	repeat with w in windows`,
		`		repeat with aTab in tabs of w`,
		`			repeat with aSession in sessions of aTab`,
		`				set v to ""`,
		`				try`,
		`					tell aSession to set v to (variable named "` + ThreadVar + `")`,
		`				end try`,
		`				if v is "` + tag + `" then`,
		`					select aSession`,
		`					select aTab`,
		`					select w`,
		`					activate`,
		`					return "found"`,
		`				end if`,
		`			end repeat`,
		`		end repeat`,
		`	end repeat`,
		`	return "notfound"`,
		`end tell`,
	}
}

// spawnScript creates a tab (or a window when none exist) with the Thread
// profile, running the thread's command as the session's actual process via
// the create command's `command` parameter — far more resilient than typing it
// into a started shell with `write text`, which races shell startup and runs
// inside the user's interactive rc — then tags it (unless NoResume).
func spawnScript(args GoArgs, tag string) []string {
	cmd := osaEscape(loginCommand(args))
	lines := []string{
		`tell application "iTerm2"`,
		`	activate`,
		`	set cmd to "` + cmd + `"`,
		`	if (count of windows) is 0 then`,
		`		try`,
		`			create window with profile "` + Profile + `" command cmd`,
		`		on error`,
		`			create window with default profile command cmd`,
		`		end try`,
		`	else`,
		`		tell current window`,
		`			try`,
		`				create tab with profile "` + Profile + `" command cmd`,
		`			on error`,
		`				create tab with default profile command cmd`,
		`			end try`,
		`		end tell`,
		`	end if`,
		`	set s to current session of current window`,
	}
	if !args.NoResume {
		lines = append(lines,
			`	tell s to set variable named "`+ThreadVar+`" to "`+tag+`"`)
	}
	lines = append(lines,
		`	select current tab of current window`,
		`	return "spawned"`,
		`end tell`,
	)
	return lines
}

// loginCommand wraps the per-thread script in a login shell so PATH is resolved
// (claude is found) without the interactive-init noise of -i. iTerm2's
// `command` parameter tokenizes respecting quotes but does NOT run the value
// through a shell, so an explicit `/bin/zsh -lc '<script>'` is required. The
// script is single-quoted, so it must contain no single quotes — the path and
// printf format in spawnCommand are double-quoted for exactly that reason.
func loginCommand(args GoArgs) string {
	return `/bin/zsh -lc '` + spawnCommand(args) + `'`
}

// spawnCommand is the shell script run by the login shell in the new session:
// cd into the thread, set the tab badge (tagged spawns only), then run claude
// (resuming if possible). It uses ONLY double quotes — no single quotes — so
// loginCommand can wrap the whole thing in single quotes for iTerm2's
// `command` tokenizer. The badge text is base64-encoded here in Go, so the
// script carries no shell base64/tr pipeline to mis-escape. `--no-resume` runs
// plain claude.
func spawnCommand(args GoArgs) string {
	cd := "cd " + shellDoubleQuote(args.Path)
	if args.NoResume {
		return cd + " && claude"
	}
	badgeB64 := base64.StdEncoding.EncodeToString([]byte(Badge + " " + args.Name))
	// SetBadgeFormat takes a base64 value; \033 (ESC) and \007 (BEL) bound the
	// OSC sequence and are interpreted by printf (the backslashes survive the
	// double quotes literally).
	setBadge := `printf "\033]1337;SetBadgeFormat=%s\007" ` + badgeB64
	return cd + " && " + setBadge + "; claude --continue 2>/dev/null || claude"
}

// shellDoubleQuote wraps s in double quotes, escaping the characters the shell
// still treats specially inside double quotes (\ " $ `). Used for the thread
// path, which sits inside the single-quoted loginCommand script.
func shellDoubleQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return `"` + r.Replace(s) + `"`
}

// osaEscape escapes a string for embedding inside an AppleScript double-quoted
// string literal: backslash and double-quote only.
func osaEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// runOsascript runs a multi-line AppleScript by passing each line as a
// separate -e argument (robust against quoting in a single -e blob).
//
// This is the package's single point of OS contact, and the only place the
// macOS-only nature of the `go` verb is enforced at runtime: on a non-darwin
// host it returns a clear "macOS-only" error rather than letting exec fail
// with a cryptic "osascript: executable file not found". Tests exercise Go()
// via an overridden runner, so they never reach this guard. (The menu-bar app
// is excluded separately, in superviseThreadsShim.)
func runOsascript(ctx context.Context, lines []string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("iTerm2 control (the thread 'go' verb) is only available on macOS; this host is %s", runtime.GOOS)
	}
	argv := make([]string, 0, len(lines)*2)
	for _, l := range lines {
		argv = append(argv, "-e", l)
	}
	cmd := exec.CommandContext(ctx, "osascript", argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}
