// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// matrixCase maps design-note rows (E1–E22 subset) to expected outcomes.
func TestMatrixOracle(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cwd, "f.txt")

	cases := []struct {
		name   string
		pre    []Op
		op     Op
		want   Outcome
		reason string
		dryRun bool
	}{
		{
			name: "E1 patch_no_base",
			op: Op{
				Timestamp: time.Now(), Path: file, CWD: cwd, Repo: "o/r",
				Kind: KindPatch, OldString: "x", NewString: "y",
			},
			want: OutcomeSkipped, reason: ReasonPatchNoBase,
		},
		{
			name: "E3 tool_use_failed skipped at adapter",
			op: Op{
				Timestamp: time.Now(), Path: file, CWD: cwd, Repo: "o/r",
				Kind: KindWrite, Body: []byte("never applied"),
			},
			want: OutcomeApplied,
		},
		{
			name: "E13 dry_run",
			op: Op{
				Timestamp: time.Now(), Path: file, CWD: cwd, Repo: "o/r",
				Kind: KindWrite, Body: []byte("dry"),
			},
			want: OutcomeApplied, dryRun: true,
		},
		{
			name: "E7 delete_missing",
			op: Op{
				Timestamp: time.Now(), Path: file, CWD: cwd, Repo: "o/r",
				Kind: KindDelete,
			},
			want: OutcomeApplied, reason: ReasonDeleteMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := DefaultConfig()
			if tc.dryRun {
				cfg.DryRun = true
			}
			eng := NewEngine(cfg)
			var ops []Op
			ops = append(ops, tc.pre...)
			ops = append(ops, tc.op)
			report, err := eng.Run(dir, ops)
			if err != nil {
				t.Fatal(err)
			}
			last := report.Results[len(report.Results)-1]
			if last.Outcome != tc.want {
				t.Fatalf("outcome=%s want %s (reason=%s)", last.Outcome, tc.want, last.Reason)
			}
			if tc.reason != "" && last.Reason != tc.reason {
				t.Fatalf("reason=%q want %q", last.Reason, tc.reason)
			}
		})
	}
}

func TestCursorWriteStrReplaceDelete(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, "a.go")
	ts := time.Now()
	falseVal := false

	writeOps, _ := OpFromToolRow(ToolRow{
		Timestamp: ts, Source: "cursor", ToolName: "Write",
		ToolInput: []byte(`{"file_path":` + jsonStr(path) + `,"content":"hello"}`),
		FilePath:  path, CWD: cwd, Repo: "u/p", ResultError: &falseVal,
	})
	patchOps, _ := OpFromToolRow(ToolRow{
		Timestamp: ts.Add(time.Second), Source: "cursor", ToolName: "StrReplace",
		ToolInput: []byte(`{"file_path":` + jsonStr(path) + `,"old_string":"hello","new_string":"world"}`),
		FilePath:  path, CWD: cwd, Repo: "u/p", ResultError: &falseVal,
	})
	delOps, _ := OpFromToolRow(ToolRow{
		Timestamp: ts.Add(2 * time.Second), Source: "cursor", ToolName: "Delete",
		FilePath: path, CWD: cwd, Repo: "u/p", ResultError: &falseVal,
	})

	ops := append(writeOps, patchOps...)
	ops = append(ops, delOps...)
	eng := NewEngine(DefaultConfig())
	report, err := eng.Run(root, ops)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsApplied < 2 {
		t.Fatalf("applied=%d results=%+v", report.OpsApplied, report.Results)
	}
}

func TestGrokWriteSearchReplace(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	_ = os.MkdirAll(filepath.Join(cwd, ".git"), 0o755)
	path := filepath.Join(cwd, "b.go")
	ts := time.Now()
	falseVal := false

	w, _ := OpFromToolRow(ToolRow{
		Timestamp: ts, Source: "grok", ToolName: "write",
		ToolInput: []byte(`{"target_file":` + jsonStr(path) + `,"content":"one"}`),
		CWD:       cwd, Repo: "g/g", ResultError: &falseVal,
	})
	p, _ := OpFromToolRow(ToolRow{
		Timestamp: ts.Add(time.Second), Source: "grok", ToolName: "search_replace",
		ToolInput: []byte(`{"file_path":` + jsonStr(path) + `,"old_string":"one","new_string":"two"}`),
		CWD:       cwd, Repo: "g/g", ResultError: &falseVal,
	})
	eng := NewEngine(DefaultConfig())
	report, err := eng.Run(root, append(w, p...))
	if err != nil {
		t.Fatal(err)
	}
	key := "g--g/b.go"
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "two" {
		t.Fatalf("got %q", data)
	}
	if report.OpsApplied != 2 {
		t.Fatalf("applied=%d", report.OpsApplied)
	}
}

func TestCodexApplyPatchFixture(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	_ = os.MkdirAll(filepath.Join(cwd, ".git"), 0o755)
	patch := `*** Begin Patch
*** Add File: new.go
+line1
+line2
*** End Patch`
	falseVal := false
	ops, reason := OpFromToolRow(ToolRow{
		Timestamp: time.Now(), Source: "codex", ToolName: "apply_patch",
		Text: patch, CWD: cwd, Repo: "c/x", ResultError: &falseVal, ToolUseID: "p1",
	})
	if reason != "" || len(ops) != 1 {
		t.Fatalf("ops=%v reason=%q", ops, reason)
	}
	eng := NewEngine(DefaultConfig())
	report, err := eng.Run(root, ops)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsApplied != 1 {
		t.Fatalf("report=%+v", report)
	}
	body, _ := os.ReadFile(filepath.Join(root, "c--x", "new.go"))
	if string(body) != "line1\nline2" {
		t.Fatalf("body=%q", body)
	}
}

// jsonStr encodes s as a JSON string literal. Test paths must go through
// it: a Windows temp path pasted raw between quotes is invalid JSON (its
// backslashes read as escapes), and OpFromToolRow then yields no op.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
