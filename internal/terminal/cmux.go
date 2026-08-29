// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/iterm"
)

// EnvTagKey is the per-workspace environment variable used for
// focus-or-spawn matching. It is omitted from `cmux workspace list` (by
// design — env may hold secrets), so find walks `workspace env` instead.
const EnvTagKey = "MNEMO_TAG"

// cmuxCLI is the executable invoked for every cmux control call.
// Overridable in tests.
var cmuxCLI = resolveCmuxCLI

// runCmux executes `cmux <args…>` and returns trimmed stdout.
// Overridable in tests so argv builders can be exercised without a live app.
var runCmux = defaultRunCmux

// cmuxWaitDelay bounds how long Run waits on pipes after context cancel.
const cmuxWaitDelay = 3 * time.Second

type cmuxBackend struct{}

func (cmuxBackend) Go(ctx context.Context, args GoArgs) (Result, error) {
	bin, err := cmuxCLI()
	if err != nil {
		return Result{}, err
	}

	tag := tagKey(args)
	if !args.NoResume {
		if hit, err := findTaggedWorkspace(ctx, bin, tag); err != nil {
			return Result{}, err
		} else if hit != nil {
			if err := focusWorkspace(ctx, bin, hit.windowID, hit.workspaceRef); err != nil {
				return Result{}, err
			}
			return Result{Action: Focused, Path: args.Path}, nil
		}
	}

	if err := spawnWorkspace(ctx, bin, args, tag); err != nil {
		return Result{}, err
	}
	return Result{Action: Spawned, Path: args.Path}, nil
}

type workspaceHit struct {
	windowID     string
	workspaceRef string
}

func findTaggedWorkspace(ctx context.Context, bin, tag string) (*workspaceHit, error) {
	out, err := runCmux(ctx, bin, "list-windows", "--json")
	if err != nil {
		return nil, cmuxUnavailable(err)
	}
	var windows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &windows); err != nil {
		return nil, fmt.Errorf("cmux list-windows: parse: %w", err)
	}
	for _, w := range windows {
		if w.ID == "" {
			continue
		}
		hit, err := findInWindow(ctx, bin, w.ID, tag)
		if err != nil {
			return nil, err
		}
		if hit != nil {
			return hit, nil
		}
	}
	return nil, nil
}

func findInWindow(ctx context.Context, bin, windowID, tag string) (*workspaceHit, error) {
	out, err := runCmux(ctx, bin, "workspace", "list", "--window", windowID, "--json")
	if err != nil {
		return nil, fmt.Errorf("cmux workspace list: %w", err)
	}
	var listed struct {
		Workspaces []struct {
			ID  string `json:"id"`
			Ref string `json:"ref"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		return nil, fmt.Errorf("cmux workspace list: parse: %w", err)
	}
	for _, ws := range listed.Workspaces {
		handle := ws.Ref
		if handle == "" {
			handle = ws.ID
		}
		if handle == "" {
			continue
		}
		envOut, err := runCmux(ctx, bin, "workspace", "env", handle, "--window", windowID, "--json")
		if err != nil {
			// A single workspace env failure should not abort the scan —
			// the workspace may have been closed between list and env.
			continue
		}
		var envResp struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(envOut), &envResp); err != nil {
			continue
		}
		if envResp.Env[EnvTagKey] == tag {
			return &workspaceHit{windowID: windowID, workspaceRef: handle}, nil
		}
	}
	return nil, nil
}

func focusWorkspace(ctx context.Context, bin, windowID, workspaceRef string) error {
	if _, err := runCmux(ctx, bin, "select-workspace", "--workspace", workspaceRef, "--window", windowID); err != nil {
		return fmt.Errorf("cmux select-workspace: %w", err)
	}
	if _, err := runCmux(ctx, bin, "focus-window", "--window", windowID); err != nil {
		return fmt.Errorf("cmux focus-window: %w", err)
	}
	return nil
}

func spawnWorkspace(ctx context.Context, bin string, args GoArgs, tag string) error {
	login := iterm.LoginCommand(iterm.GoArgs{
		Path:     args.Path,
		Name:     args.Name,
		NoResume: args.NoResume,
		Command:  args.Command,
		TagKey:   args.TagKey,
	})
	layout, err := json.Marshal(map[string]any{
		"pane": map[string]any{
			"surfaces": []map[string]any{
				{"type": "terminal", "command": login},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("cmux layout: %w", err)
	}

	argv := []string{
		"workspace", "create",
		"--cwd", args.Path,
		"--name", args.Name,
		"--focus", "true",
		"--layout", string(layout),
	}
	if !args.NoResume {
		argv = append(argv, "--env", EnvTagKey+"="+tag)
	}
	if _, err := runCmux(ctx, bin, argv...); err != nil {
		return fmt.Errorf("cmux workspace create: %w", err)
	}
	return nil
}

func resolveCmuxCLI() (string, error) {
	if p, err := exec.LookPath("cmux"); err == nil {
		return p, nil
	}
	// App-bundle binary when the cask is installed but PATH is not set
	// (daemon launched from launchd often has a thin PATH).
	candidate := "/Applications/cmux.app/Contents/Resources/bin/cmux"
	if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
		return candidate, nil
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		alt := filepath.Join(home, "Applications", "cmux.app", "Contents", "Resources", "bin", "cmux")
		if fi, err := os.Stat(alt); err == nil && !fi.IsDir() {
			return alt, nil
		}
	}
	return "", fmt.Errorf(
		"terminal.backend is \"cmux\" but the cmux CLI was not found — install cmux (https://www.cmux.dev/), ensure it is on PATH, or set terminal.backend to \"iterm2\"")
}

func defaultRunCmux(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = cmuxWaitDelay
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
	return strings.TrimSpace(stdout.String()), nil
}

func cmuxUnavailable(err error) error {
	return fmt.Errorf(
		"terminal.backend is \"cmux\" but cmux is not reachable (%v) — start the cmux app, or set terminal.backend to \"iterm2\" in ~/.mnemo/config.json",
		err)
}
