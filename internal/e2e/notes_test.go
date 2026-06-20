// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestNotesRoundTripViaMCP is 🎯T65 acceptance criterion #7 over the
// wire: a note posted to a directory inbox via mnemo_note_post is
// received via mnemo_note_recv through a live daemon's MCP transport,
// then consumed (a second unread recv is empty), and remains browsable
// via mnemo_note_list. Absolute inboxes are used so the round-trip needs
// no session-cwd fixtures (relative-path addressing is unit-tested in
// internal/store).
func TestNotesRoundTripViaMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e notes test skipped under -short")
	}
	d := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The inbox just has to be an existing directory the daemon can see;
	// the daemon's own MNEMO_HOME tempdir qualifies.
	inbox := d.Home
	const body = "mnemo v0.42 published, brew formula updated"

	if out, err := d.Call(ctx, "mnemo_note_post", map[string]any{
		"inbox": inbox,
		"body":  body,
	}); err != nil {
		t.Fatalf("mnemo_note_post: %v\n--- daemon log ---\n%s", err, d.Log())
	} else if !strings.Contains(out, "Posted note") {
		t.Errorf("note_post output unexpected: %s", out)
	}

	out, err := d.Call(ctx, "mnemo_note_recv", map[string]any{"inbox": inbox})
	if err != nil {
		t.Fatalf("mnemo_note_recv: %v\n--- daemon log ---\n%s", err, d.Log())
	}
	var got []struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("recv unmarshal: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Body != body {
		t.Fatalf("recv = %v, want one note %q", got, body)
	}

	// Consumed: a second unread recv returns the empty marker.
	if out, _ := d.Call(ctx, "mnemo_note_recv", map[string]any{"inbox": inbox}); out != "No notes." {
		t.Errorf("2nd recv = %q, want %q", out, "No notes.")
	}

	// Still browsable via list (read state preserved, not consumed).
	if out, err := d.Call(ctx, "mnemo_note_list", map[string]any{"inbox": inbox}); err != nil {
		t.Fatalf("mnemo_note_list: %v", err)
	} else if !strings.Contains(out, body) {
		t.Errorf("note_list missing the delivered note: %s", out)
	}
}
