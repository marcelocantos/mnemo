// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// newNoteStore builds an empty store for note tests.
func newNoteStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "db.sqlite"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedSession records a session's cwd/repo in session_meta and binds it
// to an MCP connection, so resolveNoteOrigin can recover the cwd from the
// connection identity alone (the production path).
func (s *Store) seedSession(t *testing.T, connID, sid, repo, cwd string) {
	t.Helper()
	if _, err := s.writeDB.Exec(
		"INSERT OR IGNORE INTO session_meta (session_id, repo, cwd) VALUES (?, ?, ?)",
		sid, repo, cwd,
	); err != nil {
		t.Fatal(err)
	}
	s.RecordConnectionSession(connID, sid)
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}

// TestNoteRoundTripCrossRepo is acceptance criterion #7: two sessions in
// different repos round-trip a note. The producer addresses the consumer
// with a RELATIVE path resolved against its own session cwd (derived from
// connection identity, not pwd); the consumer reads with an absolute path.
// Both must address the same inbox.
func TestNoteRoundTripCrossRepo(t *testing.T) {
	s := newNoteStore(t)
	work := t.TempDir()
	mnemoDir := filepath.Join(work, "mnemo")
	yttDir := filepath.Join(work, "ytt")
	for _, d := range []string{mnemoDir, yttDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Producer session is rooted in .../mnemo, bound to conn-prod.
	s.seedSession(t, "conn-prod", "sess-prod", "mnemo", mnemoDir)

	posted, err := s.PostNote(NotePostParams{
		Inbox:        "../ytt", // relative to the producer's session cwd
		Body:         "mnemo v0.42 published, brew formula updated",
		ConnectionID: "conn-prod",
	})
	if err != nil {
		t.Fatalf("PostNote: %v", err)
	}
	wantInbox := mustEvalSymlinks(t, yttDir)
	if posted.Inbox != wantInbox {
		t.Errorf("posted inbox = %q, want %q", posted.Inbox, wantInbox)
	}
	if posted.FromSession != "sess-prod" || posted.FromRepo != "mnemo" {
		t.Errorf("from_* not stamped from identity: session=%q repo=%q", posted.FromSession, posted.FromRepo)
	}

	// Consumer in .../ytt reads with an absolute path, different conn.
	got, err := s.RecvNotes(NoteRecvParams{
		Inbox:        yttDir,
		ConnectionID: "conn-cons",
		UnreadOnly:   true,
		MarkRead:     true,
	})
	if err != nil {
		t.Fatalf("RecvNotes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d notes, want 1", len(got))
	}
	if got[0].Body != "mnemo v0.42 published, brew formula updated" {
		t.Errorf("body = %q", got[0].Body)
	}
	if got[0].ReadAt == nil {
		t.Error("note should be marked read after recv")
	}

	// Second recv (unread-only) sees nothing — the note was consumed.
	again, err := s.RecvNotes(NoteRecvParams{Inbox: yttDir, UnreadOnly: true, MarkRead: true})
	if err != nil {
		t.Fatalf("RecvNotes (2nd): %v", err)
	}
	if len(again) != 0 {
		t.Errorf("got %d notes on 2nd recv, want 0", len(again))
	}
}

// TestNoteInboxCanonicalization covers the addressing rules: collision-free
// across spellings, ~ rejection, and non-existent directory errors.
func TestNoteInboxCanonicalization(t *testing.T) {
	s := newNoteStore(t)
	dir := t.TempDir()
	real := mustEvalSymlinks(t, dir)

	// Three spellings of the same directory must land in one inbox.
	spellings := []string{
		dir,
		dir + "/",
		filepath.Join(dir, "sub", ".."),
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sp := range spellings {
		n, err := s.PostNote(NotePostParams{Inbox: sp, Body: "x"})
		if err != nil {
			t.Fatalf("PostNote(%q): %v", sp, err)
		}
		if n.Inbox != real {
			t.Errorf("spelling %q resolved to %q, want %q", sp, n.Inbox, real)
		}
	}
	all, err := s.RecvNotes(NoteRecvParams{Inbox: dir, MarkRead: false})
	if err != nil {
		t.Fatalf("RecvNotes: %v", err)
	}
	if len(all) != len(spellings) {
		t.Errorf("got %d notes in canonical inbox, want %d", len(all), len(spellings))
	}

	// A leading "~" (shell home-expansion) is rejected.
	if _, err := s.PostNote(NotePostParams{Inbox: "~/work/foo", Body: "x"}); err == nil {
		t.Error("expected error for inbox starting with '~'")
	}
	// A "~" *inside* a path component is a legitimate literal (e.g. Windows
	// 8.3 short names like C:\Users\RUNNER~1\...) and must be accepted.
	tildeDir := filepath.Join(dir, "RUNNER~1")
	if err := os.MkdirAll(tildeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostNote(NotePostParams{Inbox: tildeDir, Body: "x"}); err != nil {
		t.Errorf("inbox with a non-leading '~' should be accepted, got: %v", err)
	}

	// Posting to a non-existent directory errors and inserts no row.
	missing := filepath.Join(dir, "does-not-exist")
	if _, err := s.PostNote(NotePostParams{Inbox: missing, Body: "x"}); err == nil {
		t.Error("expected error for non-existent inbox")
	}
	none, _ := s.ListNotes(NoteListParams{Inbox: dir})
	if len(none) != len(spellings) {
		t.Errorf("non-existent post leaked a row: have %d, want %d", len(none), len(spellings))
	}

	// A relative path with no resolvable session cwd is a clear error.
	if _, err := s.PostNote(NotePostParams{Inbox: "../somewhere", Body: "x"}); err == nil {
		t.Error("expected error for relative inbox with unknown session cwd")
	}
}

// TestNoteListBrowsesWithoutConsuming verifies list does not mark read and
// the no-inbox form spans every inbox in the window.
func TestNoteListBrowsesWithoutConsuming(t *testing.T) {
	s := newNoteStore(t)
	a := t.TempDir()
	b := t.TempDir()
	if _, err := s.PostNote(NotePostParams{Inbox: a, Body: "to a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostNote(NotePostParams{Inbox: b, Body: "to b"}); err != nil {
		t.Fatal(err)
	}

	// No inbox → every inbox in the window.
	all, err := s.ListNotes(NoteListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListNotes(all) = %d, want 2", len(all))
	}
	for _, n := range all {
		if n.ReadAt != nil {
			t.Error("list must not mark notes read")
		}
	}

	// Scoped to one inbox.
	just, err := s.ListNotes(NoteListParams{Inbox: a})
	if err != nil {
		t.Fatal(err)
	}
	if len(just) != 1 || just[0].Body != "to a" {
		t.Errorf("ListNotes(a) = %+v, want one note 'to a'", just)
	}

	// The notes are still unread (list never consumes).
	unread, _ := s.RecvNotes(NoteRecvParams{Inbox: a, UnreadOnly: true, MarkRead: false})
	if len(unread) != 1 {
		t.Errorf("note in a should still be unread, got %d", len(unread))
	}
}
