// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestBackendSurfaceHasNoDeadMethods is the 🎯T143 residue ratchet:
// every method on Backend must have a production caller outside
// internal/store and outside _test.go. A panic stub on fakeBackend is
// not a caller.
func TestBackendSurfaceHasNoDeadMethods(t *testing.T) {
	root := moduleRoot(t)
	typ := reflect.TypeOf((*Backend)(nil)).Elem()
	var dead []string
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !hasProductionCaller(t, root, name) {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Fatalf("Backend methods with no production caller outside internal/store: %s",
			strings.Join(dead, ", "))
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test working directory")
		}
		dir = parent
	}
}

func hasProductionCaller(t *testing.T, root, name string) bool {
	t.Helper()
	cmd := exec.Command("git", "grep", "-n", name+"(")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return false
		}
		t.Fatalf("git grep %s(: %v", name, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		path, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if path == "internal/store/store.go" || path == "internal/store/iface.go" {
			continue
		}
		return true
	}
	return false
}
