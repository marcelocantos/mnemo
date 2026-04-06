// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// mnemo is an MCP server that provides searchable memory across all
// Claude Code session transcripts. It indexes JSONL files from
// ~/.claude/projects/ and maintains a realtime FTS5 index in SQLite.
//
// Two modes:
//
//	mnemo              # stdio MCP server (what Claude Code launches)
//	mnemo serve        # persistent daemon (what brew services runs)
//	claude mcp add --scope user mnemo -- mnemo
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/rpc"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
)

//go:embed agents-guide.md
var agentsGuide string

const version = "0.4.0"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	helpAgent := flag.Bool("help-agent", false, "print agent guide and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("mnemo", version)
		return
	}
	if *helpAgent {
		flag.CommandLine.SetOutput(os.Stdout)
		fmt.Fprintf(os.Stdout, "mnemo %s\n\nUsage: mnemo [command] [flags]\n\nCommands:\n  serve    Run persistent daemon (for brew services)\n  (none)   Run stdio MCP server\n\nFlags:\n", version)
		flag.PrintDefaults()
		fmt.Fprintln(os.Stdout)
		fmt.Print(agentsGuide)
		return
	}

	args := flag.Args()
	if len(args) > 0 && args[0] == "serve" {
		runServe()
	} else {
		runStdio()
	}
}

// runServe runs the persistent daemon: opens the store, ingests,
// watches for changes, and serves RPC over a Unix domain socket.
func runServe() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects")
	dbPath := filepath.Join(homeDir, ".mnemo", "mnemo.db")

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create db directory: %v\n", err)
		os.Exit(1)
	}

	mem, err := store.New(dbPath, projectDir)
	if err != nil {
		slog.Error("failed to open store", "err", err)
		os.Exit(1)
	}
	defer mem.Close()

	// Ingest and watch in the background.
	go func() {
		slog.Info("ingesting transcripts", "dir", projectDir)
		if err := mem.IngestAll(); err != nil {
			slog.Error("initial ingest failed", "err", err)
		}
		if stats, err := mem.Stats(); err == nil {
			slog.Info("ingest complete", "sessions", stats.TotalSessions, "messages", stats.TotalMessages)
		}
		if err := mem.Watch(); err != nil {
			slog.Error("watcher failed", "err", err)
		}
	}()

	// Serve RPC over Unix domain socket.
	srv, err := rpc.NewServer(mem)
	if err != nil {
		slog.Error("failed to start RPC server", "err", err)
		os.Exit(1)
	}
	defer srv.Close()

	slog.Info("mnemo serve starting", "version", version)
	if err := srv.Serve(); err != nil {
		slog.Error("RPC server failed", "err", err)
		os.Exit(1)
	}
}

// runStdio runs the stdio MCP server: connects to the serve process
// over UDS and proxies MCP tool calls.
func runStdio() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))

	client, err := rpc.Dial()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: %v\n", err)
		fmt.Fprintf(os.Stderr, "hint: start the server with 'brew services start mnemo' or 'mnemo serve'\n")
		os.Exit(1)
	}
	defer client.Close()

	proxy := rpc.NewProxy(client)

	s := mcpserver.NewMCPServer(
		"mnemo",
		version,
		mcpserver.WithToolCapabilities(true),
	)

	tools.Register(s, proxy)

	stdio := mcpserver.NewStdioServer(s)
	if err := stdio.Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "mnemo stdio: %v\n", err)
		os.Exit(1)
	}
}
