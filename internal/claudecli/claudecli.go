// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package claudecli runs one headless `claude -p` turn with a bare
// surface: the caller's own system prompt, no tools, no MCP servers, no
// settings files, and prompt caching disabled.
//
// Why this exists instead of claudia.Task. A summariser turn is a pure
// text→text call, but a stock `claude -p` spawn is a full Claude Code
// session: its own multi-thousand-token system prompt, every built-in
// tool schema, the user-scope MCP servers (and their deferred-tool
// listing), the global CLAUDE.md, and the skill listing. Measured on the
// production summariser (28 days, 4,543 compactor calls): a 20-token
// prompt cost 34,724 input tokens, and every call wrote its whole prompt
// to the 1-hour prompt cache at 2× the plain input rate even though no
// second turn ever reads it. That fixed context was ~90% of the
// summariser's token bill. The flags below bring the same 20-token probe
// down to 510 tokens and bill the transcript as plain input.
//
// The output contract (stream-json NDJSON) is unchanged, so parsing is
// delegated to claudia.ParseTaskLine; only the spawn surface differs.
package claudecli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/marcelocantos/claudia"
)

// DefaultMaxBudgetUSD is the per-call ceiling passed as --max-budget-usd
// when Request.MaxBudgetUSD is zero. A summariser turn costs cents; a
// dollar is several times the largest honest call and stops a runaway
// (the 🎯T139 incident was ~4.3 billion tokens from one spawn) inside a
// single call rather than after a session-wide ceiling notices.
const DefaultMaxBudgetUSD = 1.0

// BinEnv is the environment variable that overrides claude binary
// resolution — the same one claudia honours, so a test or a launchd
// plist configures both paths identically.
const BinEnv = "CLAUDE_BIN"

// DisablePromptCachingEnv is Claude Code's switch for cache_control
// markers. A summariser spawn is a single turn: a cache write is pure
// surcharge (1h tier bills at 2× plain input, 5m at 1.25×) because the
// entry is never read back.
const DisablePromptCachingEnv = "DISABLE_PROMPT_CACHING"

// nestedSessionEnv is set by Claude Code inside its own sessions; a
// child spawned with it present refuses to start ("nested session").
const nestedSessionEnv = "CLAUDECODE"

// maxLineBytes bounds one NDJSON line from claude. A result line carries
// the whole response text; summariser payloads are a few KB, but a
// misbehaving model can emit far more before --max-budget-usd stops it.
const maxLineBytes = 16 << 20

// waitDelay is how long Run waits for stdout to drain after the context
// is cancelled before the process is killed outright.
const waitDelay = 5 * time.Second

// stderrTailBytes is how much of claude's stderr is kept for the error
// message when the process fails without a result event.
const stderrTailBytes = 4 << 10

// Request describes one headless turn.
type Request struct {
	// WorkDir is the child's cwd. It is deliberately irrelevant to the
	// model — no CLAUDE.md is discovered from it — but claude still
	// keys session persistence on it.
	WorkDir string
	// Model is the --model value (alias or full id). Empty uses Claude
	// Code's default.
	Model string
	// SystemPrompt replaces Claude Code's built-in system prompt
	// entirely. Required: an empty value would reinstate the default.
	SystemPrompt string
	// Prompt is the single user turn, passed as the positional argument.
	Prompt string
	// DisallowTools is appended to claudia.BaseDisallowedTools. With
	// --tools "" there are no tools to remove, but the list stays as
	// belt-and-braces in case a future claude version grows a tool the
	// empty list does not cover.
	DisallowTools []string
	// MaxBudgetUSD caps the call; zero means DefaultMaxBudgetUSD.
	MaxBudgetUSD float64
}

// Result is the parsed outcome of one turn.
type Result struct {
	// Text is the concatenated assistant text.
	Text string
	// Model is the id claude resolved for the run (full id even when an
	// alias was requested); empty if the init event was absent.
	Model string
	// SessionID is the persisted session, for forensic correlation.
	SessionID string

	InputTokens         int
	CacheCreationTokens int
	CacheReadTokens     int
	OutputTokens        int
	CostUSD             float64
	DurationMs          float64
}

// PromptTokens is the total input the model processed, across all
// three billing classes.
func (r Result) PromptTokens() int {
	return r.InputTokens + r.CacheCreationTokens + r.CacheReadTokens
}

// ErrNoResult is returned when claude exited without emitting a result
// event — a crash, a kill, or an output format we no longer recognise.
var ErrNoResult = errors.New("claudecli: claude exited without a result event")

// ErrNoSystemPrompt is returned for a Request without a SystemPrompt;
// running without one would silently reinstate Claude Code's default.
var ErrNoSystemPrompt = errors.New("claudecli: Request.SystemPrompt is required")

// Args renders the argv for a request (binary excluded). Exported so a
// test can assert on what is actually handed to claude rather than on
// what this file appears to intend. The prompt is last and positional,
// as claudia passes it.
func Args(req Request) []string {
	budget := req.MaxBudgetUSD
	if budget <= 0 {
		budget = DefaultMaxBudgetUSD
	}
	disallowed := claudia.BaseDisallowedTools
	if len(req.DisallowTools) > 0 {
		disallowed += "," + strings.Join(req.DisallowTools, ",")
	}
	args := []string{
		"-p",
		"--verbose",
		"--output-format", "stream-json",
		// Our prompt, not Claude Code's — the built-in one plus its
		// per-machine sections was ~14k tokens of cache write per call.
		"--system-prompt", req.SystemPrompt,
		// No built-in tools: no schemas in the prefix, and nothing for
		// an imperative sentence inside the transcript to act on.
		"--tools", "",
		// No MCP servers: no server instructions, no deferred-tool
		// listing, no server start-up per call.
		"--strict-mcp-config",
		// No settings files: this is what stops the global CLAUDE.md,
		// the skill listing, and plugin context being injected.
		"--setting-sources", "",
		"--disallowedTools", disallowed,
		"--max-budget-usd", strconv.FormatFloat(budget, 'f', 2, 64),
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return append(args, req.Prompt)
}

// Env derives the child environment from base: the nested-session
// marker is removed and prompt caching is disabled.
func Env(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, nestedSessionEnv+"=") ||
			strings.HasPrefix(kv, DisablePromptCachingEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, DisablePromptCachingEnv+"=1")
}

// Run spawns claude for one turn and returns its parsed result.
func Run(ctx context.Context, req Request) (Result, error) {
	if req.SystemPrompt == "" {
		return Result{}, ErrNoSystemPrompt
	}
	bin, err := resolveBin()
	if err != nil {
		return Result{}, err
	}
	args := Args(req)
	slog.Debug("claudecli: spawning", "bin", bin, "model", req.Model, "prompt_bytes", len(req.Prompt))

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = req.WorkDir
	cmd.Env = Env(os.Environ())
	cmd.WaitDelay = waitDelay
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("claudecli: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("claudecli: start %s: %w", bin, err)
	}

	var (
		res       Result
		text      strings.Builder
		gotResult bool
		runErr    error
	)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	for scanner.Scan() {
		for _, ev := range claudia.ParseTaskLine(scanner.Bytes()) {
			switch ev.Type {
			case claudia.TaskEventInit:
				res.Model = ev.Model
				res.SessionID = ev.SessionID
			case claudia.TaskEventText:
				text.WriteString(ev.Content)
			case claudia.TaskEventResult:
				gotResult = true
				res.CostUSD = ev.CostUSD
				res.DurationMs = ev.DurationMs
				res.InputTokens = ev.Usage.InputTokens
				res.CacheCreationTokens = ev.Usage.CacheCreationInputTokens
				res.CacheReadTokens = ev.Usage.CacheReadInputTokens
				res.OutputTokens = ev.Usage.OutputTokens
			case claudia.TaskEventError:
				if runErr == nil {
					runErr = fmt.Errorf("claudecli: %s", ev.ErrorMsg)
				}
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	switch {
	case runErr != nil:
		return Result{}, runErr
	case ctx.Err() != nil:
		return Result{}, fmt.Errorf("claudecli: %w", ctx.Err())
	case scanErr != nil:
		return Result{}, fmt.Errorf("claudecli: read stdout: %w", scanErr)
	case !gotResult:
		return Result{}, fmt.Errorf("%w (wait: %v; stderr: %s)", ErrNoResult, waitErr, stderr.String())
	}
	if waitErr != nil {
		// A result was emitted, so the turn completed; a non-zero exit
		// after that is worth a log line, not a failed summarisation.
		slog.Debug("claudecli: claude exited non-zero after result", "err", waitErr)
	}
	res.Text = text.String()
	return res, nil
}

// resolveBin locates the claude executable: BinEnv (absolute path or
// PATH-resolvable name), then PATH, then the usual install locations.
// The fallbacks matter under a launcher (launchd, systemd) whose PATH
// lacks the user-local install dir. Mirrors claudia's resolution so the
// two spawn paths never disagree about which claude runs.
func resolveBin() (string, error) {
	if p := os.Getenv(BinEnv); p != "" {
		if filepath.IsAbs(p) {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		} else if abs, err := exec.LookPath(p); err == nil {
			return abs, nil
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("claudecli: claude binary not found (set " + BinEnv + " or add it to PATH)")
}

// tailBuffer keeps the last stderrTailBytes written to it.
type tailBuffer struct {
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if extra := t.buf.Len() - stderrTailBytes; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	return strings.TrimSpace(t.buf.String())
}
