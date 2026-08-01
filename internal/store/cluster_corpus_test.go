// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
	"time"
)

// wordsOfLength builds a body of n whitespace tokens so token-count
// gates can be exercised precisely.
func wordsOfLength(n int) string {
	return strings.TrimSpace(strings.Repeat("lorem ", n))
}

func findCorpusDoc(docs []ClusterCorpusDoc, docID string) (ClusterCorpusDoc, bool) {
	for _, d := range docs {
		if d.DocID == docID {
			return d, true
		}
	}
	return ClusterCorpusDoc{}, false
}

// --- decision stream ---------------------------------------------------

func TestDecisionIsHighSignal(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	old := now.Add(-14 * 24 * time.Hour)
	fresh := now.Add(-2 * 24 * time.Hour)
	rationale := wordsOfLength(30)

	cases := []struct {
		name string
		c    decisionCandidate
		want bool
	}{
		{"confirmed old with rationale", decisionCandidate{
			ProposalText: rationale, ConfirmationText: "agreed, let's do it this way", FirstSeen: old, Now: now}, true},
		{"unconfirmed", decisionCandidate{
			ProposalText: rationale, ConfirmationText: "", FirstSeen: old, Now: now}, false},
		{"terse rationale", decisionCandidate{
			ProposalText: "do X?", ConfirmationText: "yes", FirstSeen: old, Now: now}, false},
		{"too recent", decisionCandidate{
			ProposalText: rationale, ConfirmationText: "agreed, let's do it this way", FirstSeen: fresh, Now: now}, false},
		{"undated fails closed", decisionCandidate{
			ProposalText: rationale, ConfirmationText: "agreed, let's do it this way", FirstSeen: time.Time{}, Now: now}, false},
	}
	for _, tc := range cases {
		if got := decisionIsHighSignal(tc.c); got != tc.want {
			t.Errorf("%s: decisionIsHighSignal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDecisionCorpusDocs(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	fresh := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	longProposal := "we should migrate the schema because " + wordsOfLength(30)

	insert := func(id int, proposal, confirmation, repo, ts string) {
		t.Helper()
		if _, err := s.writeDB.Exec(
			`INSERT INTO decisions (id, session_id, proposal_text, confirmation_text, repo, timestamp)
			 VALUES (?, 'sess', ?, ?, ?, ?)`,
			id, proposal, confirmation, repo, ts); err != nil {
			t.Fatal(err)
		}
	}
	insert(1, longProposal, "confirmed, that is the right call for us", "mnemo", old)   // included
	insert(2, longProposal, "", "mnemo", old)                                           // unconfirmed
	insert(3, "do it?", "yes", "mnemo", old)                                            // terse
	insert(4, longProposal, "confirmed, that is the right call for us", "mnemo", fresh) // too recent

	docs, err := s.DecisionCorpusDocs()
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 decision doc, got %d: %+v", len(docs), docs)
	}
	d := docs[0]
	if d.DocID != "decision:1" || d.Kind != "decision" || d.Repo != "mnemo" {
		t.Errorf("unexpected doc: %+v", d)
	}
	if d.Weight != DecisionStreamWeight {
		t.Errorf("weight = %v, want %v", d.Weight, DecisionStreamWeight)
	}
	if !strings.Contains(d.Text, "migrate the schema") {
		t.Errorf("text missing proposal content: %q", d.Text)
	}
}

// --- compaction stream -------------------------------------------------

func TestCompactionCorpusDocs(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, repo) VALUES ('sess', 'mnemo')`); err != nil {
		t.Fatal(err)
	}
	insert := func(id int, summary, payload string) {
		t.Helper()
		if _, err := s.writeDB.Exec(
			`INSERT INTO compactions (id, session_id, summary, payload_json, generated_at)
			 VALUES (?, 'sess', ?, ?, '2026-07-01T00:00:00Z')`,
			id, summary, payload); err != nil {
			t.Fatal(err)
		}
	}
	insert(1, "worked on the clustering engine", `{"targets_active":["T64.8"]}`)       // included
	insert(2, "single message then clear", `{}`)                                       // empty → excluded
	insert(3, "progressed a target", `{"targets_progressed":{"T64.8":"corpus done"}}`) // included via progressed
	insert(4, "", `{"targets_active":["T1"],"summary":"payload fallback prose"}`)      // summary fallback

	docs, err := s.CompactionCorpusDocs()
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Fatalf("want 3 compaction docs, got %d: %+v", len(docs), docs)
	}
	d1, ok := findCorpusDoc(docs, "compaction:1")
	if !ok {
		t.Fatal("compaction:1 missing")
	}
	if d1.Repo != "mnemo" || d1.Weight != CompactionStreamWeight || d1.Kind != "compaction" {
		t.Errorf("unexpected compaction:1 %+v", d1)
	}
	if _, ok := findCorpusDoc(docs, "compaction:2"); ok {
		t.Error("empty-payload compaction should be excluded")
	}
	d4, ok := findCorpusDoc(docs, "compaction:4")
	if !ok || d4.Text != "payload fallback prose" {
		t.Errorf("summary fallback failed: %+v", d4)
	}
}

// --- vault_user stream -------------------------------------------------

func TestVaultUserCorpusDocs(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	root := "/vault"

	insert := func(path, content string) {
		t.Helper()
		if _, err := s.writeDB.Exec(
			`INSERT INTO docs (repo, file_path, kind, content, mtime, doc_date, indexed_at)
			 VALUES ('vault', ?, 'vault', ?, '2026-07-01', '2026-07-01', '2026-07-01T00:00:00Z')`,
			path, content); err != nil {
			t.Fatal(err)
		}
	}
	longBody := "---\ntype: note\n---\n" + wordsOfLength(150)
	insert("/vault/topics/auth.md", longBody)               // included
	insert("/vault/_mnemo/themes/x.md", longBody)           // under _mnemo → excluded
	insert("/vault/topics/stub.md", "---\nx: 1\n---\ntiny") // < 100 tokens → excluded

	docs, err := s.VaultUserCorpusDocs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 vault_user doc, got %d: %+v", len(docs), docs)
	}
	d := docs[0]
	if d.DocID != "vault_user:/vault/topics/auth.md" || d.Weight != VaultUserStreamWeight {
		t.Errorf("unexpected vault_user doc: %+v", d)
	}
	if strings.Contains(d.Text, "type: note") {
		t.Errorf("frontmatter leaked into text: %q", d.Text)
	}

	// Empty vaultRoot omits the stream entirely.
	empty, err := s.VaultUserCorpusDocs("")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("empty vaultRoot should yield no docs, got %d", len(empty))
	}
}

// --- aggregator + helpers ----------------------------------------------

func TestClusterCorpusMergesStreams(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.writeDB.Exec(
		`INSERT INTO decisions (id, session_id, proposal_text, confirmation_text, repo, timestamp)
		 VALUES (1, 'sess', ?, 'confirmed, that is right for the project', 'mnemo', ?)`,
		"we should migrate because "+wordsOfLength(30), old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, repo) VALUES ('sess', 'mnemo')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO compactions (id, session_id, summary, payload_json, generated_at)
		 VALUES (1, 'sess', 'did work', '{"targets_active":["T1"]}', '2026-07-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	docs, err := s.ClusterCorpus("")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("want 2 merged docs (1 decision + 1 compaction), got %d: %+v", len(docs), docs)
	}
	// Stable order: decision precedes compaction.
	if docs[0].Kind != "decision" || docs[1].Kind != "compaction" {
		t.Errorf("stream order not stable: %s, %s", docs[0].Kind, docs[1].Kind)
	}
}

func TestStripLeadingFrontmatter(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"with frontmatter", "---\ntype: x\ntags: [a]\n---\nbody here", "body here"},
		{"no frontmatter", "just body", "just body"},
		{"bom prefix", "\uFEFF---\nk: v\n---\nafter", "after"},
		{"triple-dash mid-body only", "body --- still body", "body --- still body"},
	}
	for _, tc := range cases {
		if got := stripLeadingFrontmatter(tc.in); got != tc.want {
			t.Errorf("%s: stripLeadingFrontmatter(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestFirstNWords(t *testing.T) {
	if got := firstNWords("a b c d e", 3); got != "a b c" {
		t.Errorf("firstNWords truncate = %q", got)
	}
	if got := firstNWords("  a   b  ", 10); got != "a b" {
		t.Errorf("firstNWords normalise = %q", got)
	}
}
