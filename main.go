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
	"time"

	"github.com/marcelocantos/mnemo/internal/bridge"

	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/rpc"
	"github.com/marcelocantos/mnemo/internal/store"
)

//go:embed agents-guide.md
var agentsGuide string

const version = "0.17.0"

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

	// Load ~/.mnemo/config.json and configure workspace roots for
	// repo-level ingest streams. Falls back to defaults when absent.
	cfg, cfgErr := store.LoadConfig()
	if cfgErr != nil {
		slog.Warn("config load failed, using defaults", "err", cfgErr)
	}
	workspaceRoots := cfg.ResolvedWorkspaceRoots()
	mem.SetWorkspaceRoots(workspaceRoots)
	slog.Info("workspace roots configured", "roots", workspaceRoots)

	// Configure extra Claude Code project directories (e.g. a Windows
	// VM's projects dir exposed over SMB). Missing or unavailable
	// entries are skipped at ingest/watch time rather than failing.
	mem.SetExtraProjectDirs(cfg.ExtraProjectDirs)
	if len(cfg.ExtraProjectDirs) > 0 {
		slog.Info("extra project dirs configured", "dirs", cfg.ExtraProjectDirs)
	}

	// Ingest and watch in the background.
	go func() {
		slog.Info("ingesting transcripts", "dir", projectDir, "extras", len(cfg.ExtraProjectDirs))
		if err := mem.IngestAll(); err != nil {
			slog.Error("initial ingest failed", "err", err)
		}
		if stats, err := mem.Stats(); err == nil {
			slog.Info("ingest complete", "sessions", stats.TotalSessions, "messages", stats.TotalMessages)
		}
		// Start background image description workers (no-op if no API key).
		mem.StartImageDescriber()
		// Start background image OCR workers (no-op if no backend available).
		mem.StartImageOCR()
		// Start background image embedding workers (no-op if uv/embed script not found).
		mem.StartImageEmbedder()
		if err := mem.IngestMemories(); err != nil {
			slog.Error("memory ingest failed", "err", err)
		}
		if err := mem.IngestSkills(); err != nil {
			slog.Error("skill ingest failed", "err", err)
		}
		if err := mem.IngestClaudeConfigs(); err != nil {
			slog.Error("claude config ingest failed", "err", err)
		}
		if err := mem.IngestAuditLogs(); err != nil {
			slog.Error("audit log ingest failed", "err", err)
		}
		if err := mem.IngestTargets(); err != nil {
			slog.Error("target ingest failed", "err", err)
		}
		if err := mem.IngestPlans(); err != nil {
			slog.Error("plan ingest failed", "err", err)
		}
		if err := mem.IngestDocs(); err != nil {
			slog.Error("doc ingest failed", "err", err)
		}
		if err := mem.Watch(); err != nil {
			slog.Error("watcher failed", "err", err)
		}
	}()

	// Start background compaction watcher.
	go func() {
		mnemoDir, _ := os.UserHomeDir()
		mnemoRepoDir := filepath.Join(mnemoDir, "work", "github.com", "marcelocantos", "mnemo")
		caller := compact.NewClaudiaCaller(mnemoRepoDir, "sonnet")
		compactor := compact.New(mem, caller, compact.Config{})
		watcher := compact.NewWatcher(mem, compactor, compact.WatcherConfig{}, mnemoRepoDir)
		ctx := context.Background()
		slog.Info("compact: watcher starting")
		watcher.Run(ctx)
	}()

	// Poll CI runs periodically (every 5 minutes).
	go func() {
		for {
			if err := mem.PollCI(); err != nil {
				slog.Warn("CI poll failed", "err", err)
			}
			time.Sleep(5 * time.Minute)
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

// runStdio runs the stdio MCP proxy via mcpbridge.
func runStdio() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))

	if err := mcpbridge.RunProxy(context.Background(), mcpbridge.ProxyConfig{
		SocketPath: rpc.SocketPath(),
		ServerName: "mnemo",
		Version:    version,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: %v\n", err)
		fmt.Fprintf(os.Stderr, "hint: start the server with 'brew services start mnemo' or 'mnemo serve'\n")
		os.Exit(1)
	}
}
