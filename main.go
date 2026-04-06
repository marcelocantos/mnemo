// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// mnemo is an MCP server that provides searchable memory across all
// Claude Code session transcripts. It indexes JSONL files from
// ~/.claude/projects/ and maintains a realtime FTS5 index in SQLite.
//
// Run as a persistent HTTP service:
//
//	mnemo                          # listen on :19419
//	mnemo --addr :8080             # custom port
//	claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
)

const version = "0.1.0"

func main() {
	addr := flag.String("addr", ":19419", "listen address")
	flag.Parse()

	// Determine paths.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects")
	dbPath := filepath.Join(homeDir, ".mnemo", "mnemo.db")

	// Ensure db directory exists.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create db directory: %v\n", err)
		os.Exit(1)
	}

	// Set up logging to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Open the transcript store.
	mem, err := store.New(dbPath, projectDir)
	if err != nil {
		slog.Error("failed to open store", "err", err)
		os.Exit(1)
	}
	defer mem.Close()

	// Ingest and watch in the background so the MCP server is
	// immediately responsive.
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

	// Create MCP server.
	s := server.NewMCPServer(
		"mnemo",
		version,
		server.WithToolCapabilities(true),
	)

	// Register tools.
	tools.Register(s, mem)

	// Run as streamable HTTP server.
	httpServer := server.NewStreamableHTTPServer(s)
	slog.Info("mnemo starting", "version", version, "addr", *addr)
	if err := httpServer.Start(*addr); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
