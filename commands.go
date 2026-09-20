// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"
	"strings"
)

// otherCommands is the hand-written half of the CLI: the commands that
// are not counterparts of an MCP tool (tool_cli.go derives those from
// tools.Definitions()).
//
// It is a table rather than a switch so `mnemo --help` can list the
// commands without a second, drifting copy of the same names. Every
// entry is `func([]string)` because a subcommand parses its own flags —
// dispatch happens before flag.Parse so a command's flags cannot
// collide with the daemon's.
//
// `ocr-worker` is deliberately absent: the daemon re-execs itself with
// it to isolate Apple Vision (🎯T118), and it is an implementation
// detail rather than a command anyone should find in help output.
var otherCommands = map[string]struct {
	run  func([]string)
	desc string
}{
	"register-mcp":         {cmdRegisterMCP, "Register this daemon with an MCP client's config"},
	"unregister-mcp":       {cmdUnregisterMCP, "Remove this daemon from an MCP client's config"},
	"install-service":      {cmdInstallService, "Install the OS service definition for the daemon"},
	"uninstall-service":    {cmdUninstallService, "Remove the OS service definition"},
	"diagnose":             {cmdDiagnose, "Check the daemon, its endpoint and its registrations"},
	"print-endpoint":       {cmdPrintEndpoint, "Print the daemon's MCP endpoint URL"},
	"print-federated-addr": {cmdPrintFederatedAddr, "Print the mTLS federated listen address"},
	"ping-peer":            {cmdPingPeer, "Check a federated peer over mTLS"},
	"thread":               {cmdThread, "Thread navigator: list, show, new, archive, go"},
	"resume":               {cmdResume, "Open a terminal tab resuming a session"},
	"budget":               {cmdBudget, "Spend, projection and throttle state"},
	"edge":                 {cmdEdge, "Manage federation edges"},
	"replay-files":         {cmdReplayFiles, "Re-ingest transcript files"},
	"dedupe-entries":       {cmdDedupeEntries, "Remove duplicate entries rows (daemon must be stopped)"},
}

// otherCommandSummary renders the table for `mnemo --help`.
func otherCommandSummary() string {
	names := make([]string, 0, len(otherCommands))
	for n := range otherCommands {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "  mnemo %-24s %s\n", n, otherCommands[n].desc)
	}
	return b.String()
}
