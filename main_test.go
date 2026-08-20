// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDefaultAddrIsLoopback(t *testing.T) {
	if defaultAddr != "localhost:19419" {
		t.Fatalf("defaultAddr = %q, want localhost:19419 sentinel", defaultAddr)
	}
}

func TestLocalHTTPBaseURL(t *testing.T) {
	tests := map[string]string{
		"127.0.0.1:19419":            "http://127.0.0.1:19419",
		"localhost:19419":            "http://localhost:19419",
		":19419":                     "http://127.0.0.1:19419",
		"0.0.0.0:19419":              "http://127.0.0.1:19419",
		"[::]:19419":                 "http://127.0.0.1:19419",
		"[::1]:19419":                "http://[::1]:19419",
		"http://localhost:8080":      "http://localhost:8080",
		"http://0.0.0.0:19419":       "http://127.0.0.1:19419",
		"http://[::]:19419/debug///": "http://127.0.0.1:19419/debug",
	}
	for in, want := range tests {
		if got := localHTTPBaseURL(in); got != want {
			t.Errorf("localHTTPBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDaemonBaseURLNormalizesEnvOverride(t *testing.T) {
	t.Setenv("MNEMO_ADDR", "0.0.0.0:19419")
	if got, want := daemonBaseURL(), "http://127.0.0.1:19419"; got != want {
		t.Fatalf("daemonBaseURL() = %q, want %q", got, want)
	}
}

func TestDaemonBaseURLDefaultUsesLocalhost(t *testing.T) {
	t.Setenv("MNEMO_ADDR", "")
	if got, want := daemonBaseURL(), "http://localhost:19419"; got != want {
		t.Fatalf("daemonBaseURL() = %q, want %q", got, want)
	}
}

func TestOpenLocalListenersRejectsExplicitLocalhost(t *testing.T) {
	if _, err := openLocalListeners("localhost:19419", false); err == nil {
		t.Fatal("openLocalListeners accepted explicit localhost")
	}
}

func TestOpenLocalListenersExplicitIPv4(t *testing.T) {
	set, err := openLocalListeners("127.0.0.1:0", false)
	if err != nil {
		t.Fatalf("openLocalListeners explicit IPv4: %v", err)
	}
	defer set.close()
	if got := len(set.listeners); got != 1 {
		t.Fatalf("listener count = %d, want 1", got)
	}
	if got, want := set.listeners[0].network, "tcp"; got != want {
		t.Fatalf("listener network = %q, want %q", got, want)
	}
	if !strings.HasPrefix(set.baseURL, "http://127.0.0.1:") {
		t.Fatalf("baseURL = %q, want IPv4 loopback", set.baseURL)
	}
}

func TestOpenLocalListenersImplicitDefaultExpandsLoopback(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("IPv4 loopback unavailable: %v", err)
	}
	port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()

	set, err := openLocalListeners("localhost:"+port, true)
	if err != nil {
		t.Fatalf("openLocalListeners implicit default: %v", err)
	}
	defer set.close()
	if len(set.listeners) == 0 {
		t.Fatal("no listeners opened")
	}
	for _, l := range set.listeners {
		if l.network != "tcp4" && l.network != "tcp6" {
			t.Fatalf("listener network = %q, want tcp4 or tcp6", l.network)
		}
		if strings.Contains(l.addr, "localhost") {
			t.Fatalf("listener passed localhost through to net.Listen: %q", l.addr)
		}
	}
}

func TestOpenLocalListenersImplicitDefaultFailsOnBusyFamily(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("IPv4 loopback unavailable: %v", err)
	}
	defer ln.Close()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if set, err := openLocalListeners("localhost:"+port, true); err == nil {
		set.close()
		t.Fatal("openLocalListeners succeeded with IPv4 loopback port already in use")
	}
}

func TestListenPort(t *testing.T) {
	tests := map[string]string{
		"127.0.0.1:19419":        "19419",
		":19419":                 "19419",
		"[::]:19419":             "19419",
		"http://[::1]:19419":     "19419",
		"https://host:19419/mcp": "19419",
	}
	for in, want := range tests {
		if got := listenPort(in); got != want {
			t.Errorf("listenPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsWildcardListenAddr(t *testing.T) {
	tests := map[string]bool{
		"*:19419":         true,
		"0.0.0.0:19419":   true,
		"[::]:19419":      true,
		"127.0.0.1:19419": false,
		"[::1]:19419":     false,
	}
	for in, want := range tests {
		if got := isWildcardListenAddr(in); got != want {
			t.Errorf("isWildcardListenAddr(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestSummariserWorkDir verifies the compactor/reviewer working
// directory (🎯T82) is a dedicated, created directory under the OS temp
// root — not a repo checkout — with no CLAUDE.md to leak project context
// into the summariser.
func TestSummariserWorkDir(t *testing.T) {
	dir := summariserWorkDir()
	if dir == "" {
		t.Fatal("summariserWorkDir returned empty on a healthy system")
	}
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Errorf("workdir %q is not under the OS temp root %q", dir, os.TempDir())
	}
	if base := filepath.Base(dir); base != "mnemo-summariser" {
		t.Errorf("workdir base = %q, want mnemo-summariser", base)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("workdir was not created as a directory: %v", err)
	}
	// The summariser must not pick up a project CLAUDE.md from its cwd.
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("unexpected CLAUDE.md in summariser workdir %q", dir)
	}
	// Idempotent: a second call returns the same path without error.
	if again := summariserWorkDir(); again != dir {
		t.Errorf("non-idempotent: %q != %q", again, dir)
	}
}
