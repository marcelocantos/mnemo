// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// seedStatusSession inserts a session's messages (alternating user/assistant,
// all stamped at the given relative offset so last_msg is deterministic) and
// its session_meta row. The messages_ai trigger builds session_summary
// (session_type 'interactive' for a normal project, substantive_msgs = count,
// last_msg = the stamp). Message ids are autoincrement, so insertion order is
// excerpt order.
func seedStatusSession(t *testing.T, s *Store, sid, repo, cwd, work, topic, offset string, texts []string) {
	t.Helper()
	for i, txt := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := s.writeDB.Exec(
			`INSERT INTO messages (session_id, project, role, text, timestamp, is_noise, content_type)
			 VALUES (?, ?, ?, ?, datetime('now', ?), 0, 'text')`,
			sid, repo, role, txt, offset); err != nil {
			t.Fatalf("seed message %s/%d: %v", sid, i, err)
		}
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, repo, cwd, work_type, topic) VALUES (?, ?, ?, ?, ?)`,
		sid, repo, cwd, work, topic); err != nil {
		t.Fatalf("seed meta %s: %v", sid, err)
	}
}

// TestStatusNestedQuery verifies the 🎯T94 single-query Status reproduces the
// old behaviour: repo ordering by recency, per-repo session cap (most recent),
// per-session newest-N excerpts in ascending order, byte truncation + flag.
func TestStatusNestedQuery(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	longText := "this is a very long pasted block that exceeds ten chars"
	// org/alpha: three sessions of differing recency; s-a1 has 5 messages
	// (last two should survive maxExcerpts=2), the last being long.
	seedStatusSession(t, s, "s-a1", "org/alpha", "/w/alpha", "branch-work", "topic A1",
		"-2 minutes", []string{"u1", "a1", "u2", "a2", longText})
	seedStatusSession(t, s, "s-a2", "org/alpha", "/w/alpha", "", "",
		"-1 minutes", []string{"hello", "world"}) // most recent
	seedStatusSession(t, s, "s-a3", "org/alpha", "/w/alpha", "", "",
		"-10 minutes", []string{"old1", "old2"}) // oldest → excluded by maxSessions=2
	// org/beta: one older session.
	seedStatusSession(t, s, "s-b1", "org/beta", "/w/beta", "", "",
		"-3 minutes", []string{"beta-only"})

	res, err := s.Status(7, "", 2, 2, 10)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(res.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(res.Repos))
	}

	// Repos ordered by last_activity DESC: alpha (s-a2 @ -1m) before beta (@ -3m).
	if res.Repos[0].Repo != "org/alpha" || res.Repos[1].Repo != "org/beta" {
		t.Fatalf("repo order wrong: %q, %q", res.Repos[0].Repo, res.Repos[1].Repo)
	}

	alpha := res.Repos[0]
	if alpha.Path != "/w/alpha" {
		t.Errorf("alpha path = %q, want /w/alpha", alpha.Path)
	}
	// maxSessions=2 keeps the two most recent by last_msg: s-a2 then s-a1; s-a3 dropped.
	if len(alpha.Sessions) != 2 {
		t.Fatalf("alpha: expected 2 sessions, got %d", len(alpha.Sessions))
	}
	if alpha.Sessions[0].SessionID != "s-a2" || alpha.Sessions[1].SessionID != "s-a1" {
		t.Fatalf("alpha session order wrong: %q, %q", alpha.Sessions[0].SessionID, alpha.Sessions[1].SessionID)
	}
	if alpha.Sessions[0].WorkType != "" || alpha.Sessions[1].WorkType != "branch-work" {
		t.Errorf("work_type mismatch: %q, %q", alpha.Sessions[0].WorkType, alpha.Sessions[1].WorkType)
	}
	if alpha.Sessions[1].Messages != 5 {
		t.Errorf("s-a1 messages = %d, want 5", alpha.Sessions[1].Messages)
	}

	// s-a1 has 5 messages; maxExcerpts=2 keeps the newest two (ids 4,5),
	// returned in ascending id order: "a2" then the long one (truncated).
	ex := alpha.Sessions[1].Excerpts
	if len(ex) != 2 {
		t.Fatalf("s-a1: expected 2 excerpts, got %d", len(ex))
	}
	if ex[0].Text != "a2" || ex[0].Truncated {
		t.Errorf("excerpt[0] = %q trunc=%v, want \"a2\" false", ex[0].Text, ex[0].Truncated)
	}
	if !ex[1].Truncated || ex[1].Text != longText[:10]+"..." {
		t.Errorf("excerpt[1] = %q trunc=%v, want %q true", ex[1].Text, ex[1].Truncated, longText[:10]+"...")
	}
	if ex[0].ID >= ex[1].ID {
		t.Errorf("excerpts not ascending by id: %d, %d", ex[0].ID, ex[1].ID)
	}

	// beta: single session, single short excerpt (not truncated).
	beta := res.Repos[1]
	if len(beta.Sessions) != 1 || len(beta.Sessions[0].Excerpts) != 1 {
		t.Fatalf("beta shape wrong: %d sessions", len(beta.Sessions))
	}
	if beta.Sessions[0].Excerpts[0].Text != "beta-only" {
		t.Errorf("beta excerpt = %q", beta.Sessions[0].Excerpts[0].Text)
	}
}

// TestStatusRepoFilter verifies the :repoPat sentinel — a filter restricts to
// matching repos, and an empty filter returns all.
func TestStatusRepoFilter(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedStatusSession(t, s, "s-a1", "org/alpha", "/w/alpha", "", "", "-1 minutes", []string{"a"})
	seedStatusSession(t, s, "s-b1", "org/beta", "/w/beta", "", "", "-1 minutes", []string{"b"})

	filtered, err := s.Status(7, "beta", 3, 3, 200)
	if err != nil {
		t.Fatalf("Status(beta): %v", err)
	}
	if len(filtered.Repos) != 1 || filtered.Repos[0].Repo != "org/beta" {
		t.Fatalf("repo filter failed: got %d repos", len(filtered.Repos))
	}

	all, err := s.Status(7, "", 3, 3, 200)
	if err != nil {
		t.Fatalf("Status(all): %v", err)
	}
	if len(all.Repos) != 2 {
		t.Fatalf("unfiltered: expected 2 repos, got %d", len(all.Repos))
	}
}
