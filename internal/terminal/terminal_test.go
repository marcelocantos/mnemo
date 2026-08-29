// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestGoRejectsEmptyPath(t *testing.T) {
	if _, err := Go(context.Background(), GoArgs{}); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestGoRejectsSingleQuoteInCommand(t *testing.T) {
	_, err := Go(context.Background(), GoArgs{Path: "/p", Command: "echo 'x'"})
	if err == nil || !strings.Contains(err.Error(), "single quote") {
		t.Fatalf("want single-quote refusal, got %v", err)
	}
}

func TestGoDispatchesConfiguredBackend(t *testing.T) {
	restoreLoad := loadConfig
	restoreLookup := lookupBackend
	defer func() {
		loadConfig = restoreLoad
		lookupBackend = restoreLookup
	}()

	loadConfig = func() (store.Config, error) {
		return store.Config{Terminal: store.TerminalConfig{Backend: "cmux"}}, nil
	}
	var saw string
	lookupBackend = func(name string) (Backend, error) {
		saw = name
		return fakeBackend{action: Spawned}, nil
	}

	res, err := Go(context.Background(), GoArgs{Path: "/p", Name: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if saw != store.TerminalBackendCmux {
		t.Errorf("dispatched %q, want cmux", saw)
	}
	if res.Action != Spawned {
		t.Errorf("action = %q", res.Action)
	}
}

func TestGoDefaultsToITerm2(t *testing.T) {
	restoreLoad := loadConfig
	restoreLookup := lookupBackend
	defer func() {
		loadConfig = restoreLoad
		lookupBackend = restoreLookup
	}()

	loadConfig = func() (store.Config, error) { return store.Config{}, nil }
	var saw string
	lookupBackend = func(name string) (Backend, error) {
		saw = name
		return fakeBackend{action: Focused}, nil
	}

	if _, err := Go(context.Background(), GoArgs{Path: "/p"}); err != nil {
		t.Fatal(err)
	}
	if saw != store.TerminalBackendITerm2 {
		t.Errorf("default backend = %q, want iterm2", saw)
	}
}

func TestDefaultLookupRejectsUnknown(t *testing.T) {
	_, err := defaultLookupBackend("ghostty")
	if err == nil || !strings.Contains(err.Error(), "ghostty") {
		t.Fatalf("want named unsupported error, got %v", err)
	}
	if !strings.Contains(err.Error(), "iterm2") || !strings.Contains(err.Error(), "cmux") {
		t.Errorf("error should name valid backends: %v", err)
	}
}

type fakeBackend struct {
	action Action
	err    error
}

func (f fakeBackend) Go(context.Context, GoArgs) (Result, error) {
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Action: f.action, Path: "/p"}, nil
}

func TestTagKey(t *testing.T) {
	if got := tagKey(GoArgs{Path: "/a", TagKey: "session:1"}); got != "session:1" {
		t.Errorf("got %q", got)
	}
	if got := tagKey(GoArgs{Path: "/a"}); got != "/a" {
		t.Errorf("got %q", got)
	}
}

// Ensure the fake satisfies Backend even if we add methods later.
var _ Backend = fakeBackend{}

func TestGoPropagatesBackendError(t *testing.T) {
	restoreLoad := loadConfig
	restoreLookup := lookupBackend
	defer func() {
		loadConfig = restoreLoad
		lookupBackend = restoreLookup
	}()
	loadConfig = func() (store.Config, error) { return store.Config{}, nil }
	lookupBackend = func(string) (Backend, error) {
		return fakeBackend{err: fmt.Errorf("boom")}, nil
	}
	_, err := Go(context.Background(), GoArgs{Path: "/p"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want boom, got %v", err)
	}
}
