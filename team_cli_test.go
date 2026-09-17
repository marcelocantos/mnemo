// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestWriteTeamInstanceConfigPreservesOtherKeys is the property that
// matters about onboarding: it edits one block of a file the user owns.
//
// Round-tripping through the Config struct would look cleaner and would
// silently drop every key this binary does not know — which, given the
// file is hand-edited and the struct grows release by release, means an
// older mnemo onboarding a contributor could quietly delete their vault
// or pricing configuration.
func TestWriteTeamInstanceConfigPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{
  "vault_path": "~/vault",
  "menu_bar_app": true,
  "a_key_this_binary_has_never_heard_of": {"nested": [1, 2, 3]}
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeTeamInstanceConfig(dir, store.TeamInstance{
		Name: "acme", URL: "https://team.example:19421/mcp",
		PeerCert: "team-mnemo", Author: "alice",
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["vault_path"] != "~/vault" {
		t.Errorf("vault_path lost: %v", got["vault_path"])
	}
	if got["menu_bar_app"] != true {
		t.Errorf("menu_bar_app lost: %v", got["menu_bar_app"])
	}
	if _, ok := got["a_key_this_binary_has_never_heard_of"]; !ok {
		t.Error("an unrecognised key was dropped; onboarding must not rewrite keys it does not know")
	}
	ti, ok := got["team_instance"].(map[string]any)
	if !ok {
		t.Fatalf("team_instance not written: %v", got["team_instance"])
	}
	if ti["name"] != "acme" || ti["url"] != "https://team.example:19421/mcp" ||
		ti["peer_cert"] != "team-mnemo" || ti["author"] != "alice" {
		t.Errorf("team_instance = %v", ti)
	}
}

// TestWriteTeamInstanceConfigRefusesMalformedConfig: rewriting a file
// we could not parse would destroy whatever the user had in it.
func TestWriteTeamInstanceConfigRefusesMalformedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"vault_path": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeTeamInstanceConfig(dir, store.TeamInstance{Name: "acme"}); err == nil {
		t.Fatal("onboarding overwrote an unparseable config")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"vault_path": ` {
		t.Errorf("the user's file was modified: %q", raw)
	}
}

func TestWriteTeamInstanceConfigCreatesAFreshFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeTeamInstanceConfig(dir, store.TeamInstance{
		Name: "acme", URL: "https://team.example/mcp", PeerCert: "team-mnemo",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	ti := got["team_instance"].(map[string]any)
	if _, hasAuthor := ti["author"]; hasAuthor {
		t.Error("an empty author was written; it should be omitted so the server's name applies")
	}
}

func TestNormaliseSince(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"2026-09-01":           "2026-09-01T00:00:00Z",
		"2026-09-01T10:00:00Z": "2026-09-01T10:00:00Z",
	}
	for in, want := range cases {
		if got := normaliseSince(in); got != want {
			t.Errorf("normaliseSince(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGitHooksDirHonoursCoreHooksPath: a hook written to .git/hooks
// when core.hooksPath points elsewhere is a hook that never runs, and
// nothing about the install output would say so.
func TestGitHooksDirHonoursCoreHooksPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	run("config", "core.hooksPath", "my-hooks")

	// gitHooksDir shells out to git in the PROCESS working directory,
	// so run it from the repo.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	dir, err := gitHooksDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "my-hooks" {
		t.Errorf("hooks dir = %q, want the configured core.hooksPath", dir)
	}
}

// TestTeamFileShapeIsWhatOnboardTeamReads pins the repo-level config
// contract, since the file is checked into VCS by teams and read by
// contributors on clone.
func TestTeamFileShapeIsWhatOnboardTeamReads(t *testing.T) {
	raw := `{
  "name": "acme",
  "url": "https://mnemo.acme.example:19421/mcp",
  "peer_cert_pem": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
}`
	var tf TeamFile
	if err := json.Unmarshal([]byte(raw), &tf); err != nil {
		t.Fatal(err)
	}
	if tf.Name != "acme" || tf.URL == "" || !strings.Contains(tf.PeerCertPEM, "BEGIN CERTIFICATE") {
		t.Errorf("parsed %+v", tf)
	}
}
