// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package teampush

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// fakeIndex stands in for the local store so collection can be tested
// against exact content without an ingest pipeline.
type fakeIndex struct {
	candidates []store.TeamPushCandidate
	messages   map[string][]store.TeamPushMessage
}

func (f *fakeIndex) TeamPushCandidates(store.TeamPushFilter) ([]store.TeamPushCandidate, error) {
	return f.candidates, nil
}

func (f *fakeIndex) TeamPushMessages(id string) ([]store.TeamPushMessage, error) {
	return f.messages[id], nil
}

// gitRepo makes a directory look like a repo root to the collector.
func gitRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func collectOne(t *testing.T, idx *fakeIndex) []Collected {
	t.Helper()
	r, err := NewRedactor(RedactConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Collect(idx, CollectOptions{Redactor: r})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestCollectRedactsBeforeTransmission is the central privacy
// guarantee: nothing reaches a Session payload without passing the
// pipeline.
func TestCollectRedactsBeforeTransmission(t *testing.T) {
	cwd := gitRepo(t, "backend")
	idx := &fakeIndex{
		candidates: []store.TeamPushCandidate{
			{SessionID: "s1", Repo: "myorg/backend", Cwd: cwd, Source: "claude"},
		},
		messages: map[string][]store.TeamPushMessage{
			"s1": {
				{Role: "user", ContentType: "text", Text: "deploy with ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
				{Role: "assistant", ContentType: "text", Text: "reading /Users/alice/work/app/main.go"},
			},
		},
	}
	got := collectOne(t, idx)
	if len(got) != 1 || got[0].SkippedReason != "" {
		t.Fatalf("got %+v", got)
	}
	sess := got[0].Session
	for _, e := range sess.Entries {
		if strings.Contains(e.Text, "ghp_") {
			t.Errorf("a token reached the payload: %s", e.Text)
		}
		if strings.Contains(e.Text, "alice") {
			t.Errorf("a home path reached the payload: %s", e.Text)
		}
	}
	if sess.RedactionTally["github_token"] != 1 || sess.RedactionTally["home_path"] != 1 {
		t.Errorf("tally = %s, want one github_token and one home_path",
			Tally(sess.RedactionTally).Summary())
	}
}

// TestCollectRefusesWithoutARedactor: the one configuration that would
// ship raw transcripts must be impossible, not merely discouraged.
func TestCollectRefusesWithoutARedactor(t *testing.T) {
	_, err := Collect(&fakeIndex{}, CollectOptions{})
	if err == nil {
		t.Fatal("collection proceeded with no redactor configured")
	}
	if !strings.Contains(err.Error(), "unredacted") {
		t.Errorf("error should name the hazard: %v", err)
	}
}

// TestExcludeFileOptsOutARepo covers the proactive escape hatch.
func TestExcludeFileOptsOutARepo(t *testing.T) {
	cwd := gitRepo(t, "client-work")
	if err := os.WriteFile(filepath.Join(cwd, ExcludeFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &fakeIndex{
		candidates: []store.TeamPushCandidate{
			{SessionID: "s1", Repo: "myorg/client-work", Cwd: cwd},
		},
		messages: map[string][]store.TeamPushMessage{
			"s1": {{Role: "user", ContentType: "text", Text: "confidential client detail"}},
		},
	}
	got := collectOne(t, idx)
	if got[0].SkippedReason == "" {
		t.Fatal("excluded repo was collected for push")
	}
	if len(Pushable(got)) != 0 {
		t.Error("excluded repo appears in the pushable set")
	}
}

// TestExcludeFileAppliesFromASubdirectory: the marker lives at the repo
// root, but sessions routinely run deeper in the tree. A guard that
// only works from the top directory is one that silently fails in the
// common case.
func TestExcludeFileAppliesFromASubdirectory(t *testing.T) {
	root := gitRepo(t, "client-work")
	if err := os.WriteFile(filepath.Join(root, ExcludeFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := &fakeIndex{
		candidates: []store.TeamPushCandidate{
			{SessionID: "s1", Repo: "myorg/client-work", Cwd: deep},
		},
		messages: map[string][]store.TeamPushMessage{
			"s1": {{Role: "user", ContentType: "text", Text: "confidential"}},
		},
	}
	if collectOne(t, idx)[0].SkippedReason == "" {
		t.Error("exclusion did not apply to a session running below the repo root")
	}
}

// TestPrivateMarkerInClaudeMDOptsOut covers the second opt-out form.
func TestPrivateMarkerInClaudeMDOptsOut(t *testing.T) {
	cwd := gitRepo(t, "secret")
	md := "# Project\n\nNotes here.\n\n<!-- mnemo: private -->\n"
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &fakeIndex{
		candidates: []store.TeamPushCandidate{{SessionID: "s1", Repo: "myorg/secret", Cwd: cwd}},
		messages: map[string][]store.TeamPushMessage{
			"s1": {{Role: "user", ContentType: "text", Text: "text"}},
		},
	}
	got := collectOne(t, idx)
	if !strings.Contains(got[0].SkippedReason, PrivateMarker) {
		t.Errorf("private marker ignored: %q", got[0].SkippedReason)
	}
}

// TestOrdinaryRepoIsNotExcluded is the counterweight: the opt-out rules
// must not swallow everything. A collector that skips every repo is
// "safe" and useless.
func TestOrdinaryRepoIsNotExcluded(t *testing.T) {
	cwd := gitRepo(t, "backend")
	if err := os.WriteFile(filepath.Join(cwd, "CLAUDE.md"),
		[]byte("# Backend\n\nOrdinary instructions.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := &fakeIndex{
		candidates: []store.TeamPushCandidate{{SessionID: "s1", Repo: "myorg/backend", Cwd: cwd}},
		messages: map[string][]store.TeamPushMessage{
			"s1": {{Role: "user", ContentType: "text", Text: "ordinary work"}},
		},
	}
	got := collectOne(t, idx)
	if got[0].SkippedReason != "" {
		t.Errorf("ordinary repo was skipped: %q", got[0].SkippedReason)
	}
}

// TestCollectedPayloadCarriesNoLocalPaths: cwd is local metadata and
// has no place on the wire. It identifies the contributor's machine
// layout and no team query uses it.
func TestCollectedPayloadCarriesNoLocalPaths(t *testing.T) {
	cwd := gitRepo(t, "backend")
	idx := &fakeIndex{
		candidates: []store.TeamPushCandidate{
			{SessionID: "s1", Repo: "myorg/backend", Cwd: cwd, Topic: "auth"},
		},
		messages: map[string][]store.TeamPushMessage{
			"s1": {{Role: "user", ContentType: "text", Text: "work"}},
		},
	}
	got := collectOne(t, idx)
	if err := (&PushRequest{Version: ProtocolVersion, Sessions: Pushable(got)}).Validate(); err != nil {
		t.Fatalf("collected payload does not validate: %v", err)
	}
	// The Session type has no Cwd field at all; this asserts the
	// collector did not smuggle it into another one.
	sess := got[0].Session
	for _, field := range []string{sess.Topic, sess.GitBranch, sess.Project, sess.Repo} {
		if strings.Contains(field, cwd) {
			t.Errorf("local path %q leaked into the payload via %q", cwd, field)
		}
	}
}

func TestSessionWithNoContentIsSkipped(t *testing.T) {
	cwd := gitRepo(t, "backend")
	idx := &fakeIndex{
		candidates: []store.TeamPushCandidate{{SessionID: "s1", Repo: "myorg/backend", Cwd: cwd}},
		messages:   map[string][]store.TeamPushMessage{"s1": nil},
	}
	got := collectOne(t, idx)
	if got[0].SkippedReason == "" {
		t.Error("an empty session was offered for push")
	}
}

func TestPushURLDerivation(t *testing.T) {
	cases := map[string]string{
		"https://team.example:19421/mcp":  "https://team.example:19421/push",
		"https://team.example:19421/mcp/": "https://team.example:19421/push",
		"https://team.example:19421":      "https://team.example:19421/push",
		"https://team.example:19421/push": "https://team.example:19421/push",
	}
	for in, want := range cases {
		if got := PushURL(in); got != want {
			t.Errorf("PushURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestValidateRejectsRepolessSessions: push is repo-scoped, and a
// session with no repo would be unreachable by every team query.
func TestValidateRejectsRepolessSessions(t *testing.T) {
	req := PushRequest{Version: ProtocolVersion, Sessions: []Session{{
		SessionID: "s1",
		Entries:   []Entry{{Index: 0, Role: "user", ContentType: "text", Text: "x"}},
	}}}
	if err := req.Validate(); err == nil {
		t.Fatal("a session with no repo validated")
	}
}

func TestValidateRejectsUnknownProtocolVersion(t *testing.T) {
	req := PushRequest{Version: ProtocolVersion + 1}
	if err := req.Validate(); err == nil {
		t.Fatal("an unknown protocol version validated")
	}
}
