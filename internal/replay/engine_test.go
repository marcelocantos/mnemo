// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDryRunNoFilesystemChanges(t *testing.T) {
	root := t.TempDir()
	before, _ := os.ReadDir(root)
	eng := NewEngine(Config{DryRun: true})
	ops := []Op{{
		Timestamp: time.Now(),
		Path:      "/tmp/project/foo.go",
		CWD:       "/tmp/project",
		Repo:      "acme/project",
		Kind:      KindWrite,
		Body:      []byte("hello"),
	}}
	report, err := eng.Run(root, ops)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsApplied != 1 {
		t.Fatalf("applied=%d want 1", report.OpsApplied)
	}
	after, _ := os.ReadDir(root)
	if len(after) != len(before) {
		t.Fatal("dry-run created files")
	}
}

func TestLastWriterWins(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "checkout")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fake git repo for repo-relative layout
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, "foo.go")
	eng := NewEngine(DefaultConfig())
	ts := time.Now()
	ops := []Op{
		{Timestamp: ts, Path: path, CWD: cwd, Repo: "acme/x", Kind: KindWrite, Body: []byte("first")},
		{Timestamp: ts.Add(time.Second), Path: path, CWD: cwd, Repo: "acme/x", Kind: KindWrite, Body: []byte("second")},
	}
	report, err := eng.Run(root, ops)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsApplied != 2 {
		t.Fatalf("applied=%d want 2", report.OpsApplied)
	}
	key := "acme--x/foo.go"
	full := filepath.Join(root, filepath.FromSlash(key))
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("content=%q want second", data)
	}
}

func TestPathEscapeRefused(t *testing.T) {
	root := t.TempDir()
	eng := NewEngine(DefaultConfig())
	ops := []Op{{
		Timestamp: time.Now(),
		Path:      "../../../etc/passwd",
		CWD:       root,
		Kind:      KindWrite,
		Body:      []byte("pwnd"),
	}}
	report, err := eng.Run(root, ops)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsFailed+report.OpsSkipped == 0 {
		t.Fatal("expected skip or fail for escape/outside scope")
	}
}

func TestPatchNoBaseSkipped(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj")
	_ = os.MkdirAll(filepath.Join(cwd, ".git"), 0o755)
	eng := NewEngine(DefaultConfig())
	ops := []Op{{
		Timestamp: time.Now(),
		Path:      filepath.Join(cwd, "a.go"),
		CWD:       cwd,
		Repo:      "r/p",
		Kind:      KindPatch,
		OldString: "a",
		NewString: "b",
	}}
	report, err := eng.Run(root, ops)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsSkipped != 1 || report.Results[0].Reason != ReasonPatchNoBase {
		t.Fatalf("got %+v", report.Results[0])
	}
}

func TestLiveTreeRefused(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	eng := NewEngine(DefaultConfig())
	_, err := eng.Run(repo, nil)
	if err == nil {
		t.Fatal("expected live tree refusal")
	}
}

func TestExpandCodexAddAndUpdate(t *testing.T) {
	patch := `*** Begin Patch
*** Add File: main.go
+package main
+
+func main() {}
*** Update File: main.go
-package main
+package main // edited
*** End Patch`
	base := Op{Timestamp: time.Now(), SessionID: "s", Source: "codex", ToolUseID: "c1"}
	ops := ExpandCodexPatch(base, patch)
	if len(ops) != 2 {
		t.Fatalf("ops=%d want 2", len(ops))
	}
	if ops[0].Kind != KindWrite || ops[1].Kind != KindPatch {
		t.Fatalf("kinds=%v %v", ops[0].Kind, ops[1].Kind)
	}
}

func TestOpFromToolRowWriteEdit(t *testing.T) {
	ops, reason := OpFromToolRow(ToolRow{
		Timestamp: time.Now(),
		ToolName:  "Write",
		ToolInput: []byte(`{"file_path":"/x.go","content":"body"}`),
		FilePath:  "/x.go",
	})
	if reason != "" || len(ops) != 1 || string(ops[0].Body) != "body" {
		t.Fatalf("write: ops=%v reason=%q", ops, reason)
	}
	falseVal := false
	ops, reason = OpFromToolRow(ToolRow{
		ToolName:  "Edit",
		ToolInput: []byte(`{"file_path":"/x.go","old_string":"a","new_string":"b"}`),
		ResultError: &falseVal,
	})
	if reason != "" || ops[0].Kind != KindPatch {
		t.Fatalf("edit: %+v reason=%q", ops, reason)
	}
	trueVal := true
	_, reason = OpFromToolRow(ToolRow{
		ToolName:    "Write",
		ToolInput:   []byte(`{"content":"x"}`),
		ResultError: &trueVal,
	})
	if reason != ReasonToolUseFailed {
		t.Fatalf("reason=%q want tool_use_failed", reason)
	}
}
