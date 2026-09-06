// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)

// flagValue returns the argv element following flag, or ok=false.
func flagValue(argv []string, flag string) (string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return "", false
	}
	return argv[i+1], true
}

func TestArgsBareSurface(t *testing.T) {
	req := Request{
		Model:         "sonnet",
		SystemPrompt:  "You are a compactor.",
		Prompt:        "[marker] transcript",
		DisallowTools: []string{"Bash", "Read"},
	}
	argv := Args(req)

	if argv[0] != "-p" {
		t.Fatalf("argv[0]=%q, want -p", argv[0])
	}
	if argv[len(argv)-1] != req.Prompt {
		t.Fatalf("prompt must be the last positional arg; got %q", argv[len(argv)-1])
	}
	for flag, want := range map[string]string{
		"--system-prompt":   req.SystemPrompt,
		"--tools":           "",
		"--setting-sources": "",
		"--model":           "sonnet",
		"--output-format":   "stream-json",
		"--max-budget-usd":  "1.00",
	} {
		got, ok := flagValue(argv, flag)
		if !ok {
			t.Errorf("%s missing from argv %q", flag, argv)
		} else if got != want {
			t.Errorf("%s=%q, want %q", flag, got, want)
		}
	}
	if !slices.Contains(argv, "--strict-mcp-config") {
		t.Error("--strict-mcp-config missing: user-scope MCP servers would load")
	}
	if slices.Contains(argv, "--dangerously-skip-permissions") {
		t.Error("--dangerously-skip-permissions present: a tool-less turn has nothing to skip permissions for")
	}
	disallowed, _ := flagValue(argv, "--disallowedTools")
	for _, tool := range append(strings.Split(claudia.BaseDisallowedTools, ","), "Bash", "Read") {
		if !slices.Contains(strings.Split(disallowed, ","), tool) {
			t.Errorf("%q missing from --disallowedTools %q", tool, disallowed)
		}
	}
}

func TestArgsModelOptionalAndBudgetOverride(t *testing.T) {
	argv := Args(Request{SystemPrompt: "s", Prompt: "p", MaxBudgetUSD: 0.25})
	if slices.Contains(argv, "--model") {
		t.Error("--model emitted with no model requested")
	}
	if got, _ := flagValue(argv, "--max-budget-usd"); got != "0.25" {
		t.Errorf("--max-budget-usd=%q, want 0.25", got)
	}
}

func TestEnvStripsNestedMarkerAndDisablesCaching(t *testing.T) {
	env := Env([]string{"PATH=/bin", "CLAUDECODE=1", "DISABLE_PROMPT_CACHING=0", "HOME=/h"})
	if slices.Contains(env, "CLAUDECODE=1") {
		t.Error("CLAUDECODE leaked into the child env (nested-session refusal)")
	}
	if !slices.Contains(env, "DISABLE_PROMPT_CACHING=1") {
		t.Errorf("DISABLE_PROMPT_CACHING=1 missing: %v", env)
	}
	if slices.Contains(env, "DISABLE_PROMPT_CACHING=0") {
		t.Error("a pre-existing DISABLE_PROMPT_CACHING=0 survived alongside =1")
	}
	for _, keep := range []string{"PATH=/bin", "HOME=/h"} {
		if !slices.Contains(env, keep) {
			t.Errorf("%s dropped", keep)
		}
	}
}

// fakeClaude installs a shell script as the claude binary. It records
// its argv (one element per line) and environment to files in dir and
// emits the given stream-json lines on stdout.
func fakeClaude(t *testing.T, dir string, stdoutLines []string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"" + filepath.Join(dir, "argv") + "\"\n" +
		"env > \"" + filepath.Join(dir, "env") + "\"\n" +
		"pwd > \"" + filepath.Join(dir, "cwd") + "\"\n" +
		"cat <<'EOF'\n" + strings.Join(stdoutLines, "\n") + "\nEOF\n" +
		"exit " + itoa(exitCode) + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinEnv, bin)
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Replace(string(rune('0'+n)), "\x00", "", 1))
}

const (
	initLine   = `{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-sonnet-5"}`
	textLine1  = `{"type":"assistant","message":{"content":[{"type":"text","text":"{\"summary\":"}]}}`
	textLine2  = `{"type":"assistant","message":{"content":[{"type":"text","text":"\"ok\"}"}]}}`
	resultLine = `{"type":"result","subtype":"success","result":"{\"summary\":\"ok\"}","duration_ms":1234,"total_cost_usd":0.0123,"usage":{"input_tokens":30000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":900}}`
	errorLine  = `{"type":"result","subtype":"success","is_error":true,"result":"Invalid model name: nope","usage":{}}`
)

func TestRunParsesStreamAndSpawnsBare(t *testing.T) {
	dir := t.TempDir()
	fakeClaude(t, dir, []string{initLine, textLine1, textLine2, resultLine}, 0)
	t.Setenv("CLAUDECODE", "1")

	work := t.TempDir()
	res, err := Run(context.Background(), Request{
		WorkDir: work, Model: "sonnet", SystemPrompt: "sys", Prompt: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != `{"summary":"ok"}` {
		t.Errorf("Text=%q", res.Text)
	}
	if res.Model != "claude-sonnet-5" || res.SessionID != "sess-1" {
		t.Errorf("init not applied: %+v", res)
	}
	if res.PromptTokens() != 30000 || res.OutputTokens != 900 || res.CostUSD != 0.0123 || res.DurationMs != 1234 {
		t.Errorf("usage not applied: %+v", res)
	}

	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	if want := Args(Request{WorkDir: work, Model: "sonnet", SystemPrompt: "sys", Prompt: "user"}); !slices.Equal(got, want) {
		t.Errorf("child argv\n got %q\nwant %q", got, want)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(env), "CLAUDECODE=") {
		t.Error("CLAUDECODE reached the child")
	}
	if !strings.Contains(string(env), "DISABLE_PROMPT_CACHING=1") {
		t.Error("DISABLE_PROMPT_CACHING=1 did not reach the child")
	}
	cwd, _ := os.ReadFile(filepath.Join(dir, "cwd"))
	if strings.TrimSpace(string(cwd)) != work {
		// macOS may report the /private prefix; compare by EvalSymlinks.
		w1, _ := filepath.EvalSymlinks(work)
		w2, _ := filepath.EvalSymlinks(strings.TrimSpace(string(cwd)))
		if w1 != w2 {
			t.Errorf("cwd=%q, want %q", cwd, work)
		}
	}
}

func TestRunSurfacesResultError(t *testing.T) {
	dir := t.TempDir()
	fakeClaude(t, dir, []string{initLine, errorLine}, 0)
	_, err := Run(context.Background(), Request{SystemPrompt: "s", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "Invalid model name") {
		t.Fatalf("err=%v, want the result's error message", err)
	}
}

func TestRunWithoutResultIsAnError(t *testing.T) {
	dir := t.TempDir()
	fakeClaude(t, dir, []string{initLine, textLine1}, 1)
	_, err := Run(context.Background(), Request{SystemPrompt: "s", Prompt: "p"})
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("err=%v, want ErrNoResult", err)
	}
}

func TestRunRequiresSystemPrompt(t *testing.T) {
	_, err := Run(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, ErrNoSystemPrompt) {
		t.Fatalf("err=%v, want ErrNoSystemPrompt", err)
	}
}

func TestRunHonoursContextCancel(t *testing.T) {
	dir := t.TempDir()
	// A claude that never answers.
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinEnv, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, Request{SystemPrompt: "s", Prompt: "p"})
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("Run did not return promptly after cancel: %v", time.Since(start))
	}
}
