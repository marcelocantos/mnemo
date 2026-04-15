// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParsePsEnvOutput verifies that parsePsEnvOutput extracts PWD from
// ps -wwEo pid,command output, including graceful degradation when PWD is
// absent or the line is malformed.
func TestParsePsEnvOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[int]string
	}{
		{
			name:  "empty output",
			input: "",
			want:  map[int]string{},
		},
		{
			name:  "header only",
			input: "  PID COMMAND\n",
			want:  map[int]string{},
		},
		{
			name: "single PID with PWD",
			input: "  PID COMMAND\n" +
				"1234 /usr/bin/claude --continue FOO=bar PWD=/Users/alice/work SHELL=/bin/zsh\n",
			want: map[int]string{1234: "/Users/alice/work"},
		},
		{
			name: "PWD absent — graceful degradation",
			input: "  PID COMMAND\n" +
				"5678 /usr/bin/claude --continue SHELL=/bin/zsh TERM=xterm-256color\n",
			want: map[int]string{},
		},
		{
			name: "multiple PIDs, some with PWD",
			input: "  PID COMMAND\n" +
				"100 /usr/bin/claude PWD=/home/bob/proj LANG=en_US\n" +
				"200 /usr/bin/claude LANG=en_AU SHELL=/bin/bash\n" +
				"300 /usr/bin/claude PWD=/tmp/test HOME=/root\n",
			want: map[int]string{
				100: "/home/bob/proj",
				300: "/tmp/test",
			},
		},
		{
			name: "invalid PID is skipped",
			input: "  PID COMMAND\n" +
				"abc /usr/bin/claude PWD=/somewhere\n" +
				"999 /usr/bin/claude PWD=/valid\n",
			want: map[int]string{999: "/valid"},
		},
		{
			name: "line with too few fields is skipped",
			input: "  PID COMMAND\n" +
				"42\n" +
				"1001 /usr/bin/claude PWD=/ok\n",
			want: map[int]string{1001: "/ok"},
		},
		{
			name: "PWD value with equals sign in path — takes first PWD token",
			input: "  PID COMMAND\n" +
				"2000 /usr/bin/claude PWD=/some/path KEY=a=b\n",
			want: map[int]string{2000: "/some/path"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePsEnvOutput([]byte(tc.input))
			if len(got) != len(tc.want) {
				t.Errorf("got %d entries, want %d: %v", len(got), len(tc.want), got)
				return
			}
			for pid, wantCwd := range tc.want {
				if gotCwd, ok := got[pid]; !ok {
					t.Errorf("missing PID %d in result", pid)
				} else if gotCwd != wantCwd {
					t.Errorf("PID %d: got cwd %q, want %q", pid, gotCwd, wantCwd)
				}
			}
		})
	}
}

// TestCwdToTranscripts verifies that cwdToTranscripts maps a cwd to .jsonl
// files in the corresponding ~/.claude/projects/<encoded> directory, sorted
// newest-mtime first.
func TestCwdToTranscripts(t *testing.T) {
	// Build a fake home directory with a projects subtree.
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/Users/alice/work/myrepo"
	encoded := strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Truncate(time.Second)

	// Write two .jsonl files with distinct mtimes.
	older := filepath.Join(dir, "session-old.jsonl")
	newer := filepath.Join(dir, "session-new.jsonl")
	nonJSONL := filepath.Join(dir, "some.db")
	for _, p := range []string{older, newer, nonJSONL} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Set mtimes explicitly.
	if err := os.Chtimes(older, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}

	got := cwdToTranscripts(cwd)

	if len(got) != 2 {
		t.Fatalf("want 2 transcripts, got %d: %v", len(got), got)
	}
	// Newest first.
	if !strings.HasSuffix(got[0].Path, "session-new.jsonl") {
		t.Errorf("want session-new.jsonl first, got %s", got[0].Path)
	}
	if !strings.HasSuffix(got[1].Path, "session-old.jsonl") {
		t.Errorf("want session-old.jsonl second, got %s", got[1].Path)
	}
	// Non-.jsonl file excluded.
	for _, tr := range got {
		if strings.HasSuffix(tr.Path, ".db") {
			t.Errorf("non-jsonl file should be excluded: %s", tr.Path)
		}
	}
}

// TestCwdToTranscriptsMissingDir verifies graceful degradation when the
// projects directory for a cwd does not exist.
func TestCwdToTranscriptsMissingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := cwdToTranscripts("/nonexistent/path/that/has/no/project")
	if len(got) != 0 {
		t.Errorf("expected empty result for missing dir, got %v", got)
	}
}

// TestWhatsupPsEnvSeam verifies that Whatsup uses the runPsEnv seam and
// populates cwd + transcripts on WhatsupSession entries.
func TestWhatsupPsEnvSeam(t *testing.T) {
	// Build a fake home with a transcript for PID 9999.
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/Users/alice/work/proj"
	encoded := strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(dir, "abc123.jsonl")
	if err := os.WriteFile(transcriptPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Override runPsEnv to return a fake ps -E line for PID 9999.
	orig := runPsEnv
	defer func() { runPsEnv = orig }()
	runPsEnv = func(pids []string) []byte {
		return []byte("  PID COMMAND\n9999 /usr/bin/claude PWD=" + cwd + " SHELL=/bin/zsh\n")
	}

	s := newTestStore(t, t.TempDir())

	// Inject a fake live session by overriding the liveness cache directly.
	s.liveMu.Lock()
	s.liveCache = map[string]int{"abc123": 9999}
	s.liveCacheTime = time.Now().Add(time.Hour) // ensure cache is fresh
	s.liveMu.Unlock()

	result, err := s.Whatsup(false)
	if err != nil {
		t.Fatalf("Whatsup: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(result.Sessions))
	}
	sess := result.Sessions[0]
	if sess.Cwd != cwd {
		t.Errorf("cwd: got %q, want %q", sess.Cwd, cwd)
	}
	if len(sess.Transcripts) != 1 {
		t.Fatalf("want 1 transcript, got %d", len(sess.Transcripts))
	}
	if sess.Transcripts[0].Path != transcriptPath {
		t.Errorf("transcript path: got %q, want %q", sess.Transcripts[0].Path, transcriptPath)
	}
}

// TestWhatsupPsEnvFailure verifies graceful degradation when runPsEnv
// returns empty output (e.g. ps fails or PWD is absent from env).
func TestWhatsupPsEnvFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Override runPsEnv to simulate failure.
	orig := runPsEnv
	defer func() { runPsEnv = orig }()
	runPsEnv = func(pids []string) []byte { return nil }

	s := newTestStore(t, t.TempDir())

	s.liveMu.Lock()
	s.liveCache = map[string]int{"defsession": 1111}
	s.liveCacheTime = time.Now().Add(time.Hour)
	s.liveMu.Unlock()

	result, err := s.Whatsup(false)
	if err != nil {
		t.Fatalf("Whatsup: %v", err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(result.Sessions))
	}
	sess := result.Sessions[0]
	// Cwd must be empty string, transcripts empty.
	if sess.Cwd != "" {
		t.Errorf("expected empty cwd on ps failure, got %q", sess.Cwd)
	}
	if len(sess.Transcripts) != 0 {
		t.Errorf("expected no transcripts on ps failure, got %v", sess.Transcripts)
	}
}
