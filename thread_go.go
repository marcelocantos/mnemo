// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/threads"
)

// threadGo opens a thread's iTerm2 tab by delegating to the running daemon's
// POST /api/thread/go (🎯T85.2, Integration §0.3). The daemon owns the single
// iTerm2 Automation grant, so the CLI never drives iTerm2 itself — it would
// otherwise carry the terminal's TCC identity and prompt separately.
func threadGo(m *threads.Manager, ref string, noResume bool) {
	// Resolve locally first so a bad name fails fast with a clear message
	// before we involve the daemon.
	if _, err := m.Resolve(ref); err != nil {
		fmt.Fprintf(os.Stderr, "thread go: %v\n", err)
		os.Exit(1)
	}

	endpoint := daemonBaseURL() + "/api/thread/go"
	form := url.Values{"name": {ref}}
	if noResume {
		form.Set("no_resume", "1")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.PostForm(endpoint, form)
	if err != nil {
		fmt.Fprintf(os.Stderr, "thread go: cannot reach the mnemo daemon at %s: %v\n", daemonBaseURL(), err)
		fmt.Fprintln(os.Stderr, "is it running? start it with `brew services start mnemo`.")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "thread go: daemon error (%s): %s\n", resp.Status, strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	var res struct {
		Action string `json:"action"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(body, &res); err == nil && res.Action != "" {
		fmt.Printf("%s tab for %s\n", res.Action, res.Path)
		return
	}
	fmt.Println(strings.TrimSpace(string(body)))
}

// daemonBaseURL returns the base URL of the local daemon. $MNEMO_ADDR
// overrides the default listen address (:19419); a bare ":port" or
// "host:port" is normalised to a localhost http URL.
func daemonBaseURL() string {
	addr := os.Getenv("MNEMO_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/")
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}
