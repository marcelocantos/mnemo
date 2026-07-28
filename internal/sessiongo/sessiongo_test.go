// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package sessiongo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// fakeBackend implements only ResolveSessionRef. Embedding the interface
// leaves every other method nil — any accidental call panics loudly rather
// than passing silently, which is what we want from a stub.
type fakeBackend struct {
	store.Backend
	hit store.SessionRef
	err error
}

func (f fakeBackend) ResolveSessionRef(string) (store.SessionRef, error) {
	return f.hit, f.err
}

// The checks below all fail before iTerm2 is involved, which is the point:
// they are the decisions worth getting right, and they are shared by the
// MCP tool, the HTTP endpoint and `mnemo resume`.

func TestOpenReportsUnresolvableReference(t *testing.T) {
	_, err := Open(context.Background(),
		fakeBackend{err: errors.New("no session matches \"zzz\"")}, "zzz")
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("want UserError, got %v", err)
	}
	if !strings.Contains(err.Error(), "zzz") {
		t.Errorf("error should name the reference that failed: %v", err)
	}
}

func TestOpenReportsMissingWorkingDirectory(t *testing.T) {
	_, err := Open(context.Background(),
		fakeBackend{hit: store.SessionRef{SessionID: "abc123"}}, "abc123")
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("want UserError, got %v", err)
	}
	if !strings.Contains(err.Error(), "abc123") {
		t.Errorf("error should name the session: %v", err)
	}
}

// A session's directory outlives neither a deleted repo nor a temp
// checkout, and both are common in the real index. Opening a terminal
// somewhere arbitrary instead would hand the agent a working tree that
// contradicts its own transcript.
func TestOpenReportsVanishedWorkingDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "deleted-repo")
	_, err := Open(context.Background(),
		fakeBackend{hit: store.SessionRef{SessionID: "abc123", CWD: gone, Source: "claude"}}, "abc123")
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("want UserError, got %v", err)
	}
	if !strings.Contains(err.Error(), gone) {
		t.Errorf("error should name the directory that is gone: %v", err)
	}
}

// A source with no verified resume path must fail by name. Opening a bare
// shell instead looks like success and silently loses the conversation.
func TestOpenRefusesSourceWithNoResumeCommand(t *testing.T) {
	_, err := Open(context.Background(),
		fakeBackend{hit: store.SessionRef{
			SessionID: "abc123", CWD: t.TempDir(), Source: "codex",
		}}, "abc123")
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("want UserError, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "codex") {
		t.Errorf("error should name the source it cannot resume: %v", err)
	}
}
