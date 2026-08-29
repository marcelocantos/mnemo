// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/iterm"
)

func TestCmuxSpawnArgvIncludesEnvTagAndLayout(t *testing.T) {
	restoreCLI := cmuxCLI
	restoreRun := runCmux
	defer func() {
		cmuxCLI = restoreCLI
		runCmux = restoreRun
	}()

	cmuxCLI = func() (string, error) { return "cmux", nil }

	var calls [][]string
	runCmux = func(_ context.Context, _ string, args ...string) (string, error) {
		cp := append([]string{}, args...)
		calls = append(calls, cp)
		switch {
		case len(args) >= 1 && args[0] == "list-windows":
			return `[{"id":"WIN1"}]`, nil
		case len(args) >= 2 && args[0] == "workspace" && args[1] == "list":
			return `{"workspaces":[]}`, nil
		case len(args) >= 2 && args[0] == "workspace" && args[1] == "create":
			return `{"workspace_ref":"workspace:9"}`, nil
		default:
			return "", fmt.Errorf("unexpected cmux args: %v", args)
		}
	}

	res, err := cmuxBackend{}.Go(context.Background(), GoArgs{
		Path: "/Users/x/proj", Name: "proj", TagKey: "session:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Spawned {
		t.Errorf("action = %q, want spawned", res.Action)
	}

	var create []string
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "workspace" && c[1] == "create" {
			create = c
			break
		}
	}
	if create == nil {
		t.Fatalf("no workspace create call in %#v", calls)
	}

	joined := strings.Join(create, "\x00")
	if !strings.Contains(joined, EnvTagKey+"=session:abc") {
		t.Errorf("create missing MNEMO_TAG: %v", create)
	}
	if !strings.Contains(joined, "--cwd") || !strings.Contains(joined, "/Users/x/proj") {
		t.Errorf("create missing cwd: %v", create)
	}
	if !strings.Contains(joined, "--focus") {
		t.Errorf("create missing --focus: %v", create)
	}

	// Layout must carry the login command as a real surface process, not
	// the racey --command send-text path.
	layoutIdx := -1
	for i, a := range create {
		if a == "--layout" && i+1 < len(create) {
			layoutIdx = i + 1
			break
		}
	}
	if layoutIdx < 0 {
		t.Fatalf("no --layout in %v", create)
	}
	var layout map[string]any
	if err := json.Unmarshal([]byte(create[layoutIdx]), &layout); err != nil {
		t.Fatalf("layout JSON: %v\n%s", err, create[layoutIdx])
	}
	wantLogin := iterm.LoginCommand(iterm.GoArgs{
		Path: "/Users/x/proj", Name: "proj", TagKey: "session:abc",
	})
	pane := layout["pane"].(map[string]any)
	surfaces := pane["surfaces"].([]any)
	surf := surfaces[0].(map[string]any)
	if surf["command"] != wantLogin {
		t.Errorf("layout command = %q\nwant %q", surf["command"], wantLogin)
	}
	for _, a := range create {
		if a == "--command" {
			t.Error("must not use racey --command send-text; use --layout")
		}
	}
}

func TestCmuxNoResumeOmitsEnvTag(t *testing.T) {
	restoreCLI := cmuxCLI
	restoreRun := runCmux
	defer func() {
		cmuxCLI = restoreCLI
		runCmux = restoreRun
	}()
	cmuxCLI = func() (string, error) { return "cmux", nil }

	var create []string
	runCmux = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "workspace" && args[1] == "create" {
			create = append([]string{}, args...)
		}
		return `{}`, nil
	}

	_, err := (cmuxBackend{}).Go(context.Background(), GoArgs{
		Path: "/p", Name: "d", NoResume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range create {
		if strings.HasPrefix(a, EnvTagKey+"=") || a == "--env" {
			t.Errorf("NoResume must not set MNEMO_TAG: %v", create)
		}
	}
	// NoResume skips find entirely — only create should run.
	if create == nil {
		t.Fatal("expected create")
	}
}

func TestCmuxFocusesTaggedWorkspace(t *testing.T) {
	restoreCLI := cmuxCLI
	restoreRun := runCmux
	defer func() {
		cmuxCLI = restoreCLI
		runCmux = restoreRun
	}()
	cmuxCLI = func() (string, error) { return "cmux", nil }

	var calls [][]string
	runCmux = func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string{}, args...))
		switch {
		case args[0] == "list-windows":
			return `[{"id":"WIN1"}]`, nil
		case args[0] == "workspace" && args[1] == "list":
			return `{"workspaces":[{"id":"WS1","ref":"workspace:3"}]}`, nil
		case args[0] == "workspace" && args[1] == "env":
			return `{"env":{"MNEMO_TAG":"session:abc"}}`, nil
		case args[0] == "select-workspace":
			return ``, nil
		case args[0] == "focus-window":
			return ``, nil
		default:
			return "", fmt.Errorf("unexpected: %v", args)
		}
	}

	res, err := cmuxBackend{}.Go(context.Background(), GoArgs{
		Path: "/p", Name: "d", TagKey: "session:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Focused {
		t.Errorf("action = %q, want focused", res.Action)
	}
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "workspace" && c[1] == "create" {
			t.Error("should not spawn when tag matches")
		}
	}
	var selected, focused bool
	for _, c := range calls {
		if c[0] == "select-workspace" {
			selected = true
			joined := strings.Join(c, " ")
			if !strings.Contains(joined, "workspace:3") || !strings.Contains(joined, "WIN1") {
				t.Errorf("select args = %v", c)
			}
		}
		if c[0] == "focus-window" {
			focused = true
		}
	}
	if !selected || !focused {
		t.Errorf("selected=%v focused=%v calls=%v", selected, focused, calls)
	}
}

func TestCmuxUnavailableNamesConfig(t *testing.T) {
	err := cmuxUnavailable(fmt.Errorf("connection refused"))
	if !strings.Contains(err.Error(), "terminal.backend") || !strings.Contains(err.Error(), "iterm2") {
		t.Errorf("want config guidance, got %v", err)
	}
}
