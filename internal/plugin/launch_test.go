// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// testPluginSource is a minimal plugin: bind 127.0.0.1:0, print
// MNEMO_PLUGIN_PORT, serve /ready + /manifest. Optional env:
//
//	MNEMO_TEST_EXIT_AFTER — Go duration; exit after that (crash-restart tests)
//	MNEMO_TEST_NAME       — manifest name (default "liveness")
const testPluginSource = `package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("MNEMO_PLUGIN_PORT %d\n", port)

	name := os.Getenv("MNEMO_TEST_NAME")
	if name == "" {
		name = "liveness"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol_version": 1,
			"name":             name,
			"version":          "0.1.0",
			"facets":           map[string]bool{"check": true},
		})
	})
	// Echo a param so tests can verify env injection.
	mux.HandleFunc("/param", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, os.Getenv("MNEMO_PLUGIN_PARAM_GRACE_MULTIPLE"))
	})

	if d := os.Getenv("MNEMO_TEST_EXIT_AFTER"); d != "" {
		go func() {
			delay, err := time.ParseDuration(d)
			if err != nil {
				delay = 50 * time.Millisecond
			}
			time.Sleep(delay)
			os.Exit(2)
		}()
	}
	_ = http.Serve(ln, mux)
}
`

var (
	testPluginOnce sync.Once
	testPluginBin  string
	testPluginErr  error
)

// compiledTestPlugin builds the helper binary once per package test run.
func compiledTestPlugin(t *testing.T) string {
	t.Helper()
	testPluginOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mnemo-testplugin-*")
		if err != nil {
			testPluginErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(testPluginSource), 0o644); err != nil {
			testPluginErr = err
			return
		}
		bin := filepath.Join(dir, "testplugin")
		cmd := exec.Command("go", "build", "-o", bin, src)
		out, err := cmd.CombinedOutput()
		if err != nil {
			testPluginErr = fmt.Errorf("go build test plugin: %w\n%s", err, out)
			return
		}
		testPluginBin = bin
	})
	if testPluginErr != nil {
		t.Fatalf("compile test plugin: %v", testPluginErr)
	}
	return testPluginBin
}

func TestLaunchStartsAndAttaches(t *testing.T) {
	bin := compiledTestPlugin(t)
	home := t.TempDir()
	m := NewManager(home, nil, testLogger())
	// Fast stop for cleanup.
	m.launchCfg = launchConfig{StopGrace: 500 * time.Millisecond}
	t.Cleanup(m.Close)

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "liveness",
		Enabled:   true,
		Transport: store.PluginTransportLaunch,
		Command:   bin,
		Params:    map[string]any{"grace_multiple": 2.0},
	}})

	snap, ok := m.Get("liveness")
	if !ok {
		t.Fatal("expected instance")
	}
	if snap.State != StateReady {
		t.Fatalf("state=%s want ready (err=%q)", snap.State, snap.Err)
	}
	if snap.BaseURL == "" || !strings.HasPrefix(snap.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("base URL: %q", snap.BaseURL)
	}
	if snap.Manifest == nil || snap.Manifest.Name != "liveness" || snap.Manifest.Version != "0.1.0" {
		t.Fatalf("manifest: %+v", snap.Manifest)
	}

	// Param injection: child exposes MNEMO_PLUGIN_PARAM_GRACE_MULTIPLE.
	resp, err := http.Get(snap.BaseURL + "/param")
	if err != nil {
		t.Fatalf("param GET: %v", err)
	}
	defer resp.Body.Close()
	var buf [64]byte
	n, _ := resp.Body.Read(buf[:])
	if got := string(buf[:n]); got != "2" {
		t.Fatalf("param value: got %q want 2", got)
	}

	// Diag healthy.
	res := m.DynamicChecks()[0].Run(context.Background())
	if res.Severity.String() != "ok" {
		t.Fatalf("diag: %+v", res)
	}

	// Disable tears down process and clears metadata.
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "liveness",
		Enabled:   false,
		Transport: store.PluginTransportLaunch,
		Command:   bin,
	}})
	snap, _ = m.Get("liveness")
	if snap.State != StateStopped {
		t.Fatalf("after disable: state=%s", snap.State)
	}
	if snap.BaseURL != "" || snap.Manifest != nil {
		t.Fatalf("after disable: metadata not cleared: %+v", snap)
	}
}

func TestLaunchHandshakeMissing(t *testing.T) {
	// Empty program exits without printing the port line.
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	_ = os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	emptyBin := filepath.Join(dir, "empty")
	if out, err := exec.Command("go", "build", "-o", emptyBin, src).CombinedOutput(); err != nil {
		t.Fatalf("build empty: %v\n%s", err, out)
	}

	m := NewManager(t.TempDir(), nil, testLogger())
	m.launchCfg = launchConfig{
		HandshakeTimeout: 2 * time.Second,
		StopGrace:        200 * time.Millisecond,
		BackoffMin:       time.Hour, // do not restart during this test
		BreakerThreshold: 100,
		BreakerCooldown:  time.Hour,
	}
	t.Cleanup(m.Close)

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "liveness",
		Enabled:   true,
		Transport: store.PluginTransportLaunch,
		Command:   emptyBin,
	}})
	snap, _ := m.Get("liveness")
	if snap.State != StateError {
		t.Fatalf("state=%s want error (err=%q)", snap.State, snap.Err)
	}
	if !strings.Contains(snap.Err, "handshake") && !strings.Contains(snap.Err, "MNEMO_PLUGIN_PORT") {
		t.Fatalf("expected handshake error, got %q", snap.Err)
	}
}

func TestLaunchRestartsAfterCrash(t *testing.T) {
	bin := compiledTestPlugin(t)
	m := NewManager(t.TempDir(), nil, testLogger())
	m.launchCfg = launchConfig{
		StopGrace:        500 * time.Millisecond,
		BackoffMin:       50 * time.Millisecond,
		BackoffMax:       200 * time.Millisecond,
		BreakerThreshold: 20,
	}
	t.Cleanup(m.Close)

	// Child exits shortly after becoming ready; supervisor should restart.
	// Pass exit-after via the plugin's own env by wrapping: use Args won't work
	// for env. Set via a small shell wrapper... cleaner: put env in the entry
	// by having the test binary read MNEMO_PLUGIN_PARAM or we set OS env for child
	// through params is only MNEMO_PLUGIN_PARAM_*. Use a wrapper script:

	// Actually buildPluginEnv only sets MNEMO_PLUGIN_*. Add exit via param that
	// the test plugin doesn't use. Better: write a wrapper that sets env.
	wrapper := filepath.Join(t.TempDir(), "wrap.sh")
	script := fmt.Sprintf("#!/bin/sh\nexport MNEMO_TEST_EXIT_AFTER=80ms\nexport MNEMO_TEST_NAME=liveness\nexec %q \"$@\"\n", bin)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "liveness",
		Enabled:   true,
		Transport: store.PluginTransportLaunch,
		Command:   wrapper,
	}})
	snap, _ := m.Get("liveness")
	if snap.State != StateReady {
		t.Fatalf("initial: state=%s err=%q", snap.State, snap.Err)
	}
	firstURL := snap.BaseURL

	// Wait until we observe a successful restart (new base URL or re-ready after error).
	deadline := time.Now().Add(5 * time.Second)
	sawError := false
	restarted := false
	for time.Now().Before(deadline) {
		snap, _ = m.Get("liveness")
		if snap.State == StateError {
			sawError = true
		}
		if snap.State == StateReady && snap.BaseURL != "" && (sawError || snap.BaseURL != firstURL) {
			restarted = true
			break
		}
		// Same URL is possible if OS reuses the port; accept Ready after Error.
		if sawError && snap.State == StateReady {
			restarted = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !restarted {
		snap, _ = m.Get("liveness")
		t.Fatalf("expected restart after crash; last state=%s err=%q url=%q first=%q",
			snap.State, snap.Err, snap.BaseURL, firstURL)
	}
}

func TestLaunchBreakerTripsOnPersistentFailure(t *testing.T) {
	// Command that always exits immediately without handshake.
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	_ = os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	failBin := filepath.Join(dir, "fail")
	if out, err := exec.Command("go", "build", "-o", failBin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fail bin: %v\n%s", err, out)
	}

	m := NewManager(t.TempDir(), nil, testLogger())
	m.launchCfg = launchConfig{
		HandshakeTimeout: 500 * time.Millisecond,
		StopGrace:        200 * time.Millisecond,
		BackoffMin:       20 * time.Millisecond,
		BackoffMax:       50 * time.Millisecond,
		BreakerThreshold: 3,
		BreakerCooldown:  time.Hour, // stay open for the rest of the test
	}
	t.Cleanup(m.Close)

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name:      "liveness",
		Enabled:   true,
		Transport: store.PluginTransportLaunch,
		Command:   failBin,
	}})
	// First failure is synchronous.
	snap, _ := m.Get("liveness")
	if snap.State != StateError {
		t.Fatalf("state=%s want error", snap.State)
	}

	// Wait until breaker opens (threshold consecutive failures in the loop).
	deadline := time.Now().Add(5 * time.Second)
	tripped := false
	for time.Now().Before(deadline) {
		snap, _ = m.Get("liveness")
		if strings.Contains(snap.Err, "circuit breaker") {
			tripped = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !tripped {
		snap, _ = m.Get("liveness")
		t.Fatalf("expected circuit breaker open; err=%q", snap.Err)
	}

	res := m.DynamicChecks()[0].Run(context.Background())
	if res.Severity.String() != "fail" {
		t.Fatalf("diag severity=%s want fail: %+v", res.Severity.String(), res)
	}
	if res.Remediation == "" {
		t.Fatal("expected remediation for breaker")
	}
}

func TestLaunchAndConnectCoexist(t *testing.T) {
	bin := compiledTestPlugin(t)

	// Connect-mode: external process we own, attach via URL.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "MNEMO_TEST_NAME=other")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	port, err := scanHandshake(stdout)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	connectURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	liveWrap := filepath.Join(t.TempDir(), "live.sh")
	script := fmt.Sprintf("#!/bin/sh\nexport MNEMO_TEST_NAME=liveness\nexec %q\n", bin)
	if err := os.WriteFile(liveWrap, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewManager(t.TempDir(), nil, testLogger())
	m.launchCfg = launchConfig{StopGrace: 500 * time.Millisecond}
	t.Cleanup(m.Close)

	m.Reconcile(context.Background(), []store.PluginEntry{
		{
			Name: "liveness", Enabled: true, Transport: store.PluginTransportLaunch, Command: liveWrap,
		},
		{
			Name: "other", Enabled: true, Transport: store.PluginTransportConnect, URL: connectURL,
		},
	})

	live, _ := m.Get("liveness")
	other, _ := m.Get("other")
	if live.State != StateReady {
		t.Fatalf("launch: state=%s err=%q", live.State, live.Err)
	}
	if other.State != StateReady {
		t.Fatalf("connect: state=%s err=%q", other.State, other.Err)
	}
	if live.Transport != store.PluginTransportLaunch || other.Transport != store.PluginTransportConnect {
		t.Fatalf("transports: %s / %s", live.Transport, other.Transport)
	}
}

func TestScanHandshake(t *testing.T) {
	port, err := scanHandshake(strings.NewReader("noise\nMNEMO_PLUGIN_PORT 54321\nmore\n"))
	if err != nil {
		t.Fatal(err)
	}
	if port != 54321 {
		t.Fatalf("port=%d", port)
	}
	_, err = scanHandshake(strings.NewReader("no port here\n"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParamEnvKey(t *testing.T) {
	if got := paramEnvKey("grace_multiple"); got != "GRACE_MULTIPLE" {
		t.Fatalf("got %q", got)
	}
	if got := paramEnvKey("grace-multiple"); got != "GRACE_MULTIPLE" {
		t.Fatalf("got %q", got)
	}
	if got := stringifyParam(2.0); got != "2" {
		t.Fatalf("stringify float: %q", got)
	}
	if got := stringifyParam(map[string]any{"a": 1}); got != `{"a":1}` {
		t.Fatalf("stringify object: %q", got)
	}
}
