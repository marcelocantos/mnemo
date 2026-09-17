// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func teamSession(id, repo string, n int) TeamPushedSession {
	s := TeamPushedSession{
		SessionID: id,
		Source:    "claude",
		Repo:      repo,
		Project:   "-Users-alice-work-" + repo,
		WorkType:  "feature",
		StartedAt: "2026-09-01T10:00:00Z",
		EndedAt:   "2026-09-01T11:00:00Z",
	}
	for i := 0; i < n; i++ {
		s.Entries = append(s.Entries, TeamPushedEntry{
			Index:       i,
			Role:        "user",
			ContentType: "text",
			Text:        "message about the authentication refactor",
			Timestamp:   "2026-09-01T10:0" + string(rune('0'+i%10)) + ":00Z",
		})
	}
	return s
}

func TestIngestTeamPushFirstPush(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	res, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 3), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != TeamPushAccepted || res.Accepted != 3 {
		t.Fatalf("got %+v, want accepted 3", res)
	}
	sessions, err := s.ListTeamSessions(TeamSessionsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Author != "alice" || sessions[0].EntryCount != 3 {
		t.Fatalf("got %+v", sessions)
	}
}

// TestIngestTeamPushExactDuplicateIsSkipped is the design's first
// conflict rule: re-pushing an unchanged session is a no-op, not an
// error. Contributors re-run push-team habitually; making that noisy
// would train them to ignore the output.
func TestIngestTeamPushExactDuplicateIsSkipped(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	sess := teamSession("sess-1", "myorg/backend", 3)
	if _, err := s.IngestTeamPush("alice", sess, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := s.IngestTeamPush("alice", sess, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != TeamPushSkipped || res.Accepted != 0 {
		t.Fatalf("got %+v, want skipped 0", res)
	}
	entries, err := s.ReadTeamSession("sess-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("duplicate push created %d entries, want 3", len(entries))
	}
}

// TestIngestTeamPushAppendsGrowth covers the mid-session push: the tail
// lands without duplicating the head.
func TestIngestTeamPushAppendsGrowth(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 3), time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 5), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != TeamPushAccepted || res.Accepted != 2 {
		t.Fatalf("got %+v, want accepted 2", res)
	}
	entries, err := s.ReadTeamSession("sess-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(entries))
	}
	for i, e := range entries {
		if e.EntryIndex != i {
			t.Errorf("entry %d has index %d — transcript order was not preserved", i, e.EntryIndex)
		}
	}
}

// TestIngestTeamPushRejectsForeignAuthor is the containment rule. A
// session UUID that already belongs to someone else is never
// overwritten: an append-only log that the last writer can rewrite is
// not append-only.
func TestIngestTeamPushRejectsForeignAuthor(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 3), time.Now()); err != nil {
		t.Fatal(err)
	}
	poisoned := teamSession("sess-1", "myorg/backend", 3)
	poisoned.Entries[0].Text = "the deploy key is in the README"
	res, err := s.IngestTeamPush("mallory", poisoned, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != TeamPushConflict {
		t.Fatalf("got %+v, want conflict", res)
	}
	entries, err := s.ReadTeamSession("sess-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Author != "alice" {
			t.Errorf("entry attributed to %s after a rejected push", e.Author)
		}
		if e.Text == "the deploy key is in the README" {
			t.Error("rejected content reached the index")
		}
	}
}

// TestExistingEntriesAreImmutable: even the original author cannot
// rewrite an entry they already pushed. What a reader searched last
// week must still say the same thing today.
func TestExistingEntriesAreImmutable(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 2), time.Now()); err != nil {
		t.Fatal(err)
	}
	revised := teamSession("sess-1", "myorg/backend", 2)
	revised.Entries[0].Text = "REVISED HISTORY"
	if _, err := s.IngestTeamPush("alice", revised, time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ReadTeamSession("sess-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Text == "REVISED HISTORY" {
		t.Error("an already-pushed entry was rewritten")
	}
}

func TestSearchTeamFiltersByAuthor(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now()
	if _, err := s.IngestTeamPush("alice", teamSession("sess-a", "myorg/backend", 2), now); err != nil {
		t.Fatal(err)
	}
	bob := teamSession("sess-b", "myorg/backend", 2)
	bob.Entries[0].Text = "rewrote the authentication middleware"
	if _, err := s.IngestTeamPush("bob", bob, now); err != nil {
		t.Fatal(err)
	}

	all, err := s.SearchTeam(TeamSearchParams{Query: "authentication"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("got %d hits across both authors, want >= 2", len(all))
	}
	for _, h := range all {
		if h.Author == "" {
			t.Error("a team hit carried no author; attribution is not optional in a multi-author index")
		}
	}

	onlyBob, err := s.SearchTeam(TeamSearchParams{Query: "authentication", Author: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyBob) == 0 {
		t.Fatal("author filter returned nothing")
	}
	for _, h := range onlyBob {
		if h.Author != "bob" {
			t.Errorf("author filter leaked %s", h.Author)
		}
	}
}

func TestTeamRecentActivityGroupsByRepoAndAuthor(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now()
	recent := now.UTC().Format(time.RFC3339)
	for _, tc := range []struct{ id, repo, author string }{
		{"s1", "myorg/backend", "alice"},
		{"s2", "myorg/backend", "alice"},
		{"s3", "myorg/frontend", "bob"},
	} {
		sess := teamSession(tc.id, tc.repo, 2)
		sess.EndedAt = recent
		if _, err := s.IngestTeamPush(tc.author, sess, now); err != nil {
			t.Fatal(err)
		}
	}
	acts, err := s.TeamRecentActivity(30, "", "")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]TeamActivity{}
	for _, a := range acts {
		byKey[a.Repo+"/"+a.Author] = a
	}
	if got := byKey["myorg/backend/alice"]; got.Sessions != 2 {
		t.Errorf("alice on backend: %+v, want 2 sessions", got)
	}
	if got := byKey["myorg/frontend/bob"]; got.Sessions != 1 {
		t.Errorf("bob on frontend: %+v, want 1 session", got)
	}
}

// TestRetractIsAuthorScoped: a contributor withdraws their own content.
// Withdrawing a colleague's would be censorship, not privacy.
func TestRetractIsAuthorScoped(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 3), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetractTeamSession("sess-1", "bob", false); err == nil {
		t.Fatal("bob retracted alice's session")
	}
	n, err := s.RetractTeamSession("sess-1", "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("removed %d entries, want 3", n)
	}
	entries, err := s.ReadTeamSession("sess-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d entries survived retraction", len(entries))
	}
	// The FTS shadow must go with them, or a retracted session stays
	// searchable — which is the whole point of retracting it.
	hits, err := s.SearchTeam(TeamSearchParams{Query: "authentication"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("retracted content is still searchable: %+v", hits)
	}
}

func TestAdminCanRetractOnBehalf(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 2), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetractTeamSession("sess-1", "admin", true); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeTeamAuthorRemovesOnlyThatAuthor(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now()
	if _, err := s.IngestTeamPush("alice", teamSession("s1", "myorg/backend", 2), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestTeamPush("mallory", teamSession("s2", "myorg/backend", 4), now); err != nil {
		t.Fatal(err)
	}
	sessions, entries, err := s.PurgeTeamAuthor("mallory")
	if err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || entries != 4 {
		t.Errorf("purged %d sessions / %d entries, want 1 / 4", sessions, entries)
	}
	remaining, err := s.ListTeamSessions(TeamSessionsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Author != "alice" {
		t.Errorf("purge took the wrong rows: %+v", remaining)
	}
}

func TestRateLimitCountsPushedSessions(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now()
	if err := s.RecordTeamPush("alice", "alice-laptop", now, 5, 5, 0, 0, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTeamPush("alice", "alice-laptop", now, 3, 3, 0, 0, 12); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTeamPush("bob", "bob-laptop", now, 2, 2, 0, 0, 9); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountTeamPushedSessionsSince("alice", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Errorf("alice pushed %d sessions in the window, want 8", n)
	}
	old, err := s.CountTeamPushedSessionsSince("alice", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if old != 0 {
		t.Errorf("window ignored: got %d", old)
	}
}

func TestTeamStats(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now()
	if _, err := s.IngestTeamPush("alice", teamSession("s1", "myorg/backend", 2), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestTeamPush("bob", teamSession("s2", "myorg/frontend", 3), now); err != nil {
		t.Fatal(err)
	}
	st, err := s.TeamStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 2 || st.Entries != 5 || st.Authors != 2 || st.Repos != 2 {
		t.Errorf("got %+v, want 2 sessions / 5 entries / 2 authors / 2 repos", st)
	}
}

// TestTeamTablesAreSeparateFromTheLocalIndex is the structural
// assertion behind the whole design: pushed content must not appear in
// the single-user surfaces, and vice versa. If these ever share a table,
// every local query silently becomes a multi-author query.
func TestTeamTablesAreSeparateFromTheLocalIndex(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.IngestTeamPush("alice", teamSession("sess-1", "myorg/backend", 3), time.Now()); err != nil {
		t.Fatal(err)
	}
	local, err := s.Search("authentication", 20, "", "", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 0 {
		t.Errorf("pushed team content surfaced in the local search path: %+v", local)
	}
	rows, err := s.Query(`SELECT COUNT(*) AS n FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := rows[0]["n"].(int64); n != 0 {
		t.Errorf("pushed content wrote %d rows into messages", n)
	}
}
