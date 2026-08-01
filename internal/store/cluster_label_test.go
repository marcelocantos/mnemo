// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// longNote builds a vault_user note body of n words containing the given
// keyword so title-coherence and min-token gates can be exercised.
func longNote(keyword string, n int) string {
	return keyword + " " + strings.TrimSpace(strings.Repeat("filler ", n))
}

func TestUserAnchorLabelPasses(t *testing.T) {
	docs := []ClusterCorpusDoc{
		corpusDoc("d1", "decision", "r", "schema migration decision text", 1.0),
		{DocID: "vault_user:/vault/topics/Schema Migration.md", Kind: "vault_user",
			EntityID: "/vault/topics/Schema Migration.md",
			Text:     longNote("schema migration", 250), Weight: 1.5},
	}
	members := []int{0, 1}
	// vault_user is the centroid-closest (give it sim 0.9 vs 0.5).
	simTo := map[int]float64{0: 0.5, 1: 0.9}
	got := userAnchorLabel(docs, members, simTo, "schema migration centroid", 200, nil)
	if got.Label != "Schema Migration" {
		t.Errorf("want user-anchored label, got %+v", got)
	}
}

func TestUserAnchorLabelGates(t *testing.T) {
	base := func(path, body string) []ClusterCorpusDoc {
		return []ClusterCorpusDoc{
			corpusDoc("d1", "decision", "r", "schema migration decision", 1.0),
			{DocID: "vault_user:" + path, Kind: "vault_user", EntityID: path, Text: body, Weight: 1.5},
		}
	}
	members := []int{0, 1}
	simTo := map[int]float64{0: 0.4, 1: 0.9}

	// Gate 3: daily-note filename.
	r := userAnchorLabel(base("/vault/daily/2026-01-01.md", longNote("schema", 250)), members, simTo, "schema centroid", 200, nil)
	if r.Label != "" || !strings.Contains(r.RejectNote, "filename") {
		t.Errorf("daily-note should be rejected on filename: %+v", r)
	}

	// Gate 2: too-short body.
	r = userAnchorLabel(base("/vault/topics/Schema.md", longNote("schema", 20)), members, simTo, "schema centroid", 200, nil)
	if r.Label != "" || !strings.Contains(r.RejectNote, "token") {
		t.Errorf("short note should be rejected on body length: %+v", r)
	}

	// Gate 4: title shares no token with centroid.
	r = userAnchorLabel(base("/vault/topics/Zzz.md", longNote("zzz", 250)), members, simTo, "schema migration centroid", 200, nil)
	if r.Label != "" || !strings.Contains(r.RejectNote, "token") {
		t.Errorf("incoherent title should be rejected: %+v", r)
	}

	// No vault_user member → silent (empty label, empty note).
	only := []ClusterCorpusDoc{corpusDoc("d1", "decision", "r", "schema migration", 1.0)}
	r = userAnchorLabel(only, []int{0}, map[int]float64{0: 1}, "schema", 200, nil)
	if r.Label != "" || r.RejectNote != "" {
		t.Errorf("no vault_user member should be silent: %+v", r)
	}
}

func TestFilenameExcluded(t *testing.T) {
	cases := map[string]bool{
		"/v/2026-01-01.md":         true,
		"/v/journal.md":            true,
		"/v/daily/anything.md":     true,
		"/v/inbox/note.md":         true,
		"/v/topics/Auth Design.md": false,
		"/v/schema-migration.md":   false,
	}
	for path, want := range cases {
		if got := filenameExcluded(path, nil); got != want {
			t.Errorf("filenameExcluded(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestNoteTitle(t *testing.T) {
	if got := noteTitle("/vault/topics/schema-migration.md"); got != "Schema Migration" {
		t.Errorf("noteTitle = %q", got)
	}
}
