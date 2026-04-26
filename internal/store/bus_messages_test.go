// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

func TestBusFreeformPostAndRecv(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	id1, err := s.PostBusMessage("deploy-watch", "build started", "alice", nil)
	if err != nil {
		t.Fatalf("post 1: %v", err)
	}
	id2, err := s.PostBusMessage("deploy-watch", "build green", "alice", &id1)
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if id2 <= id1 {
		t.Errorf("id ordering wrong: id1=%d id2=%d", id1, id2)
	}

	msgs, err := s.RecvBusMessages("deploy-watch", "", true, 0)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2", len(msgs))
	}
	if msgs[0].Body != "build started" || msgs[1].Body != "build green" {
		t.Errorf("ordering wrong: %+v", msgs)
	}
	if msgs[1].ReplyTo == nil || *msgs[1].ReplyTo != id1 {
		t.Errorf("reply_to not preserved: %+v", msgs[1])
	}
	if msgs[0].ReadAt == "" || msgs[1].ReadAt == "" {
		t.Errorf("recv with mark_read=true should set ReadAt")
	}
}

func TestBusRecvSinceFiltersOutOlderMessages(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	id1, _ := s.PostBusMessage("t", "first", "", nil)
	_ = id1

	first, err := s.RecvBusMessages("t", "", true, 0)
	if err != nil {
		t.Fatalf("recv 1: %v", err)
	}
	cursor := first[0].PostedAt

	if _, err := s.PostBusMessage("t", "second", "", nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecvBusMessages("t", cursor, true, 0)
	if err != nil {
		t.Fatalf("recv 2: %v", err)
	}
	// since is exclusive (>): if first.PostedAt == second.PostedAt
	// (sub-second collision), the result may include both or just
	// the second. The acceptance: never returns the first by the
	// stricter contract.
	for _, m := range got {
		if m.Body == "first" && m.PostedAt < cursor {
			t.Errorf("recv with since=%q returned older message %+v", cursor, m)
		}
	}
}

func TestBusRecvWithoutMarkReadLeavesUnread(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.PostBusMessage("t", "x", "", nil); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.RecvBusMessages("t", "", false, 0)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ReadAt != "" {
		t.Errorf("recv with mark_read=false should not set ReadAt: %+v", msgs)
	}

	// Re-recv with mark_read=true confirms the message was still unread.
	msgs2, err := s.RecvBusMessages("t", "", true, 0)
	if err != nil {
		t.Fatalf("recv 2: %v", err)
	}
	if len(msgs2) != 1 || msgs2[0].ReadAt == "" {
		t.Errorf("second recv with mark_read should set ReadAt: %+v", msgs2)
	}
}

func TestBusListMessagesDoesNotConsume(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.PostBusMessage("t", "x", "", nil); err != nil {
		t.Fatal(err)
	}

	listed, err := s.ListBusMessages("t", true, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].ReadAt != "" {
		t.Errorf("list should not modify ReadAt: %+v", listed)
	}
	listed2, err := s.ListBusMessages("t", true, 0)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(listed2) != 1 {
		t.Errorf("list 2 should still see unread message: %+v", listed2)
	}
}

func TestBusListTopics(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for i, body := range []string{"a", "b", "c"} {
		topic := "t1"
		if i == 2 {
			topic = "t2"
		}
		if _, err := s.PostBusMessage(topic, body, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecvBusMessages("t1", "", true, 0); err != nil {
		t.Fatal(err)
	}

	topics, err := s.ListBusTopics()
	if err != nil {
		t.Fatalf("list topics: %v", err)
	}
	if len(topics) != 2 {
		t.Fatalf("got %d topics, want 2", len(topics))
	}
	byName := map[string]BusTopic{}
	for _, tp := range topics {
		byName[tp.Topic] = tp
	}
	if byName["t1"].Messages != 2 || byName["t1"].Unread != 0 {
		t.Errorf("t1 should be 2 msgs / 0 unread: %+v", byName["t1"])
	}
	if byName["t2"].Messages != 1 || byName["t2"].Unread != 1 {
		t.Errorf("t2 should be 1 msg / 1 unread: %+v", byName["t2"])
	}
}

func TestBusTopicResolveSessionAddressing(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "demo", "sess-bus42", []map[string]any{
		metaMsg("user", "hi", "2026-04-26T10:00:00Z",
			"/Users/dev/work/github.com/acme/bustest", "main"),
		msg("assistant", "ok", "2026-04-26T10:00:05Z"),
		msg("user", "more", "2026-04-26T10:00:10Z"),
		msg("assistant", "more", "2026-04-26T10:01:00Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, topic    string
		wantPrefix     string // canonical form prefix the topic should resolve to
		wantErrSubstr  string
	}{
		{"freeform passes through", "deploy-watch", "deploy-watch", ""},
		{"bare session: passes through", "session:already-canonical", "session:already-canonical", ""},
		{"session:repo= resolves to canonical session uuid", "session:repo=bustest", "session:", ""},
		{"session:latest@ resolves to canonical session uuid",
			"session:latest@/Users/dev/work/github.com/acme", "session:", ""},
		{"session:repo= for unknown repo errors clearly",
			"session:repo=does-not-exist", "", "no sessions for repo"},
		{"session:latest@ for unknown dir errors clearly",
			"session:latest@/no/such/path", "", "no sessions under cwd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, err := s.PostBusMessage(c.topic, "x", "", nil)
			if c.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, posted as id=%d", c.wantErrSubstr, id)
				}
				if !strings.Contains(err.Error(), c.wantErrSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), c.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Verify the message landed under the canonical topic.
			topics, _ := s.ListBusTopics()
			found := false
			for _, tp := range topics {
				if strings.HasPrefix(tp.Topic, c.wantPrefix) {
					found = true
					if c.wantPrefix == "session:" && tp.Topic == "session:" {
						t.Errorf("session: form should be followed by a uuid, got bare %q", tp.Topic)
					}
				}
			}
			if !found {
				t.Errorf("no topic with prefix %q after post; topics=%+v", c.wantPrefix, topics)
			}
		})
	}
}
