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
//	mnemo diagnose              # health check: tools, paths, db, freshness, integration
//	mnemo print-endpoint        # print this host's mTLS public cert (for federated peer trust)
//	mnemo print-federated-addr  # print the URL peers paste into linked_instances
//	mnemo ping-peer <name>      # call mnemo_stats on a configured peer (smoke-test federation)
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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/federation"
	"github.com/marcelocantos/mnemo/internal/mcpconfig"
	"github.com/marcelocantos/mnemo/internal/registry"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
)

// stdioMigrationManualHint is emitted only when auto-migration of
// the user's ~/.claude.json fails. Normal launches via a legacy
// stdio registration auto-rewrite the entry to HTTP and then exit
// asking the user to restart the session.
const stdioMigrationManualHint = `mnemo has migrated to HTTP MCP (🎯T27 in v0.20.0) but the
auto-migration of your Claude Code config failed. Update by hand:

  claude mcp remove mnemo
  claude mcp add --scope user --transport http mnemo "http://localhost:19419/mcp?user=<your-username>"

Then restart this Claude Code session.`

//go:embed agents-guide.md
var agentsGuide string

const (
	version              = "0.29.0"
	defaultAddr          = ":19419"
	defaultFederatedAddr = ":19420"
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
		case "diagnose":
			cmdDiagnose(os.Args[2:])
			return
		case "print-endpoint":
			cmdPrintEndpoint(os.Args[2:])
			return
		case "print-federated-addr":
			cmdPrintFederatedAddr(os.Args[2:])
			return
		case "ping-peer":
			cmdPingPeer(os.Args[2:])
			return
		}
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	helpAgent := flag.Bool("help-agent", false, "print agent guide and exit")
	addr := flag.String("addr", defaultAddr, "HTTP listen address")
	federatedAddr := flag.String("federated-addr", defaultFederatedAddr,
		"mTLS federated listen address (empty disables federation)")
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
	if handled, err := runAsServiceIfUnderSCM(*addr, *federatedAddr); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "service run failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Detect a stale stdio MCP registration before opening the
	// store. If stdin is piped, this binary was launched by an MCP
	// client expecting a stdio server — but mnemo only speaks HTTP
	// since v0.20.0. Auto-migrate the user's ~/.claude.json to the
	// HTTP transport (and try to start the daemon if nothing's
	// listening yet), then exit asking the user to restart their
	// session. One restart instead of three terminal commands.
	if stdinPiped() {
		autoMigrateStdioAndExit(*addr)
	}

	if err := runServe(context.Background(), *addr, *federatedAddr); err != nil {
		os.Exit(1)
	}
}

// cmdPingPeer dials a configured federation peer and runs mnemo_stats
// against it, printing the peer's response (or a typed error) for
// manual verification (🎯T15.4). Reads ~/.mnemo/config.json for the
// LinkedInstances list and ~/.mnemo/endpoint/* for the local mTLS
// material; the peer name argument selects which entry to call.
func cmdPingPeer(args []string) {
	fs := flag.NewFlagSet("ping-peer", flag.ExitOnError)
	tool := fs.String("tool", "mnemo_stats", "tool name to invoke on the peer")
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mnemo ping-peer [--tool=NAME] <peer-name>")
		os.Exit(2)
	}
	peerName := fs.Arg(0)

	cfg, err := store.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping-peer: %v\n", err)
		os.Exit(1)
	}
	if len(cfg.LinkedInstances) == 0 {
		fmt.Fprintln(os.Stderr, "ping-peer: no linked_instances configured in ~/.mnemo/config.json")
		os.Exit(1)
	}
	mnemoDir, err := endpoint.DefaultDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping-peer: %v\n", err)
		os.Exit(1)
	}
	ep, err := endpoint.Load(mnemoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping-peer: load endpoint: %v\n", err)
		os.Exit(1)
	}
	client, err := federation.NewClient(ep, cfg.LinkedInstances)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping-peer: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.CallTool(ctx, peerName, *tool, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping-peer: %v\n", err)
		os.Exit(1)
	}
	if res.IsError {
		fmt.Fprintf(os.Stderr, "ping-peer: peer returned error\n")
	}
	for _, c := range res.Content {
		if t, ok := c.(mcp.TextContent); ok {
			fmt.Println(t.Text)
		}
	}
}

// cmdPrintFederatedAddr emits the URL peers should put in their
// linked_instances entry to reach this host's federated MCP endpoint
// (🎯T15.3). Defaults to https://<hostname>:19420/mcp; --addr lets
// the user override the host:port portion (handy when the daemon
// listens on a non-default port or the public name differs).
func cmdPrintFederatedAddr(args []string) {
	fs := flag.NewFlagSet("print-federated-addr", flag.ExitOnError)
	addrFlag := fs.String("addr", defaultFederatedAddr,
		"federated listen address — port portion is used as-is, host portion defaults to os.Hostname()")
	_ = fs.Parse(args)

	host, _, err := net.SplitHostPort(*addrFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "print-federated-addr: parse %q: %v\n", *addrFlag, err)
		os.Exit(1)
	}
	if host == "" {
		host, err = os.Hostname()
		if err != nil || host == "" {
			fmt.Fprintf(os.Stderr, "print-federated-addr: cannot determine hostname; pass --addr=host:port\n")
			os.Exit(1)
		}
	}
	_, port, _ := net.SplitHostPort(*addrFlag)
	fmt.Printf("https://%s:%s/mcp\n", host, port)
}

// autoMigrateStdioAndExit rewrites the user's ~/.claude.json entry
// for mnemo to the new HTTP transport (with ?user=<current-user>
// embedded), best-effort starts the daemon if it isn't already
// running, and exits — telling the user to restart their session.
// On any failure path it falls back to printing the manual hint so
// the user has a recovery route.
func autoMigrateStdioAndExit(addr string) {
	fmt.Fprintln(os.Stderr, "mnemo: detected legacy stdio registration; auto-migrating to HTTP...")

	path, err := mcpconfig.ConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: cannot find ~/.claude.json: %v\n\n%s\n", err, stdioMigrationManualHint)
		os.Exit(1)
	}
	username, err := store.CurrentUsername()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: cannot resolve current user: %v\n\n%s\n", err, stdioMigrationManualHint)
		os.Exit(1)
	}
	url := mcpconfig.URLForUser(username)
	changed, err := mcpconfig.Register(path, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mnemo: auto-migration failed: %v\n\n%s\n", err, stdioMigrationManualHint)
		os.Exit(1)
	}
	if changed {
		fmt.Fprintf(os.Stderr, "mnemo: ~/.claude.json updated to %s\n", url)
	}

	// If nothing's listening on the HTTP port yet, try to start the
	// daemon via brew services. Best-effort — if brew isn't on PATH
	// (common on Linux installs that built from source) we skip the
	// start and the user gets a clean "please start mnemo" message
	// next session.
	if !portInUse(addr) {
		if _, err := exec.LookPath("brew"); err == nil {
			cmd := exec.Command("brew", "services", "start", "mnemo")
			if err := cmd.Run(); err == nil {
				fmt.Fprintln(os.Stderr, "mnemo: started daemon via `brew services start mnemo`")
			} else {
				fmt.Fprintf(os.Stderr, "mnemo: could not auto-start daemon (%v); run `brew services start mnemo` manually\n", err)
			}
		} else {
			fmt.Fprintln(os.Stderr, "mnemo: HTTP daemon not running; start it before restarting your session")
		}
	}

	fmt.Fprintln(os.Stderr, "mnemo: restart this Claude Code session to pick up the new HTTP registration.")
	os.Exit(1)
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

// cmdPrintEndpoint emits the daemon's public mTLS certificate to
// stdout for paste-distribution to peer mnemo instances. If no cert
// exists yet, one is generated on the spot — matching the "first
// start" semantics of `mnemo serve`.
func cmdPrintEndpoint(args []string) {
	fs := flag.NewFlagSet("print-endpoint", flag.ExitOnError)
	dirFlag := fs.String("dir", "", "mnemo state directory (default ~/.mnemo)")
	_ = fs.Parse(args)

	dir := *dirFlag
	if dir == "" {
		d, err := endpoint.DefaultDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "print-endpoint: %v\n", err)
			os.Exit(1)
		}
		dir = d
	}
	ep, err := endpoint.Load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "print-endpoint: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(ep.CertPEM); err != nil {
		fmt.Fprintf(os.Stderr, "print-endpoint: %v\n", err)
		os.Exit(1)
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

// registerFanoutTools installs the federation fan-out wrapper for
// every tool in federation.FanoutToolNames. The wrapper runs the
// local handler and the per-peer Fanout in parallel, merges the
// outputs into a federation.FanoutEnvelope, and returns it as MCP
// TextContent. Local errors short-circuit (peers can't compensate
// for a broken local store); peer errors classify into warnings on
// the envelope so a slow or offline peer never blocks the response.
func registerFanoutTools(s *server.MCPServer, h *tools.Handler, fed *federation.Client) {
	for _, tool := range tools.Definitions() {
		if _, ok := federation.FanoutToolNames[tool.Name]; !ok {
			continue
		}
		name := tool.Name
		local := h.LocalHandler(name)
		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			type localOut struct {
				res *mcp.CallToolResult
				err error
			}
			localCh := make(chan localOut, 1)
			go func() {
				res, err := local(ctx, req)
				localCh <- localOut{res: res, err: err}
			}()
			peerResults, peerWarnings := fed.Fanout(ctx, name, req.GetArguments())
			lo := <-localCh
			if lo.err != nil {
				return lo.res, lo.err
			}
			if lo.res != nil && lo.res.IsError {
				return lo.res, nil
			}

			localText := ""
			if lo.res != nil {
				for _, c := range lo.res.Content {
					if t, ok := c.(mcp.TextContent); ok {
						localText += t.Text
					}
				}
			}
			merged, err := federation.MergePeerResults(localText, peerResults, peerWarnings)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s: merge peer results: %v", name, err)), nil
			}
			return mcp.NewToolResultText(merged), nil
		})
	}
}

// runServe opens the store, starts ingest and background workers, and
// serves the MCP protocol over HTTP until ctx is cancelled or the
// server fails. Used by both the foreground launcher and the Windows
// Service handler.
//
// If federatedAddr is non-empty, a second mTLS listener is started in
// parallel that exposes the read-only tool subset to peer mnemo
// instances (🎯T15.3). An empty federatedAddr disables federation
// entirely; the daemon makes no outbound peer calls and accepts no
// inbound mTLS connections.
func runServe(ctx context.Context, addr, federatedAddr string) error {
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
	mcpSrv := server.NewMCPServer(
		"mnemo",
		version,
		server.WithToolCapabilities(true),
	)
	handler := tools.NewHandler(resolve)

	// Build a federation client if linked_instances are configured —
	// this owns one persistent http.Client per peer and is shared
	// across every fan-out tool registration. Failure to construct
	// (e.g. a bad peer cert that slipped past startup validation)
	// disables fan-out but does not block the local listener.
	var fedClient *federation.Client
	if len(cfg.LinkedInstances) > 0 {
		mnemoDir, err := endpoint.DefaultDir()
		if err == nil {
			ep, err := endpoint.Load(mnemoDir)
			if err == nil {
				fedClient, err = federation.NewClient(ep, cfg.LinkedInstances)
				if err != nil {
					slog.Warn("federation client disabled", "err", err)
					fedClient = nil
				} else {
					slog.Info("federation peers configured", "peers", fedClient.PeerNames())
				}
			} else {
				slog.Warn("federation client disabled", "err", err)
			}
		}
	}

	if fedClient != nil && len(fedClient.PeerNames()) > 0 {
		// Skip the fan-out tools when registering defaults so we can
		// install our wrapper instead.
		handler.RegisterToolsExcept(mcpSrv, federation.FanoutToolNames)
		registerFanoutTools(mcpSrv, handler, fedClient)
	} else {
		handler.RegisterTools(mcpSrv)
	}

	httpSrv := server.NewStreamableHTTPServer(mcpSrv,
		server.WithStateful(true),
		server.WithHTTPContextFunc(tools.UsernameContextFunc),
	)

	slog.Info("mnemo serve starting", "version", version, "addr", addr)

	// Run the local HTTP server in a goroutine so we can react to ctx
	// cancellation (triggered by the Windows Service handler on SCM
	// Stop, or never triggered in the foreground case).
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Start(addr) }()

	// Optionally start the federated mTLS server in parallel
	// (🎯T15.3). A startup failure here is non-fatal — we log and
	// continue serving local clients, so a missing endpoint cert or a
	// busy federated port doesn't take down the daemon.
	var fedSrv *http.Server
	if federatedAddr != "" {
		mnemoDir, err := endpoint.DefaultDir()
		if err != nil {
			slog.Warn("federated listener disabled", "err", err)
		} else {
			fedSrv, err = federation.Start(ctx, mnemoDir, federatedAddr, version,
				tools.NewHandler(resolve))
			if err != nil {
				slog.Warn("federated listener disabled", "err", err)
			}
		}
	}

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
		if fedSrv != nil {
			if err := fedSrv.Shutdown(shutdownCtx); err != nil {
				slog.Warn("federated HTTP shutdown error", "err", err)
			}
		}
		return nil
	}
}
