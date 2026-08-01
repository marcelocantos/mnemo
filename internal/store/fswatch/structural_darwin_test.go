// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package fswatch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDarwinBackendNotRecursiveFsnotify asserts the Darwin watch implementation
// uses FSEvents and the store package does not Walk+fsnotify.Add every dir in Watch.
func TestDarwinBackendNotRecursiveFsnotify(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	pkgDir := filepath.Dir(thisFile)

	// watch_darwin.go must import fsevents, not fsnotify.
	darwinSrc, err := os.ReadFile(filepath.Join(pkgDir, "watch_darwin.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(darwinSrc)
	if !strings.Contains(s, "github.com/fsnotify/fsevents") {
		t.Fatal("watch_darwin.go must import fsevents")
	}
	if strings.Contains(s, "github.com/fsnotify/fsnotify") {
		t.Fatal("watch_darwin.go must not import fsnotify/kqueue")
	}

	// store.go Watch must not contain recursive Walk + watcher.Add pattern.
	storeGo := filepath.Join(pkgDir, "..", "store.go")
	src, err := os.ReadFile(storeGo)
	if err != nil {
		t.Fatal(err)
	}
	// Parse to ensure file is valid Go; scan for forbidden legacy pattern.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, storeGo, src, 0); err != nil {
		t.Fatalf("parse store.go: %v", err)
	}
	text := string(src)
	if strings.Contains(text, "filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {\n\t\t\tif err == nil && info.IsDir() {\n\t\t\t\tif wErr := watcher.Add(path)") {
		t.Fatal("store.Watch still recursive Walk+fsnotify.Add on every directory")
	}
	if !strings.Contains(text, "fswatch.New") {
		t.Fatal("store.Watch must call fswatch.New")
	}
	// registry vault watcher
	regGo := filepath.Join(pkgDir, "..", "..", "registry", "registry.go")
	regSrc, err := os.ReadFile(regGo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(regSrc), "fswatch.New") {
		t.Fatal("vault watcher must use fswatch.New")
	}
	if strings.Contains(string(regSrc), "fsnotify.NewWatcher") {
		t.Fatal("registry must not construct raw fsnotify watchers")
	}
}
