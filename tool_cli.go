// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/marcelocantos/mnemo/internal/tools"
)

// CLI counterparts to the MCP tools (🎯T187).
//
// Every tool in tools.Definitions() is reachable as `mnemo <name>`, and
// nothing here is written per tool: the command list, the flags, the
// positional argument and the help text are all derived from the tool's
// own input schema. A new tool therefore arrives on the command line
// without a second edit, and cannot arrive with a CLI surface that
// disagrees with its MCP one. tool_cli_test.go ratchets that in both
// directions, the way surface_test.go does for the MCP surface.
//
// The call goes through the running daemon (POST /api/tool/<name>), not
// through a second opener of the database. That is the whole point of
// the feature — one writer, one cache, one schema-upgrade actor — and
// it is also why these commands report a clear error when the daemon is
// down rather than silently doing something local.

// toolCommandTimeout bounds a single tool call. Generous because some
// tools legitimately scan the corpus: mnemo_status walks per-project
// coverage and mnemo_ops op=compress_gc can repack for a while.
const toolCommandTimeout = 5 * time.Minute

// toolCLIName maps an MCP tool name to its command name: mnemo_read_session
// becomes `mnemo read-session`. Hyphens because that is what a command line
// looks like; the underscore form stays the wire name.
func toolCLIName(mcpName string) string {
	return strings.ReplaceAll(strings.TrimPrefix(mcpName, "mnemo_"), "_", "-")
}

// toolCommands maps command name to tool definition. Built from
// Definitions() so the two cannot diverge.
func toolCommands() map[string]mcp.Tool {
	out := make(map[string]mcp.Tool, len(tools.Definitions()))
	for _, t := range tools.Definitions() {
		out[toolCLIName(t.Name)] = t
	}
	return out
}

// reservedToolCommands are command names that a hand-written subcommand
// already owns. The hand-written one wins, and the tool stays reachable
// over MCP — this is not a name to silently reassign.
//
// `thread` is the only entry and the reason is not arbitrary: its
// list/show/new/archive run in the CLI process against the filesystem
// and work with the daemon stopped, so routing them through the bridge
// would take a working command and make it require a daemon. Only
// `thread go` needs the daemon (it holds the single iTerm2 Automation
// grant) and it already delegates.
var reservedToolCommands = map[string]bool{"thread": true}

// toolPositional returns the argument a command takes positionally, or
// "". Every tool's schema has at most one required property and all of
// them are strings, so the rule is simply "the required one" — `mnemo
// search <query>`, `mnemo ops <op>`, `mnemo locate-uuid <uuid>`. An
// optional property is never positional: order would then be the only
// thing distinguishing two of them.
func toolPositional(t mcp.Tool) string {
	if len(t.InputSchema.Required) == 1 {
		if propType(t, t.InputSchema.Required[0]) == "string" {
			return t.InputSchema.Required[0]
		}
	}
	return ""
}

func propType(t mcp.Tool, name string) string {
	p, _ := t.InputSchema.Properties[name].(map[string]any)
	s, _ := p["type"].(string)
	return s
}

func propDesc(t mcp.Tool, name string) string {
	p, _ := t.InputSchema.Properties[name].(map[string]any)
	s, _ := p["description"].(string)
	return s
}

// cmdTool runs a tool command. name is the command name (hyphenated).
func cmdTool(name string, t mcp.Tool, argv []string) {
	fs := flag.NewFlagSet("mnemo "+name, flag.ExitOnError)
	positional := toolPositional(t)

	// Flags come from the schema. Values are collected as pointers and
	// read back only for flags the user actually set, so an unset flag
	// leaves the tool's own default in force rather than overriding it
	// with a Go zero value.
	strs := map[string]*string{}
	nums := map[string]*float64{}
	bools := map[string]*bool{}
	propNames := make([]string, 0, len(t.InputSchema.Properties))
	for p := range t.InputSchema.Properties {
		propNames = append(propNames, p)
	}
	sort.Strings(propNames)
	for _, p := range propNames {
		if p == positional {
			continue
		}
		usage := propDesc(t, p)
		switch propType(t, p) {
		case "string":
			strs[p] = fs.String(flagName(p), "", usage)
		case "number", "integer":
			// Float64 rather than Int: the schema says "number", and
			// every numeric argument being a count today is not a
			// guarantee it stays one. The help therefore reads
			// "-limit float", which is Go's own convention.
			nums[p] = fs.Float64(flagName(p), 0, usage)
		case "boolean":
			bools[p] = fs.Bool(flagName(p), false, usage)
		}
	}
	asJSON := fs.Bool("json", false,
		"emit the daemon's {ok,text} envelope instead of the tool's text")
	user := fs.String("user", "", "act as this mnemo user (default: the daemon's default user)")

	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "Usage: mnemo %s", name)
		if positional != "" {
			fmt.Fprintf(out, " <%s>", positional)
		}
		fmt.Fprintf(out, " [flags]\n\n%s\n\nFlags:\n", firstParagraph(t.Description))
		fs.PrintDefaults()
	}
	flagArgs, words := partitionArgs(fs, argv)
	_ = fs.Parse(flagArgs)

	args := map[string]any{}
	if positional != "" {
		if len(words) == 0 {
			fmt.Fprintf(os.Stderr, "mnemo %s: <%s> is required\n", name, positional)
			fs.Usage()
			os.Exit(2)
		}
		// Join the words so `mnemo search QR code pairing` works without
		// quoting — a search query is the common case and a shell user
		// should not have to think about it.
		args[positional] = strings.Join(words, " ")
	} else if len(words) > 0 {
		fmt.Fprintf(os.Stderr, "mnemo %s: unexpected argument %q (this command takes flags only)\n",
			name, words[0])
		os.Exit(2)
	}
	fs.Visit(func(f *flag.Flag) {
		p := propFromFlag(f.Name)
		switch {
		case strs[p] != nil:
			args[p] = *strs[p]
		case nums[p] != nil:
			args[p] = *nums[p]
		case bools[p] != nil:
			args[p] = *bools[p]
		}
	})

	text, ok, envelope := callToolViaDaemon(t.Name, *user, args)
	if *asJSON {
		os.Stdout.Write(envelope)
		if len(envelope) == 0 || envelope[len(envelope)-1] != '\n' {
			fmt.Println()
		}
	} else {
		fmt.Println(strings.TrimRight(text, "\n"))
	}
	if !ok {
		os.Exit(1)
	}
}

// partitionArgs splits argv into flag tokens and positional words, so
// flags may appear anywhere on the line.
//
// Go's flag package stops parsing at the first non-flag argument, which
// for these commands is the common shape: `mnemo search compaction
// --limit 1` would parse zero flags and hand "compaction --limit 1" to
// fs.Args(). Since the positional is joined into one string, that lands
// in the query itself — the search runs for the literal text "compaction
// --limit 1", finds nothing, and reports an honest-looking empty result.
// Nothing errors, so the only way to notice is to compare against the
// same call made directly against the daemon.
//
// A flag token is "-x" or "--x", optionally "=value". When it carries no
// "=" and names a non-boolean flag, the following token is its value and
// travels with it. Everything else is a positional word. "--" ends flag
// parsing, as usual.
func partitionArgs(fs *flag.FlagSet, argv []string) (flagArgs, words []string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			words = append(words, argv[i+1:]...)
			return flagArgs, words
		}
		if len(a) < 2 || a[0] != '-' {
			words = append(words, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		bare := strings.TrimLeft(a, "-")
		if strings.Contains(bare, "=") {
			continue
		}
		// A boolean flag never consumes the next token: `--json compaction`
		// must leave "compaction" as the query, not eat it as a value.
		if f := fs.Lookup(bare); f != nil && !isBoolFlag(f) && i+1 < len(argv) {
			i++
			flagArgs = append(flagArgs, argv[i])
		}
	}
	return flagArgs, words
}

// isBoolFlag reports whether f is a boolean flag, which the flag package
// models as a Value that additionally answers IsBoolFlag.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

// callToolViaDaemon posts the arguments to the daemon and returns the
// tool's text, its ok verdict, and the raw envelope for --json.
func callToolViaDaemon(mcpName, user string, args map[string]any) (string, bool, []byte) {
	// Marshal the arguments rather than hand-building the request: the
	// tool layer type-asserts numeric arguments as float64, so they have
	// to travel as JSON numbers. A Go int would fail the assertion on
	// the far side and be replaced by the tool's default — a flag that
	// looks accepted and is ignored.
	body, err := json.Marshal(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: encode arguments: %v\n", err)
		os.Exit(1)
	}
	url := daemonBaseURL() + "/api/tool/" + mcpName
	if user != "" {
		url += "?user=" + user
	}
	client := &http.Client{Timeout: toolCommandTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: cannot reach the mnemo daemon at %s: %v\n", daemonBaseURL(), err)
		fmt.Fprintln(os.Stderr, "is it running? check with `mnemo diagnose`.")
		os.Exit(1)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "mnemo: daemon error (%s): %s\n", resp.Status, strings.TrimSpace(string(raw)))
		os.Exit(1)
	}
	var res struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: decode response: %v\n", err)
		os.Exit(1)
	}
	return res.Text, res.OK, raw
}

// flagName renders a schema property as a flag: session_id → --session-id.
func flagName(prop string) string { return strings.ReplaceAll(prop, "_", "-") }

// propFromFlag is flagName's inverse.
func propFromFlag(name string) string { return strings.ReplaceAll(name, "-", "_") }

// firstParagraph trims a tool description down to its opening paragraph.
// The full descriptions are written for an agent's context window and run
// to dozens of lines; a shell user wants the first sentence and the flags.
func firstParagraph(desc string) string {
	if i := strings.Index(desc, "\n\n"); i >= 0 {
		desc = desc[:i]
	}
	return strings.TrimSpace(strings.ReplaceAll(desc, "\n", " "))
}

// toolCommandSummary lists the tool commands for `mnemo --help`.
func toolCommandSummary() string {
	cmds := toolCommands()
	names := make([]string, 0, len(cmds))
	for n := range cmds {
		if !reservedToolCommands[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		arg := ""
		if p := toolPositional(cmds[n]); p != "" {
			arg = " <" + p + ">"
		}
		fmt.Fprintf(&b, "  mnemo %-24s %s\n", n+arg,
			clip(firstSentence(firstParagraph(cmds[n].Description)), summaryWidth))
	}
	return b.String()
}

// summaryWidth keeps the one-line command summaries inside a terminal.
// Tool descriptions are written for an agent's context window, so the
// first sentence alone can still run past 200 characters.
const summaryWidth = 62

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n], " ")
	if cut <= 0 {
		cut = n
	}
	return strings.TrimRight(s[:cut], " .,;:") + "…"
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}
