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
//	mnemo install-service       # (Windows) install mnemo as a Service
//	mnemo uninstall-service     # (Windows) remove the Service
//	claude mcp add --scope user --transport http mnemo "http://localhost:19419/mcp?user=<name>"
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/mcpconfig"
	"github.com/marcelocantos/mnemo/internal/registry"
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
	version     = "0.25.0"
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
		case "install-service":
			cmdInstallService(os.Args[2:])
			return
		case "uninstall-service":
			cmdUninstallService(os.Args[2:])
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

	// On Windows, if the SCM launched this process (no interactive
	// session), hand off to the service control dispatcher, which
	// calls runServe with a cancellable context driven by SCM events.
	if handled, err := runAsServiceIfUnderSCM(*addr); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "service run failed: %v\n", err)
			os.Exit(1)
		}
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
	urlFlag := fs.String("url", "", "MCP endpoint URL to register (default: localhost:19419/mcp?user=<current>)")
	userFlag := fs.String("user", "", "username to embed as ?user= in the default URL (default: current OS user)")
	configPath := fs.String("config", "", "Claude Code config path (default ~/.claude.json)")
	_ = fs.Parse(args)

	url := *urlFlag
	if url == "" {
		username := *userFlag
		if username == "" {
			u, err := store.CurrentUsername()
			if err != nil {
				fmt.Fprintf(os.Stderr, "register-mcp: %v\n", err)
				os.Exit(1)
			}
			username = u
		}
		url = mcpconfig.URLForUser(username)
	}

	path := *configPath
	if path == "" {
		p, err := mcpconfig.ConfigPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		path = p
	}
	changed, err := mcpconfig.Register(path, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register-mcp: %v\n", err)
		os.Exit(1)
	}
	if changed {
		fmt.Printf("mnemo MCP registered in %s (url=%s)\n", path, url)
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

func cmdInstallService(args []string) {
	if err := installService(args); err != nil {
		fmt.Fprintf(os.Stderr, "install-service: %v\n", err)
		os.Exit(1)
	}
}

func cmdUninstallService(args []string) {
	if err := uninstallService(args); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall-service: %v\n", err)
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

	// Load ~/.mnemo/config.json once; it applies uniformly to every
	// per-user Store spun up by the Registry. Per-user home paths are
	// resolved lazily inside the Registry when the first request for
	// each user arrives.
	cfg, cfgErr := store.LoadConfig()
	if cfgErr != nil {
		slog.Warn("config load failed, using defaults", "err", cfgErr)
	}
	slog.Info("workspace roots configured", "roots", cfg.ResolvedWorkspaceRoots())
	if len(cfg.ExtraProjectDirs) > 0 {
		slog.Info("extra project dirs configured", "dirs", cfg.ExtraProjectDirs)
	}

	// The compactor watcher needs mnemo's own source tree to invoke
	// claudia, regardless of whose transcripts are being compacted.
	// Resolve via the process owner's home (not each indexed user's).
	procHome, _ := os.UserHomeDir()
	mnemoRepoDir := filepath.Join(procHome, "work", "github.com", "marcelocantos", "mnemo")

	reg := registry.NewRegistry(ctx, cfg, mnemoRepoDir)
	defer reg.Close()

	// Determine the default username — used when a request arrives
	// without an explicit ?user=<name> query parameter. On a Windows
	// Service deployment (running as LocalSystem) there is no
	// sensible default, so every request MUST carry a user.
	defaultUser, defErr := store.DefaultUsername()
	if defErr != nil {
		slog.Info("no default user — requests must include ?user=<name>", "reason", defErr)
	} else {
		slog.Info("default user", "user", defaultUser)
	}

	// Resolver threaded into tools.Handler. If the inbound request
	// carried no ?user= parameter, fall back to the process default
	// (empty on a service deployment, which produces a useful error).
	resolve := func(username string) (store.Backend, error) {
		if username == "" {
			if defErr != nil {
				return nil, errors.New(
					"no user identity on request; add ?user=<name> to the MCP URL",
				)
			}
			username = defaultUser
		}
		return reg.ForUser(username)
	}

	// Build the MCP server, register every tool, and expose it as an
	// HTTP streamable endpoint. Stateful mode lets clients maintain an
	// Mcp-Session-Id across requests — the value we thread through to
	// mnemo_self for session binding. WithHTTPContextFunc captures the
	// ?user=<name> query parameter onto every request's ctx so tool
	// handlers can look up the right user's store.
	mcp := server.NewMCPServer(
		"mnemo",
		version,
		server.WithToolCapabilities(true),
	)
	tools.NewHandler(resolve).RegisterTools(mcp)

	httpSrv := server.NewStreamableHTTPServer(mcp,
		server.WithStateful(true),
		server.WithHTTPContextFunc(tools.UsernameContextFunc),
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
