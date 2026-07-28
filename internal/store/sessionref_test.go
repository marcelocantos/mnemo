// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// seedSession inserts a session with enough shape to be resolvable.
func seedSession(t *testing.T, s *Store, id, repo, project, cwd, source, lastMsg string, msgs int) {
	t.Helper()
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, repo, cwd, source) VALUES (?, ?, ?, ?)`,
		id, repo, cwd, source,
	); err != nil {
		t.Fatal(err)
	}
	// session_summary is a view over messages, so give it messages to
	// summarise rather than trying to insert into the view.
	for i := range msgs {
		if _, err := s.writeDB.Exec(
			`INSERT INTO messages (session_id, project, role, text, timestamp, type)
			 VALUES (?, ?, 'user', ?, ?, 'text')`,
			id, project, "body", lastMsg,
		); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}
}

func TestResolveSessionRef(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	seedSession(t, s, "aaaaaaaa-1111-2222-3333-444444444444",
		"marcelocantos/mnemo", "-Users-a-mnemo", "/w/mnemo", "claude", "2026-07-01T00:00:00Z", 5)
	seedSession(t, s, "bbbbbbbb-1111-2222-3333-444444444444",
		"squz/yourworld", "-Users-a-yourworld", "/w/yourworld", "claude", "2026-07-20T00:00:00Z", 5)
	seedSession(t, s, "cccccccc-1111-2222-3333-444444444444",
		"marcelocantos/mnemo", "-Users-a-mnemo", "/w/mnemo2", "codex", "2026-07-25T00:00:00Z", 5)

	tests := []struct {
		name string
		ref  string
		want string // expected session id prefix
	}{
		{"empty means newest overall", "", "cccccccc"},
		{"latest means newest overall", "latest", "cccccccc"},
		{"recent is a synonym", "recent", "cccccccc"},
		{"latest:scope narrows by repo", "latest:yourworld", "bbbbbbbb"},
		{"latest scope with a space, as people speak", "latest yourworld", "bbbbbbbb"},
		{"a bare repo fragment resolves to its newest", "yourworld", "bbbbbbbb"},
		{"a repo with several sessions gives the newest", "mnemo", "cccccccc"},
		{"a full id resolves exactly", "aaaaaaaa-1111-2222-3333-444444444444", "aaaaaaaa"},
		{"an id prefix resolves", "aaaaaaaa", "aaaaaaaa"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ResolveSessionRef(tc.ref)
			if err != nil {
				t.Fatalf("ResolveSessionRef(%q): %v", tc.ref, err)
			}
			if !strings.HasPrefix(got.SessionID, tc.want) {
				t.Errorf("ResolveSessionRef(%q) = %s, want prefix %s", tc.ref, got.SessionID, tc.want)
			}
		})
	}
}

// TestResolveSessionRefAmbiguousPrefix: guessing here would silently open
// the wrong conversation, which is the one outcome worse than an error.
func TestResolveSessionRefAmbiguousPrefix(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedSession(t, s, "dddddddd-1111-0000-0000-000000000001",
		"o/r", "-p", "/w/a", "claude", "2026-07-01T00:00:00Z", 3)
	seedSession(t, s, "dddddddd-1111-0000-0000-000000000002",
		"o/r", "-p", "/w/b", "claude", "2026-07-02T00:00:00Z", 3)

	_, err := s.ResolveSessionRef("dddddddd")
	if err == nil {
		t.Fatal("an id prefix matching two sessions must not silently pick one")
	}
	if !strings.Contains(err.Error(), "longer prefix") {
		t.Errorf("error should say how to disambiguate, got: %v", err)
	}
}

// TestResolveSessionRefNoMatch: zero matches must be explicit, and the
// message should point at the forms that do work.
func TestResolveSessionRefNoMatch(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedSession(t, s, "eeeeeeee-1111-0000-0000-000000000001",
		"o/r", "-p", "/w/a", "claude", "2026-07-01T00:00:00Z", 3)

	if _, err := s.ResolveSessionRef("nothing-like-this"); err == nil {
		t.Fatal("expected an error for a reference matching nothing")
	}
}

// TestResolveSessionRefSkipsTrivialSessions: resuming a one-message
// session is never what "latest" meant.
func TestResolveSessionRefSkipsTrivialSessions(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedSession(t, s, "11111111-0000-0000-0000-000000000001",
		"o/real", "-p", "/w/real", "claude", "2026-07-01T00:00:00Z", 5)
	seedSession(t, s, "22222222-0000-0000-0000-000000000002",
		"o/trivial", "-p", "/w/trivial", "claude", "2026-07-30T00:00:00Z", 1)

	got, err := s.ResolveSessionRef("latest")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got.SessionID, "22222222") {
		t.Error("latest picked a one-message session over a substantive older one")
	}
}

// TestResumeCommandIsSourceAware: mnemo indexes three agents that do not
// share a CLI. Opening a bare shell for a Codex session would look like
// success while doing something else entirely.
func TestResumeCommandIsSourceAware(t *testing.T) {
	claude := SessionRef{SessionID: "abc", Source: "claude"}
	cmd, err := claude.ResumeCommand()
	if err != nil {
		t.Fatalf("claude session must be resumable: %v", err)
	}
	if !strings.Contains(cmd, "--resume abc") {
		t.Errorf("resume command = %q, want it to resume the id", cmd)
	}

	// An empty source is a Claude session from before source tagging.
	if _, err := (SessionRef{SessionID: "abc"}).ResumeCommand(); err != nil {
		t.Errorf("untagged session should default to claude: %v", err)
	}

	for _, src := range []string{"codex", "grok"} {
		if _, err := (SessionRef{SessionID: "abc", Source: src}).ResumeCommand(); err == nil {
			t.Errorf("%s has no known resume command; it must refuse rather than guess", src)
		} else if !strings.Contains(err.Error(), src) {
			t.Errorf("refusal should name the source, got: %v", err)
		}
	}
}
