// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// cmdResume implements `mnemo resume [<ref>]` — reopen a past conversation
// without knowing its session id (🎯T125).
//
// The MCP tool covers the case where an agent is already running and can be
// asked. This covers the other half of the premise: wanting to pick a
// conversation back up from a shell, with nothing running to ask.
//
// Like `mnemo thread go`, it delegates to the daemon rather than resolving
// and opening locally. Two reasons, both load-bearing:
//
//   - The daemon holds the single iTerm2 Automation TCC grant. A CLI that
//     drove iTerm2 itself would carry the invoking terminal's identity and
//     prompt for a second, separate grant.
//   - It keeps this command away from the store entirely, so reopening a
//     conversation can never trigger a schema migration or a multi-minute
//     pre-migration backup against a database the daemon is holding open.
//
// resumeTimeout is deliberately longer than the daemon's own automation
// bound, so a wedged terminal surfaces as the daemon's clear error rather
// than as a client-side timeout with no explanation.
const resumeTimeout = 45 * time.Second

func cmdResume(args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo resume [<ref>]

Reopen a past conversation in the directory it ran in.

  <ref>   which session to reopen:
            (omitted)         the most recent session
            latest | recent   the same
            latest:<scope>    newest in a matching repo or project
            <id or prefix>    that session; an exact id always wins
            <repo fragment>   newest session whose repo/project matches

Examples:
  mnemo resume                  # pick up where you left off
  mnemo resume mnemo            # newest mnemo session
  mnemo resume latest:yourworld
  mnemo resume 84369401
`)
	}
	_ = fs.Parse(args)

	// Everything after the subcommand is the reference, joined so
	// `mnemo resume latest mnemo` works as spoken.
	ref := strings.TrimSpace(strings.Join(fs.Args(), " "))

	endpoint := daemonBaseURL() + "/api/session/go"
	client := &http.Client{Timeout: resumeTimeout}
	resp, err := client.PostForm(endpoint, url.Values{"session": {ref}})
	if err != nil {
		// A timeout is NOT the same as an unreachable daemon, and saying
		// so sends people to `brew services` when the daemon is fine. The
		// usual cause is the terminal automation blocking — most often
		// because iTerm2 is not running and osascript is stuck trying to
		// launch it.
		if ue, ok := err.(*url.Error); ok && ue.Timeout() {
			fmt.Fprintf(os.Stderr,
				"resume: the mnemo daemon accepted the request but did not answer within %s.\n",
				resumeTimeout)
			fmt.Fprintln(os.Stderr,
				"the daemon is probably fine — check `curl -s -o /dev/null -w '%{http_code}' "+
					"http://localhost:19419/health`.")
			fmt.Fprintln(os.Stderr,
				"the usual cause is terminal automation: make sure iTerm2 is running, and that "+
					"mnemo has Automation permission for it (System Settings > Privacy & Security > Automation).")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "resume: cannot reach the mnemo daemon at %s: %v\n", daemonBaseURL(), err)
		fmt.Fprintln(os.Stderr, "is it running? start it with `brew services start mnemo`.")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "resume: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	var res struct {
		Action    string `json:"action"`
		Path      string `json:"path"`
		SessionID string `json:"session_id"`
		Repo      string `json:"repo"`
		Topic     string `json:"topic"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Action == "" {
		fmt.Println(strings.TrimSpace(string(body)))
		return
	}
	// Name the session that was chosen. The whole point is that the caller
	// did not specify one exactly, so they need to see which it picked.
	where := res.Repo
	if where == "" {
		where = res.Path
	}
	fmt.Printf("%s tab for %s (%s)\n", res.Action, where, res.SessionID)
	if res.Topic != "" {
		fmt.Printf("  %s\n", res.Topic)
	}
}
