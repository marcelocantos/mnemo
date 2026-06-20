// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

func newNoteHandler(t *testing.T) *callHandler {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "db.sqlite"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &callHandler{mem: s}
}

func TestNoteToolsRoundTrip(t *testing.T) {
	ch := newNoteHandler(t)
	inbox := t.TempDir()

	out, isErr, err := ch.notePost(map[string]any{"inbox": inbox, "body": "hello"})
	if err != nil || isErr {
		t.Fatalf("notePost: isErr=%v err=%v out=%s", isErr, err, out)
	}

	out, isErr, err = ch.noteRecv(map[string]any{"inbox": inbox})
	if err != nil || isErr {
		t.Fatalf("noteRecv: isErr=%v err=%v out=%s", isErr, err, out)
	}
	var got []store.Note
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Body != "hello" {
		t.Fatalf("recv = %+v, want one note 'hello'", got)
	}

	// Consumed: a second recv returns the empty marker.
	out, _, _ = ch.noteRecv(map[string]any{"inbox": inbox})
	if out != "No notes." {
		t.Errorf("2nd recv = %q, want %q", out, "No notes.")
	}
}

func TestNoteToolsValidation(t *testing.T) {
	ch := newNoteHandler(t)

	if _, isErr, _ := ch.notePost(map[string]any{"body": "x"}); !isErr {
		t.Error("expected error when inbox missing")
	}
	if _, isErr, _ := ch.notePost(map[string]any{"inbox": "/tmp"}); !isErr {
		t.Error("expected error when body missing")
	}
	if _, isErr, _ := ch.noteRecv(map[string]any{}); !isErr {
		t.Error("expected error when recv inbox missing")
	}
	// A bad inbox surfaces as a tool-level error, not a transport error.
	out, isErr, err := ch.notePost(map[string]any{"inbox": "/no/such/dir/here", "body": "x"})
	if err != nil {
		t.Errorf("transport error should be nil, got %v", err)
	}
	if !isErr {
		t.Errorf("expected tool error for bad inbox, got %q", out)
	}
}
