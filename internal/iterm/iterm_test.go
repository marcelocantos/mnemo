// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package iterm

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

func TestShellDoubleQuote(t *testing.T) {
	cases := map[string]string{
		"/a/b":        `"/a/b"`,
		"/has space":  `"/has space"`,
		`/a"b`:        `"/a\"b"`,
		"/a$b":        `"/a\$b"`,
		"~/think/x-1": `"~/think/x-1"`,
		"/a\\b":       `"/a\\b"`,
	}
	for in, want := range cases {
		if got := shellDoubleQuote(in); got != want {
			t.Errorf("shellDoubleQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginCommandHasNoBareSingleQuotesInScript(t *testing.T) {
	// The script is wrapped in single quotes for iTerm2's command tokenizer,
	// so spawnCommand itself must contain no single quotes.
	for _, args := range []GoArgs{
		{Path: "/Users/x/think/threads/demo", Name: "demo"},
		{Path: "/p", Name: "d", NoResume: true},
	} {
		if strings.Contains(spawnCommand(args), "'") {
			t.Errorf("spawnCommand must not contain single quotes: %s", spawnCommand(args))
		}
	}
	lc := loginCommand(GoArgs{Path: "/p", Name: "d"})
	if !strings.HasPrefix(lc, `/bin/zsh -lc '`) || !strings.HasSuffix(lc, `'`) {
		t.Errorf("loginCommand should be a single-quoted /bin/zsh -lc wrapper: %s", lc)
	}
}

func TestSpawnScriptUsesCommandParam(t *testing.T) {
	s := strings.Join(spawnScript(GoArgs{Path: "/p", Name: "d"}, "tag"), "\n")
	if !strings.Contains(s, "create tab with profile \"Thread\" command cmd") {
		t.Errorf("spawn should pass the command parameter, not write text:\n%s", s)
	}
	if strings.Contains(s, "write text") {
		t.Errorf("spawn should no longer use write text:\n%s", s)
	}
}

func TestOsaEscape(t *testing.T) {
	if got := osaEscape(`a"b\c`); got != `a\"b\\c` {
		t.Errorf("osaEscape = %q", got)
	}
}

func TestSpawnCommandTagged(t *testing.T) {
	cmd := spawnCommand(GoArgs{Path: "/Users/x/think/threads/demo", Name: "demo"})
	badge := base64.StdEncoding.EncodeToString([]byte("🪡 demo"))
	for _, want := range []string{
		`cd "/Users/x/think/threads/demo"`,
		`SetBadgeFormat`,
		badge, // badge text is base64-encoded in Go, not shelled out
		`claude --continue 2>/dev/null || claude`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("spawnCommand missing %q in:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "exec ") {
		t.Errorf("spawnCommand should run claude directly, not exec a shell:\n%s", cmd)
	}
}

func TestSpawnCommandNoResume(t *testing.T) {
	cmd := spawnCommand(GoArgs{Path: "/p", Name: "demo", NoResume: true})
	if strings.Contains(cmd, "SetBadgeFormat") {
		t.Errorf("no-resume spawn should not set a badge: %s", cmd)
	}
	if cmd != `cd "/p" && claude` {
		t.Errorf("no-resume spawn = %q, want plain claude in the thread dir", cmd)
	}
}

func TestSpawnScriptTaggingGating(t *testing.T) {
	tag := base64.StdEncoding.EncodeToString([]byte("/p"))
	tagged := strings.Join(spawnScript(GoArgs{Path: "/p", Name: "d"}, tag), "\n")
	if !strings.Contains(tagged, `tell s to set variable named "user.thread" to "`+tag+`"`) {
		t.Errorf("tagged spawn must set user.thread")
	}
	untagged := strings.Join(spawnScript(GoArgs{Path: "/p", Name: "d", NoResume: true}, tag), "\n")
	if strings.Contains(untagged, "set variable named") {
		t.Errorf("no-resume spawn must not tag the tab")
	}
}

func TestGoFocusesExistingTab(t *testing.T) {
	var scripts [][]string
	restore := runner
	defer func() { runner = restore }()
	runner = func(_ context.Context, lines []string) (string, error) {
		scripts = append(scripts, lines)
		return "found\n", nil // first (find) call reports a match
	}

	res, err := Go(context.Background(), GoArgs{Path: "/Users/x/threads/demo", Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Focused {
		t.Errorf("action = %q, want focused", res.Action)
	}
	if len(scripts) != 1 {
		t.Errorf("expected only the find script to run, got %d scripts", len(scripts))
	}
	// The find script must compare against the base64 of the path.
	tag := base64.StdEncoding.EncodeToString([]byte("/Users/x/threads/demo"))
	if !strings.Contains(strings.Join(scripts[0], "\n"), tag) {
		t.Errorf("find script does not reference the path tag")
	}
}

func TestGoSpawnsWhenNotFound(t *testing.T) {
	var calls int
	restore := runner
	defer func() { runner = restore }()
	runner = func(_ context.Context, _ []string) (string, error) {
		calls++
		if calls == 1 {
			return "notfound", nil // find misses
		}
		return "spawned", nil // spawn succeeds
	}
	res, err := Go(context.Background(), GoArgs{Path: "/p", Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Spawned {
		t.Errorf("action = %q, want spawned", res.Action)
	}
	if calls != 2 {
		t.Errorf("expected find then spawn (2 calls), got %d", calls)
	}
}

func TestGoNoResumeSkipsFind(t *testing.T) {
	var calls int
	restore := runner
	defer func() { runner = restore }()
	runner = func(_ context.Context, _ []string) (string, error) {
		calls++
		return "spawned", nil
	}
	if _, err := Go(context.Background(), GoArgs{Path: "/p", Name: "d", NoResume: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("no-resume should skip find and only spawn; got %d calls", calls)
	}
}

func TestGoRejectsEmptyPath(t *testing.T) {
	if _, err := Go(context.Background(), GoArgs{}); err == nil {
		t.Error("expected error for empty path")
	}
}
