// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package e2e is the daemon-subprocess test harness for mnemo's
// integration / scale tests (🎯T73). It spawns `bin/mnemo` as a
// real subprocess with MNEMO_HOME=<tempdir>, drives it via the
// MCP HTTP transport, and tears down cleanly when the test ends.
//
// Tests built on top of this package exercise the daemon as a black
// box — through MCP, not through the in-process Go API — so failure
// modes that only manifest at the MCP / serialisation / per-user
// routing layer become testable.
//
// Typical usage:
//
//	func TestSomething(t *testing.T) {
//	    d := e2e.Start(t)              // launches daemon under MNEMO_HOME=tempdir
//	    out, err := d.Call(ctx, "mnemo_status", nil)
//	    if err != nil { t.Fatal(err) }
//	    // assert on out
//	}
//
// d.Stop is registered via t.Cleanup so tests don't have to.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// buildOnce caches the daemon binary across all tests in a single
// `go test` invocation. The first Start triggers `go build`; every
// subsequent Start reuses the cached path. The binary lives under
// the test cache dir and is removed when the test process exits.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// Daemon is a running mnemo daemon subprocess plus its MCP client.
// Call Stop to cancel it; t.Cleanup is wired in Start so most callers
// can ignore Stop entirely.
type Daemon struct {
	// Home is the MNEMO_HOME directory the daemon was launched
	// under. Tests may write fixtures (config.json, project
	// transcripts) under this root BEFORE calling Start, or read
	// the daemon's state after.
	Home string

	// URL is the http://127.0.0.1:<port>/mcp endpoint the daemon
	// is listening on. Exposed for tests that want a raw HTTP
	// client; most should use Call instead.
	URL string

	cmd    *exec.Cmd
	client *client.Client
	cancel context.CancelFunc
	stderr *strings.Builder
}

// Options controls the daemon's launch configuration. Zero-value is
// fine for most tests.
type Options struct {
	// Home overrides the MNEMO_HOME directory. Empty creates a
	// fresh tempdir (the common case).
	Home string

	// User selects the username injected into MCP requests via the
	// ?user= query parameter. Empty defaults to the process owner.
	User string

	// StartTimeout bounds how long Start waits for the daemon's
	// HTTP listener to become responsive. Zero defaults to 30s,
	// which is generous enough for cold builds.
	StartTimeout time.Duration

	// Env adds extra environment variables to the daemon
	// subprocess. Format: "KEY=value". MNEMO_HOME is always
	// injected and wins over any duplicate here.
	Env []string

	// Args appends extra CLI flags to the daemon invocation.
	// Useful for tests that want to disable federation, change
	// log level, etc.
	Args []string
}

// Start launches a mnemo daemon subprocess, waits for its MCP
// endpoint to become ready, returns a connected Daemon handle, and
// registers a t.Cleanup that calls Stop on test exit. Use opts...
// to override the default tempdir / timeout / args.
func Start(t *testing.T, opts ...Options) *Daemon {
	t.Helper()
	o := Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.StartTimeout == 0 {
		// 60s is generous enough for cold daemon startup on a loaded
		// Windows CI runner where the preceding test packages have
		// already saturated the CPU. The previous 30s ceiling caused
		// intermittent false-red failures on windows-latest.
		o.StartTimeout = 60 * time.Second
	}
	if o.Home == "" {
		o.Home = t.TempDir()
	}

	bin, err := ensureBinary()
	if err != nil {
		t.Fatalf("e2e: build daemon: %v", err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatalf("e2e: allocate port: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(context.Background())
	args := append([]string{"--addr", addr, "--federated-addr", ""}, o.Args...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"MNEMO_HOME="+o.Home,
		// Disable optional features that would surprise tests by
		// reaching out: backup worker, GitHub poller, federation
		// client all default to disabled-on-no-config and stay that
		// way under a fresh tempdir HOME.
	)
	cmd.Env = append(cmd.Env, o.Env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("e2e: spawn daemon: %v", err)
	}

	url := fmt.Sprintf("http://%s/mcp", addr)
	if o.User != "" {
		url += "?user=" + o.User
	}

	d := &Daemon{
		Home:   o.Home,
		URL:    url,
		cmd:    cmd,
		cancel: cancel,
		stderr: &stderr,
	}
	t.Cleanup(d.Stop)

	if err := d.waitReady(o.StartTimeout); err != nil {
		// On readiness failure, the captured stderr is the most
		// useful single artefact — surface it on the t.Fatal.
		t.Fatalf("e2e: daemon failed to become ready: %v\n--- daemon log ---\n%s", err, stderr.String())
	}

	cli, err := client.NewStreamableHttpClient(url)
	if err != nil {
		t.Fatalf("e2e: build MCP client: %v", err)
	}
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("e2e: start MCP client: %v", err)
	}
	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	defer initCancel()
	_, err = cli.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "mnemo-e2e", Version: "1"},
		},
	})
	if err != nil {
		t.Fatalf("e2e: MCP initialize: %v", err)
	}
	d.client = cli
	return d
}

// Stop shuts down the MCP client and daemon subprocess. Safe to call
// multiple times; idempotent. Wired via t.Cleanup in Start so most
// callers should not call this explicitly.
func (d *Daemon) Stop() {
	if d.client != nil {
		_ = d.client.Close()
		d.client = nil
	}
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	if d.cmd != nil {
		_ = d.cmd.Wait()
		d.cmd = nil
	}
}

// Call invokes an MCP tool by name and returns the first text-content
// block of the response. Arguments encode as JSON via the standard
// MCP CallToolRequest shape. A nil args map sends no parameters.
//
// When the tool returns an isError result, Call returns a non-nil
// error whose message is the tool's first text-content block.
//
// For multi-content responses or non-text content, use CallFull
// instead (returns the raw *mcp.CallToolResult).
func (d *Daemon) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	res, err := d.CallFull(ctx, tool, args)
	if err != nil {
		return "", err
	}
	text := firstText(res)
	if res.IsError {
		return text, fmt.Errorf("tool %q returned error: %s", tool, text)
	}
	return text, nil
}

// CallFull is Call without the text/error post-processing — returns
// the raw MCP CallToolResult. Use this when you need access to
// multi-block content or the IsError flag explicitly.
func (d *Daemon) CallFull(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	if d.client == nil {
		return nil, errors.New("e2e: daemon not started or already stopped")
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	if args != nil {
		req.Params.Arguments = args
	}
	return d.client.CallTool(ctx, req)
}

// Log returns everything the daemon has written to stdout/stderr
// since Start. Useful for failure diagnostics in tests that don't
// want to register their own stderr capture.
func (d *Daemon) Log() string { return d.stderr.String() }

// waitReady polls the daemon's HTTP listener until it accepts a TCP
// connection on /mcp, then succeeds. Timeout returns an error.
//
// We deliberately don't probe MCP-level readiness here; an HTTP-
// listener-accepts probe is enough — the MCP client's Initialize
// call below catches any deeper failure mode with a richer error.
func (d *Daemon) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := strings.TrimPrefix(strings.SplitN(d.URL, "/mcp", 2)[0], "http://")
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		// If the process died, surface that immediately rather
		// than waiting out the deadline.
		if d.cmd.ProcessState != nil && d.cmd.ProcessState.Exited() {
			return fmt.Errorf("daemon exited before ready: %s", d.cmd.ProcessState)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %v", timeout)
}

// freePort asks the kernel for an unused TCP port by listening on
// :0 and immediately closing. Race-free in practice for tests; a
// rare collision will surface as the daemon failing to bind, which
// waitReady catches.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ensureBinary builds the mnemo daemon binary once per test process
// and caches the resulting path. Subsequent Start calls reuse it.
// The build runs from the repo root (located by walking up from this
// source file's directory) with the sqlite_fts5 tag, matching what
// `make build` produces in normal use.
func ensureBinary() (string, error) {
	buildOnce.Do(func() {
		root, err := repoRoot()
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "mnemo-e2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "mnemo")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		cmd := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", out, ".")
		cmd.Dir = root
		if combined, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, combined)
			return
		}
		binPath = out
	})
	return binPath, buildErr
}

// repoRoot locates the mnemo repository root by walking up from this
// source file's directory until a go.mod is found whose module path
// is github.com/marcelocantos/mnemo. Anchored to the *file* rather
// than the test's working directory so tests in any package can
// build the daemon correctly.
func repoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 16; i++ {
		gm := filepath.Join(dir, "go.mod")
		if b, err := os.ReadFile(gm); err == nil {
			if strings.Contains(string(b), "module github.com/marcelocantos/mnemo") {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("mnemo repo root not found from " + thisFile)
}

// firstText extracts the first text-content block from a tool call
// result. Returns empty string when no text content is present —
// callers that care about non-text content should use CallFull
// instead.
func firstText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
