// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPruneCrashDumps(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 9; i++ {
		body := []byte("fatal error: example\n")
		if i%3 == 0 {
			body = nil // a clean run leaves an empty file
		}
		name := filepath.Join(dir, fmt.Sprintf("crash-20261015-1300%02d-1.log", i))
		if err := os.WriteFile(name, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneCrashDumps(dir, 3)
	left, _ := filepath.Glob(filepath.Join(dir, "crash-*.log"))
	want := []string{"crash-20261015-130005-1.log", "crash-20261015-130007-1.log", "crash-20261015-130008-1.log"}
	if len(left) != len(want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	for i, w := range want {
		if filepath.Base(left[i]) != w {
			t.Fatalf("left %v, want %v", left, want)
		}
	}
}
