// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// mnemo is an MCP server that provides searchable memory across all
// Claude Code session transcripts. It indexes JSONL files from
// ~/.claude/projects/ and maintains a realtime FTS5 index in SQLite.
//
// mnemo runs as a single HTTP MCP daemon:
//
//	mnemo                       # run the HTTP MCP daemon (default :19419)
//	mnemo --addr :8080          # custom listen address
//	mnemo register-mcp          # add mnemo to ~/.claude.json
//	mnemo unregister-mcp        # remove mnemo from ~/.claude.json
//	mnemo install-agent         # (Windows) register the per-user Scheduled Task
//	mnemo uninstall-agent       # (Windows) remove the Scheduled Task
//	claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/mcpconfig"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
)

// stdioMigrationMessage is emitted when mnemo is launched with stdin
// piped (i.e. by an MCP client expecting a stdio server) but cannot
// bind its HTTP port because the brew-managed daemon already owns it.
// This is the common upgrade hazard from v0.19.0: Claude Code's
// stdio registration survived the upgrade and now launches the new
// binary in an incompatible mode.
const stdioMigrationMessage = `mnemo has migrated to HTTP MCP (🎯T27 in v0.20.0). Your Claude Code
registration is out of date.

The mnemo daemon is already running on http://localhost:19419/mcp
(via brew services). Update your registration:

  claude mcp remove mnemo
  claude mcp add --scope user --transport http mnemo http://localhost:19419/mcp

Then restart this Claude Code session.`

//go:embed agents-guide.md
var agentsGuide string

const (
	version     = "0.24.0"
	defaultAddr = ":19419"
)

func main() {
	// Subcommands are dispatched before flag.Parse so their own flags
	// don't collide with the global ones (--addr, --version). The
	// default (no subcommand) path keeps the v0.21.0 behaviour: parse
	// global flags and serve HTTP.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "register-mcp":
			cmdRegisterMCP(os.Args[2:])
			return
		case "unregister-mcp":
			cmdUnregisterMCP(os.Args[2:])
			return
		case "install-agent":
			cmdInstallAgent(os.Args[2:])
			return
		case "uninstall-agent":
			cmdUninstallAgent(os.Args[2:])
			return
		}
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	helpAgent := flag.Bool("help-agent", false, "print agent guide and exit")
	addr := flag.String("addr", defaultAddr, "HTTP listen address")
	flag.Parse()

	if *showVersion {
		fmt.Println("mnemo", version)
		return
	}
	if *helpAgent {
		flag.CommandLine.SetOutput(os.Stdout)
		fmt.Fprintf(os.Stdout, "mnemo %s\n\nUsage: mnemo [flags]\n\nFlags:\n", version)
		flag.PrintDefaults()
		fmt.Fprintln(os.Stdout)
		fmt.Print(agentsGuide)
		return
	}

	// Detect a stale stdio MCP registration before opening the store.
	// If our stdin looks piped and the target port is already bound,
	// the caller is almost certainly Claude Code invoking this binary
	// via an old stdio registration while the new HTTP daemon holds
	// the port. Exit with a migration hint rather than silently
	// failing on port-already-in-use.
	if stdinPiped() && portInUse(*addr) {
		fmt.Fprintln(os.Stderr, stdioMigrationMessage)
		os.Exit(1)
	}

	if err := runServe(context.Background(), *addr); err != nil {
		os.Exit(1)
	}
}

func cmdRegisterMCP(args []string) {
	fs := flag.NewFlagSet("register-mcp", flag.ExitOnError)
	url := fs.String("url", mcpconfig.DefaultURL, "MCP endpoint URL to register")
	configPath := fs.String("config", "", "Claude Code config path (default ~/.claude.json)")
	_ = fs.Parse(args)
	path := *configPath
	if path == "" {
		p, err := mcpconfig.ConfigPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		path = p
	}
	changed, err := mcpconfig.Register(path, *url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register-mcp: %v\n", err)
		os.Exit(1)
	}
	if changed {
		fmt.Printf("mnemo MCP registered in %s\n", path)
	} else {
		fmt.Printf("mnemo MCP already registered in %s\n", path)
	}
}

func cmdUnregisterMCP(args []string) {
	fs := flag.NewFlagSet("unregister-mcp", flag.ExitOnError)
	configPath := fs.String("config", "", "Claude Code config path (default ~/.claude.json)")
	_ = fs.Parse(args)
	path := *configPath
	if path == "" {
		p, err := mcpconfig.ConfigPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		path = p
	}
	changed, err := mcpconfig.Unregister(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unregister-mcp: %v\n", err)
		os.Exit(1)
	}
	if changed {
		fmt.Printf("mnemo MCP removed from %s\n", path)
	} else {
		fmt.Printf("mnemo MCP was not registered in %s\n", path)
	}
}

func cmdInstallAgent(args []string) {
	if err := installAgent(args); err != nil {
		fmt.Fprintf(os.Stderr, "install-agent: %v\n", err)
		os.Exit(1)
	}
}

func cmdUninstallAgent(args []string) {
	if err := uninstallAgent(args); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall-agent: %v\n", err)
		os.Exit(1)
	}
}

// stdinPiped reports whether stdin is a pipe or file (i.e. not a tty),
// which is the case when an MCP client launches mnemo as a stdio
// server. Returns false on stat errors so terminal-interactive users
// never see the migration path by accident.
func stdinPiped() bool {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// portInUse reports whether the given TCP address is already bound.
// Any listen error (including "address in use") is treated as busy.
func portInUse(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

// runServe opens the store, starts ingest and background workers, and
// serves the MCP protocol over HTTP until ctx is cancelled or the
// server fails. Used by both the foreground launcher and the Windows
// Service handler.
func runServe(ctx context.Context, addr string) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine home directory: %v\n", err)
		return err
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects")
	dbPath := filepath.Join(homeDir, ".mnemo", "mnemo.db")

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create db directory: %v\n", err)
		return err
	}

	mem, err := store.New(dbPath, projectDir)
	if err != nil {
		slog.Error("failed to open store", "err", err)
		return err
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
		slog.Info("compact: watcher starting")
		watcher.Run(ctx)
	}()

	// Poll CI runs periodically (every 5 minutes).
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			if err := mem.PollCI(); err != nil {
				slog.Warn("CI poll failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// Build the MCP server, register every tool, and expose it as an
	// HTTP streamable endpoint. Stateful mode lets clients maintain an
	// Mcp-Session-Id across requests — the value we thread through to
	// mnemo_self for session binding.
	mcp := server.NewMCPServer(
		"mnemo",
		version,
		server.WithToolCapabilities(true),
	)
	tools.NewHandler(mem).RegisterTools(mcp)

	httpSrv := server.NewStreamableHTTPServer(mcp,
		server.WithStateful(true),
	)

	slog.Info("mnemo serve starting", "version", version, "addr", addr)

	// Run the HTTP server in a goroutine so we can react to ctx
	// cancellation (triggered by the Windows Service handler on SCM
	// Stop, or never triggered in the foreground case).
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Start(addr) }()

	select {
	case err := <-errCh:
		slog.Error("HTTP MCP server failed", "err", err)
		return err
	case <-ctx.Done():
		slog.Info("mnemo serve shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("HTTP shutdown error", "err", err)
		}
		return nil
	}
}
