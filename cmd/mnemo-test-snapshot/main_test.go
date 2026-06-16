// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeSourceHome lays down a minimal source $HOME with ~/.mnemo and
// ~/.claude/projects content. Returns the home dir.
func makeSourceHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	mnemo := filepath.Join(home, ".mnemo")
	if err := os.MkdirAll(mnemo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnemo, "mnemo.db"), []byte("fake db"), 0o644); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(home, ".claude", "projects", "proj-a")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projects, "session.jsonl"), []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// extractMnemoHome scans stdout for the last `MNEMO_HOME=...` line.
func extractMnemoHome(t *testing.T, stdout string) string {
	t.Helper()
	var out string
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if strings.HasPrefix(line, "MNEMO_HOME=") {
			out = strings.TrimPrefix(line, "MNEMO_HOME=")
		}
	}
	if out == "" {
		t.Fatalf("no MNEMO_HOME= line in stdout: %q", stdout)
	}
	// Should be the LAST line.
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "MNEMO_HOME=") {
		t.Fatalf("MNEMO_HOME= is not the last line of stdout: %q", stdout)
	}
	return out
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Fatalf("%s: got %q, want %q", path, string(b), want)
	}
}

func TestSnapshot_Darwin_CloneCopy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only: exercises cp -c -R clonefile path")
	}
	home := makeSourceHome(t)
	dest := filepath.Join(t.TempDir(), "snap")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--source-home", home, "--dest", dest}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	got := extractMnemoHome(t, stdout.String())
	if got != dest {
		t.Fatalf("MNEMO_HOME: got %q, want %q", got, dest)
	}
	assertContent(t, filepath.Join(dest, ".mnemo", "mnemo.db"), "fake db")
	assertContent(t, filepath.Join(dest, ".claude", "projects", "proj-a", "session.jsonl"), "line\n")
	// Stub config.json should exist.
	if _, err := os.Stat(filepath.Join(dest, ".mnemo", "config.json")); err != nil {
		t.Fatalf("expected stub config.json: %v", err)
	}
}

func TestSnapshot_FallbackCopy(t *testing.T) {
	prev := forceFallback
	forceFallback = true
	t.Cleanup(func() { forceFallback = prev })

	home := makeSourceHome(t)
	dest := filepath.Join(t.TempDir(), "snap")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--source-home", home, "--dest", dest}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	got := extractMnemoHome(t, stdout.String())
	if got != dest {
		t.Fatalf("MNEMO_HOME: got %q, want %q", got, dest)
	}
	if !strings.Contains(stderr.String(), "[fallback]") {
		t.Fatalf("expected [fallback] log line in stderr, got: %s", stderr.String())
	}
	assertContent(t, filepath.Join(dest, ".mnemo", "mnemo.db"), "fake db")
	assertContent(t, filepath.Join(dest, ".claude", "projects", "proj-a", "session.jsonl"), "line\n")
}

func TestSnapshot_RefusesGitTrackedDest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := makeSourceHome(t)

	gitRoot := t.TempDir()
	cmd := exec.Command("git", "-C", gitRoot, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	dest := filepath.Join(gitRoot, "subdir", "snap")

	var stdout, stderr bytes.Buffer
	err := run([]string{"--source-home", home, "--dest", dest}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected error for git-tracked destination, got nil")
	}
	if !strings.Contains(err.Error(), "git working tree") {
		t.Fatalf("error message does not mention git working tree: %v", err)
	}
}

func TestSnapshot_RefusesExistingWithoutForce(t *testing.T) {
	home := makeSourceHome(t)
	dest := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "marker"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := run([]string{"--source-home", home, "--dest", dest}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("expected error for existing destination without --force")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error does not mention 'already exists': %v", err)
	}
	// Marker file must still be there.
	if _, err := os.Stat(filepath.Join(dest, "marker")); err != nil {
		t.Fatalf("marker file was removed: %v", err)
	}
}

func TestSnapshot_AllowsExistingWithForce(t *testing.T) {
	home := makeSourceHome(t)
	dest := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "marker"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--source-home", home, "--dest", dest, "--force"}, &stdout, &stderr); err != nil {
		t.Fatalf("run with --force: %v\nstderr: %s", err, stderr.String())
	}
	got := extractMnemoHome(t, stdout.String())
	if got != dest {
		t.Fatalf("MNEMO_HOME: got %q, want %q", got, dest)
	}
	// Old marker should be gone (clobbered).
	if _, err := os.Stat(filepath.Join(dest, "marker")); !os.IsNotExist(err) {
		t.Fatalf("expected marker to be clobbered, got err: %v", err)
	}
	// Fresh content present.
	assertContent(t, filepath.Join(dest, ".mnemo", "mnemo.db"), "fake db")
}

func TestSnapshot_PrintsMnemoHomeLast(t *testing.T) {
	home := makeSourceHome(t)
	dest := filepath.Join(t.TempDir(), "snap")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--source-home", home, "--dest", dest}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	if last != "MNEMO_HOME="+dest {
		t.Fatalf("last stdout line = %q, want %q", last, "MNEMO_HOME="+dest)
	}
}

func TestSnapshot_MissingSourcesTolerated(t *testing.T) {
	home := t.TempDir() // no .mnemo, no .claude/projects
	dest := filepath.Join(t.TempDir(), "snap")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--source-home", home, "--dest", dest}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	got := extractMnemoHome(t, stdout.String())
	if got != dest {
		t.Fatalf("MNEMO_HOME: got %q, want %q", got, dest)
	}
	// Stub config.json should still be created.
	if _, err := os.Stat(filepath.Join(dest, ".mnemo", "config.json")); err != nil {
		t.Fatalf("expected stub config.json: %v", err)
	}
}
