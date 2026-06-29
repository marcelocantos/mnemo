// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/threads"
)

// cmdThread implements `mnemo thread <sub> ...`. The default sub-command is
// `list`. The thread model is a live filesystem projection rooted at the
// configured threads root (default ~/think/threads); see 🎯T85 and
// docs/design/threads-navigator.md.
func cmdThread(args []string) {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}

	m, err := newThreadManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "thread: %v\n", err)
		os.Exit(1)
	}

	switch sub {
	case "list":
		threadList(m, rest)
	case "new":
		threadNew(m, rest)
	case "show":
		threadShow(m, rest)
	case "archive":
		threadArchive(m, rest)
	case "go":
		threadGoCmd(m, rest)
	default:
		fmt.Fprintf(os.Stderr, "thread: unknown sub-command %q (want list|new|show|archive|go)\n", sub)
		os.Exit(1)
	}
}

// newThreadManager builds the Manager from the live config + home, matching
// the MCP and HTTP adapters so all three see the same threads root.
func newThreadManager() (*threads.Manager, error) {
	home, err := store.EffectiveHome()
	if err != nil {
		return nil, err
	}
	cfg, err := store.LoadConfig()
	if err != nil {
		return nil, err
	}
	return &threads.Manager{Root: cfg.ResolvedThreadsRoot(), Home: home}, nil
}

func threadList(m *threads.Manager, args []string) {
	fs := flag.NewFlagSet("thread list", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose: show status and current focus")
	fs.BoolVar(verbose, "verbose", false, "verbose: show status and current focus")
	_ = fs.Parse(args)

	ts, err := m.ListSorted()
	if err != nil {
		fmt.Fprintf(os.Stderr, "thread list: %v\n", err)
		os.Exit(1)
	}
	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if *verbose {
		for _, t := range ts {
			fmt.Fprintf(tw, "%s\t%s\tfocus: %s\t(%s)\n",
				displayName(t), truncate(t.Status, 50), truncate(t.Focus, 80), t.ActivitySummary(now))
		}
		_ = tw.Flush()
		return
	}
	for _, t := range ts {
		state := t.State
		if state == "" {
			state = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", displayName(t), state, t.ActivitySummary(now))
	}
	_ = tw.Flush()
	fmt.Printf("\n%d threads in %s\n", len(ts), tildeAbbrev(m))
	fmt.Println("Run `mnemo thread show <name>` for detail.")
}

func threadShow(m *threads.Manager, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "thread show: name required")
		os.Exit(1)
	}
	name := args[0]
	if _, err := m.Get(name); err != nil {
		fmt.Fprintf(os.Stderr, "thread show: %v\n", err)
		// Fallback: list known threads to help the user pick.
		if ts, lerr := m.ListSorted(); lerr == nil {
			for _, t := range ts {
				fmt.Fprintf(os.Stderr, "  %s\n", t.Name)
			}
		}
		os.Exit(1)
	}
	if body, err := m.ReadContext(name); err == nil {
		showMarkdownCLI(os.Stdout, body)
	}

	files, err := m.Files(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "thread show: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n--- Files ---")
	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, f := range files {
		name := f.Name
		size := fmt.Sprintf("%d", f.Size)
		if f.IsDir {
			name += "/"
			size = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, size, threads.RelativeTime(now, f.ModTime))
	}
	_ = tw.Flush()
}

func threadNew(m *threads.Manager, args []string) {
	fs := flag.NewFlagSet("thread new", flag.ExitOnError)
	noTab := fs.Bool("no-tab", false, "do not open a terminal tab for the new thread")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "thread new: name required")
		os.Exit(1)
	}
	name := fs.Arg(0)
	t, err := m.New(threads.NewArgs{Name: name})
	if err != nil {
		fmt.Fprintf(os.Stderr, "thread new: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created thread %q at %s\n", t.Name, t.Path)
	if !*noTab {
		threadGo(m, name, false)
	}
}

func threadArchive(m *threads.Manager, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "thread archive: name required")
		os.Exit(1)
	}
	name := args[0]
	if err := m.Archive(name); err != nil {
		fmt.Fprintf(os.Stderr, "thread archive: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Archived %q to %s\n", name, filepath.Join(m.Root, threads.ArchivedDir, name))
}

func threadGoCmd(m *threads.Manager, args []string) {
	fs := flag.NewFlagSet("thread go", flag.ExitOnError)
	noResume := fs.Bool("no-resume", false, "always spawn a fresh, untagged tab")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "thread go: name-or-path required")
		os.Exit(1)
	}
	threadGo(m, fs.Arg(0), *noResume)
}

// displayName prefixes a thread with its marker glyph (🪡 / ❗️) for the CLI,
// matching the menu-bar list's marker column.
func displayName(t threads.Thread) string {
	return t.Marker.Emoji() + " " + t.Name
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// tildeAbbrev renders the threads root with ~ for the home directory.
func tildeAbbrev(m *threads.Manager) string {
	if m.Home != "" && strings.HasPrefix(m.Root, m.Home) {
		return "~" + strings.TrimPrefix(m.Root, m.Home)
	}
	return m.Root
}
