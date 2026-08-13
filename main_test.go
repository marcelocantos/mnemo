// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultAddrIsLoopback(t *testing.T) {
	if defaultAddr != "127.0.0.1:19419" {
		t.Fatalf("defaultAddr = %q, want loopback-only 127.0.0.1:19419", defaultAddr)
	}
}

func TestLocalHTTPBaseURL(t *testing.T) {
	tests := map[string]string{
		"127.0.0.1:19419":       "http://127.0.0.1:19419",
		":19419":                "http://127.0.0.1:19419",
		"http://localhost:8080": "http://localhost:8080",
	}
	for in, want := range tests {
		if got := localHTTPBaseURL(in); got != want {
			t.Errorf("localHTTPBaseURL(%q) = %q, want %q", in, got, want)
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
