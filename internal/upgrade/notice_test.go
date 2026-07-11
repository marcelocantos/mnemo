// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import "testing"

func TestNoticeOncePerSession(t *testing.T) {
	t.Parallel()
	tr := NewNoticeTracker()
	tr.MarkSession("s1", "0.61.0", "0.62.0")
	tr.MarkSession("s1", "0.61.0", "0.62.0") // duplicate mark
	msg, ok := tr.Consume("s1")
	if !ok {
		t.Fatal("expected notice")
	}
	want := "mnemo upgraded v0.61.0 -> v0.62.0"
	if msg != want {
		t.Fatalf("got %q want %q", msg, want)
	}
	if _, ok := tr.Consume("s1"); ok {
		t.Fatal("second consume must be empty")
	}
	// Re-mark same upgrade after shown → suppressed
	tr.MarkSession("s1", "0.61.0", "0.62.0")
	if tr.PendingCount() != 0 {
		t.Fatal("should not re-queue already shown upgrade")
	}
	// Newer upgrade can notify again
	tr.MarkSession("s1", "0.62.0", "0.63.0")
	msg, ok = tr.Consume("s1")
	if !ok || msg != "mnemo upgraded v0.62.0 -> v0.63.0" {
		t.Fatalf("got %q ok=%v", msg, ok)
	}
}

func TestNoticeMarkSessions(t *testing.T) {
	t.Parallel()
	tr := NewNoticeTracker()
	tr.MarkSessions([]string{"a", "b", ""}, "1.0.0", "1.1.0")
	if tr.PendingCount() != 2 {
		t.Fatalf("pending %d", tr.PendingCount())
	}
}
