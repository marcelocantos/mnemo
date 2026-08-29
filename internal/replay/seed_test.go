// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"strings"
	"testing"
	"time"
)

func TestStitchReadChunks(t *testing.T) {
	chunks := []readChunk{
		{offset: 1, text: "line1\nline2", ts: time.Now()},
		{offset: 3, text: "line3\nline4", ts: time.Now()},
	}
	body := stitchReadChunks(chunks)
	want := "line1\nline2\nline3\nline4"
	if string(body) != want {
		t.Fatalf("got %q want %q", body, want)
	}
}

func TestLooksLikeToolAckJSON(t *testing.T) {
	if !looksLikeToolAckJSON(`{"type":"SearchReplace","EditsApplied":{}}`) {
		t.Fatal("expected ack detection")
	}
	if looksLikeToolAckJSON(`/* css */\n.foo { color: red; }`) {
		t.Fatal("css should not look like ack")
	}
}

func TestRewindPathMatch(t *testing.T) {
	cands := []string{
		"/Users/me/jevons/ui/src/cockpit.css",
		"ui/src/cockpit.css",
	}
	if !rewindPathMatch("ui/src/cockpit.css", cands) {
		t.Fatal("expected match")
	}
	if rewindPathMatch("internal/fleet/fleet.go", cands) {
		t.Fatal("unexpected match")
	}
}

func TestStitchReadChunksOverlappingOffsets(t *testing.T) {
	// Later chunk with same/earlier offset still appends in input order —
	// callers must supply ascending slices from SQL ORDER BY.
	chunks := []readChunk{
		{offset: 1, text: "a\nb", ts: time.Unix(1, 0)},
		{offset: 1, text: "a\nb\nc", ts: time.Unix(2, 0)},
	}
	body := stitchReadChunks(chunks)
	if !strings.Contains(string(body), "c") {
		t.Fatalf("expected richer later chunk retained somehow, got %q", body)
	}
}

func TestLooksLikeToolAckJSONRejectsPlainJSONObject(t *testing.T) {
	// Ordinary file content that happens to be JSON must not be discarded
	// unless it looks like a SearchReplace ack.
	if looksLikeToolAckJSON(`{"name":"cockpit","theme":"dark"}`) {
		t.Fatal("generic JSON object must not be treated as tool ack")
	}
}
