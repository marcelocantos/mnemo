// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestNoStdinModeSniffing is the ratchet for 🎯T160.
//
// mnemo used to decide what it was by looking at stdin: not a character
// device meant "an MCP client launched me as a stdio server", which sent
// it down a path that rewrote ~/.claude.json, ran `brew services start
// mnemo`, and exited 1. supervisord gives every child a pipe on stdin so
// supervisorctl can write to it, so mnemo never stayed up under a
// process supervisor while the same binary run by hand was fine.
//
// The type of stdin carries no intent. A supervisor, a CI runner, a
// container entrypoint, `foo | mnemo`, and a genuine stdio parent are
// indistinguishable by that test. Argv is the only thing that says what
// the operator wanted, so nothing here may consult stdin's mode.
func TestNoStdinModeSniffing(t *testing.T) {
	banned := []struct{ pattern, why string }{
		{"os.Stdin.Stat()", "inspects stdin to infer how mnemo was launched"},
		{"stdinPiped", "the removed sniffing helper"},
	}
	for _, path := range goFilesInPackage(t) {
		if filepath.Base(path) == "no_stdio_test.go" {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		for _, b := range banned {
			if strings.Contains(body, b.pattern) {
				t.Errorf("%s references %s — %s. Invocation mode is an "+
					"explicit input; take it from argv.",
					filepath.Base(path), b.pattern, b.why)
			}
		}
		// Asking whether a stream is a terminal is fine in itself —
		// deciding whether to pretty-print OUTPUT is a normal use, and
		// thread_render.go does exactly that. What is banned is asking
		// it about STDIN, because that is the question whose answer was
		// mistaken for intent.
		if strings.Contains(body, "ModeCharDevice") && strings.Contains(body, "os.Stdin") {
			t.Errorf("%s tests stdin against ModeCharDevice. Whether stdin is a "+
				"pipe says nothing about how mnemo was launched — a supervisor, a "+
				"CI runner and a shell pipeline all look identical.",
				filepath.Base(path))
		}
	}
}

// TestNeverInstallsItselfAsAService is the second half of 🎯T160.
//
// The migration path ran `brew services start mnemo`, installing a
// launchd job with KeepAlive=true. That job outlived the config that
// created it: it came back after `brew services stop mnemo` and deleting
// the plist, which made the daemon look as though it were re-spawning
// itself out of nowhere.
//
// A daemon may be installed as a service by an operator. It may not
// install itself. `brew services restart` in the auto-upgrade path is
// deliberately allowed — it restarts a service the operator already
// installed and opted into, and installs nothing.
func TestNeverInstallsItselfAsAService(t *testing.T) {
	banned := []struct{ pattern, why string }{
		{`"services", "start"`, "starts a brew service for itself"},
		{`"services", "install"`, "installs a brew service for itself"},
		{`"launchctl", "load"`, "installs a launchd job for itself"},
		{`"launchctl", "bootstrap"`, "installs a launchd job for itself"},
		{`"systemctl", "enable"`, "installs a systemd unit for itself"},
	}
	for _, path := range goFilesInPackage(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range banned {
			if strings.Contains(string(src), b.pattern) {
				t.Errorf("%s %s. Installing a service is the operator's job; "+
					"a self-installed KeepAlive job outlives the thing that "+
					"created it.", filepath.Base(path), b.why)
			}
		}
	}
}

// TestPipedStdinRunsTheDaemon is the behavioural oracle: the ratchets
// above prove the old code is gone, this proves the reported symptom is
// gone. It reproduces what supervisord does — hand the process a pipe on
// stdin — and asserts mnemo behaves as a daemon rather than exiting.
//
// It also asserts the config file is untouched, because the migration
// did not merely exit: it rewrote ~/.claude.json on every start, a no-op
// rewrite of an entry that was already in HTTP form.
func TestPipedStdinRunsTheDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and starts a daemon")
	}
	// .exe on Windows, or the built file is not executable and exec
	// reports "executable file not found in %PATH%".
	name := "mnemo-under-test"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if out, err := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	home := t.TempDir()
	// A registration already in the HTTP form — exactly the shape the
	// migration used to rewrite pointlessly on every launch.
	const registration = `{"mcpServers":{"mnemo":{"type":"http","url":"http://localhost:19419/mcp?user=someone"}}}`
	cfgPath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(cfgPath, []byte(registration), 0o644); err != nil {
		t.Fatal(err)
	}

	// Port 0 so this never collides with a real daemon on 19419.
	cmd := exec.Command(bin, "--addr", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "HOME="+home, "MNEMO_HOME="+home)
	stdin, err := cmd.StdinPipe() // the supervisord shape
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		t.Fatalf("mnemo exited (%v) with a pipe on stdin; it must run as a daemon.\n"+
			"This is the supervisord failure: a pipe on stdin is not a stdio MCP client.\n%s",
			err, out.String())
	case <-time.After(8 * time.Second):
		// Still running: correct.
	}
	_ = stdin.Close()
	_ = cmd.Process.Kill()
	<-done

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != registration {
		t.Errorf("startup rewrote the MCP registration.\n got: %s\nwant: %s\n"+
			"Starting the daemon must not edit the user's config; only "+
			"`mnemo register-mcp` writes registrations.", got, registration)
	}
	if s := out.String(); strings.Contains(s, "migrat") || strings.Contains(s, "restart this Claude Code session") {
		t.Errorf("startup emitted migration output:\n%s", s)
	}
}

// goFilesInPackage lists the .go files in the main package directory.
func goFilesInPackage(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			out = append(out, e.Name())
		}
	}
	return out
}
