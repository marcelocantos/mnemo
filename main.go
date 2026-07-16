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
//	mnemo edge                  # transparent MCP edge proxy (🎯T97.3)
//	claude mcp add --scope user --transport http mnemo "http://localhost:19419/mcp?user=<name>"
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/api"
	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/edgeproxy"
	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/federation"
	"github.com/marcelocantos/mnemo/internal/mcpconfig"
	"github.com/marcelocantos/mnemo/internal/plugin"
	"github.com/marcelocantos/mnemo/internal/registry"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
	"github.com/marcelocantos/mnemo/internal/upgrade"
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

//go:embed ui/dashboard.html
var dashboardHTML []byte

const (
	version              = "0.66.0"
	defaultAddr          = ":19419"
	defaultFederatedAddr = ":19420"

	// drainDeadline caps the graceful shutdown sequence (🎯T97.1). Stopping
	// HTTP intake, stopping workers, quiescing the read pool, and truncating
	// the WAL should complete well within this; if it overruns we hard-exit,
	// which is safe under mnemo's crash-only durability (the next start
	// recovers any un-checkpointed WAL frames).
	drainDeadline = 10 * time.Second
)

// summariserWorkDir returns a dedicated, always-present working
// directory for the compactor/reviewer's `claude -p` subprocesses
// (🎯T82). The summariser is stateless — it summarises the prompt text,
// so the cwd's contents are irrelevant — hence a neutral directory under
// the OS temp root. It is deliberately NOT a repo checkout (the old
// hardcoded ~/work/github.com/marcelocantos/mnemo broke on every machine
// without that path) and deliberately has no CLAUDE.md ancestors, so the
// summariser loads no project instructions. Returns "" only if even the
// temp dir can't be created, signalling the caller to disable
// summarisation rather than spawn into a missing cwd.
func summariserWorkDir() string {
	dir := filepath.Join(os.TempDir(), "mnemo-summariser")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("summariser workdir unavailable; compaction and review disabled",
			"dir", dir, "err", err)
		return ""
	}
	return dir
}

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
		case "thread":
			cmdThread(os.Args[2:])
			return
		case "edge":
			cmdEdge(os.Args[2:])
			return
		}
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	helpAgent := flag.Bool("help-agent", false, "print agent guide and exit")
	addr := flag.String("addr", defaultAddr, "HTTP listen address")
	federatedAddr := flag.String("federated-addr", defaultFederatedAddr,
		"mTLS federated listen address (empty disables federation)")
	homeFlag := flag.String("home", "",
		"daemon home directory (overrides $MNEMO_HOME; defaults to OS user home). "+
			"Routes ~/.mnemo and the default-user data tree to this root. (🎯T73)")
	flag.Parse()

	// --home wins over MNEMO_HOME; both end up exported as MNEMO_HOME so
	// every store.EffectiveHome() call sees the same value regardless of
	// which entry point it came in through.
	if *homeFlag != "" {
		if err := os.Setenv(store.MnemoHomeEnv, *homeFlag); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", store.MnemoHomeEnv, err)
			os.Exit(1)
		}
	}

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

	// Install a signal-driven cancellable context so a foreground
	// SIGTERM/SIGINT (Ctrl+C, `brew services restart`, systemd stop) drives
	// the graceful drain in runServe instead of hard-killing the daemon
	// mid-request (🎯T97.1). The Windows-Service path handled its own
	// SCM-driven cancellation above and never reaches here.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runServe(ctx, *addr, *federatedAddr); err != nil {
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

// stringList is a repeatable flag.Value for multi --backend flags.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// cmdEdge runs the transparent MCP edge proxy (🎯T97.3). The edge owns
// the public listener and all client connections; backends serve on
// loopback HTTP or a Unix domain socket. Session affinity is by
// Mcp-Session-Id — standard MCP-over-HTTP, no custom protocol.
//
// When --route-file is set (or defaults to ~/.mnemo/edge-route.json and
// that file exists), the edge reloads primary index from the file so
// auto-apply can flip backends without restarting the edge (🎯T97.5).
func cmdEdge(args []string) {
	fs := flag.NewFlagSet("edge", flag.ExitOnError)
	listen := fs.String("listen", defaultAddr,
		"public listen address (edge owns client TCP connections)")
	primary := fs.Int("primary", 0,
		"index of the backend that receives new initialize requests")
	routeFile := fs.String("route-file", "",
		"optional edge-route.json path (default: ~/.mnemo/edge-route.json when present)")
	var backends stringList
	fs.Var(&backends, "backend",
		"backend base URL (repeatable). http://host:port or unix:///path")
	_ = fs.Parse(args)

	// Prefer explicit --backend flags; else load route file.
	routePath := *routeFile
	if routePath == "" {
		if home, err := store.EffectiveHome(); err == nil && upgrade.RouteConfigured(home) {
			routePath = upgrade.RoutePath(home)
		}
	}
	if len(backends) == 0 && routePath != "" {
		data, err := os.ReadFile(routePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "edge: read route file: %v\n", err)
			os.Exit(1)
		}
		var rf upgrade.RouteFile
		if err := json.Unmarshal(data, &rf); err != nil {
			fmt.Fprintf(os.Stderr, "edge: parse route file: %v\n", err)
			os.Exit(1)
		}
		backends = rf.Backends
		*primary = rf.Primary
	}
	if len(backends) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mnemo edge --backend URL [--backend URL ...] [--listen :19419] [--primary 0]")
		fmt.Fprintln(os.Stderr, "   or: mnemo edge --route-file ~/.mnemo/edge-route.json")
		fmt.Fprintln(os.Stderr, "edge: at least one --backend is required")
		os.Exit(2)
	}

	router, err := edgeproxy.NewRouter(backends, *primary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "edge: %v\n", err)
		os.Exit(1)
	}
	proxy := edgeproxy.NewProxy(router)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		// No Read/WriteTimeout — GET/SSE streams are long-lived.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Poll route file for backend growth + primary flips + repin.
	if routePath != "" {
		go watchEdgeRoute(ctx, routePath, router, proxy)
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("edge listening",
			"listen", *listen,
			"backends", len(backends),
			"primary", *primary,
			"route_file", routePath,
			"version", version,
		)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "edge: shutdown: %v\n", err)
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "edge: %v\n", err)
			os.Exit(1)
		}
	}
}

// watchEdgeRoute applies edge-route.json (grow backends + primary flip)
// and writes pin_counts so a draining backend can AffinityDrain until
// its pin count is zero (🎯T97.5). repin_all is crash-failover only.
func watchEdgeRoute(ctx context.Context, path string, router *edgeproxy.Router, proxy *edgeproxy.Proxy) {
	_ = proxy // clients grow lazily via Proxy.clientAt → syncClients
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastControl string // backends+primary+repin (not pin_counts)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var rf upgrade.RouteFile
			if err := json.Unmarshal(data, &rf); err != nil {
				continue
			}
			controlKey := fmt.Sprintf("%v|%d|%v", rf.Backends, rf.Primary, rf.RepinAll)
			if controlKey != lastControl {
				prim, err := router.ApplyRoute(rf.Backends, rf.Primary)
				if err != nil {
					slog.Warn("edge: apply route file", "err", err)
					continue
				}
				if rf.RepinAll {
					// Crash-failover only — not happy-path upgrade drain.
					n := upgrade.FailoverRepin(router.RepinAllToPrimary)
					slog.Info("edge: failover repin to primary", "moved", n, "primary", prim)
					rf.RepinAll = false
				}
				slog.Info("edge: route applied", "backends", router.BackendCount(), "primary", prim)
				lastControl = controlKey
			}
			// Always refresh pin_counts for AffinityDrain observers.
			// Atomic write so concurrent ReadRoute never sees partial JSON.
			rf.PinCounts = router.PinCounts()
			rf.Backends = router.BackendURLs()
			rf.Primary = router.PrimaryIndex()
			if err := upgrade.WriteRouteFile(path, rf); err != nil {
				slog.Warn("edge: write pin_counts", "err", err)
			}
		}
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

	// The compactor and CLAUDE.md reviewer spawn `claude -p` to
	// summarise transcript text; that subprocess only needs a valid
	// directory to chdir into — its contents are irrelevant to the
	// stateless summarisation. Use a dedicated dir under the OS temp
	// root: it always exists (created here), has no CLAUDE.md ancestors
	// so the summariser loads no project context, and never depends on a
	// developer checkout being present (🎯T82). Empty only when even the
	// temp dir can't be created — the registry then disables
	// summarisation rather than spawning into a missing cwd.
	summariserDir := summariserWorkDir()

	reg := registry.NewRegistry(ctx, cfg, summariserDir)
	defer reg.Close()

	// 🎯T102.2: plugin registry reconciles against config on startup and
	// on every mnemo_config hot-reload. Home is the process effective
	// home for ~/.mnemo/plugins/<name> path convention.
	if pluginHome, err := store.EffectiveHome(); err == nil {
		reg.SetPluginManager(plugin.NewManager(pluginHome, nil, slog.Default()))
	} else {
		slog.Warn("plugin manager disabled (no home)", "err", err)
	}
	if n := len(cfg.Plugins); n > 0 {
		slog.Info("plugins configured", "count", n)
	}

	// 🎯T97: upgrade detector, background lease, notices, auto-apply.
	// Lease is acquired before eager ForUser so startWorkers can gate
	// singleton background work on holder status.
	upgradeNotices := upgrade.NewNoticeTracker()
	activeSessions := upgrade.NewSessionSet()
	// listChanged is registered after mcpSrv is built (orchestrator is
	// wired earlier). Best-effort tools/list_changed on version change.
	var listChangedHolder upgrade.ListChangedHolder
	upgradeFX := &upgrade.SideEffects{
		Notices: upgradeNotices,
		ListChanged: func(from, to string) {
			listChangedHolder.Send(from, to)
		},
	}
	var lastMCPActivity atomicTime
	lastMCPActivity.Set(time.Now())

	homeForLease, homeErr := store.EffectiveHome()
	var pendingUpgradeFrom, pendingUpgradeTo string
	if homeErr == nil {
		// Load once: allowlisted sessions only; file is deleted (🎯T97.6).
		if p, ok, err := upgrade.LoadAndConsumePending(homeForLease, upgradeNotices); err != nil {
			slog.Warn("upgrade pending notice load failed", "err", err)
		} else if ok {
			pendingUpgradeFrom, pendingUpgradeTo = p.From, p.To
		}
	}

	var bgLease *upgrade.Lease
	if homeErr == nil {
		var lerr error
		bgLease, lerr = upgrade.NewLease(&upgrade.LeaseArgs{
			Path:     upgrade.DefaultLeasePath(homeForLease),
			HolderID: upgrade.DefaultHolderID(),
		})
		if lerr != nil {
			slog.Warn("background lease unavailable", "err", lerr)
		} else {
			reg.SetLease(bgLease)
			if ok, aerr := bgLease.TryAcquire(); aerr != nil {
				slog.Warn("background lease acquire failed", "err", aerr)
			} else if ok {
				slog.Info("background lease acquired", "holder", upgrade.DefaultHolderID())
				go bgLease.RunHeartbeatLoop(ctx.Done(), 0)
			} else {
				slog.Info("background lease held by another process; serve-only mode")
			}
			// Retry acquire periodically so a handoff after peer drain works.
			go func() {
				t := time.NewTicker(time.Second)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						if bgLease.Held() {
							continue
						}
						ok, err := bgLease.TryAcquire()
						if err != nil || !ok {
							continue
						}
						slog.Info("background lease acquired (handoff)")
						go bgLease.RunHeartbeatLoop(ctx.Done(), 0)
						reg.EnsureBackgroundWorkers()
					}
				}
			}()
		}
	}

	upgradeDetector := upgrade.NewDetector(&upgrade.DetectorArgs{
		CurrentVersion: version,
		Fetch:          upgrade.GHReleaseFetcher(""),
		Disabled:       cfg.DisableUpgradeCheck,
		MinInterval:    6 * time.Hour,
	})
	reg.SetUpgradeDetector(upgradeDetector)
	go runUpgradeDetectorLoop(ctx, upgradeDetector)

	// Auto-apply (🎯T97.5). Homebrew-only. Sequence:
	//   apply → OnUpgrade (pending + in-process marks) →
	//   spawn (sibling loads pending) → flip primary (pins stay) →
	//   AffinityDrain (wait pin_counts[self]==0, then SIGTERM).
	// Never repin_all on this path — that invalidates mcp-go sessions.
	orchQuiescence, _ := cfg.AutoUpgrade.EffectiveQuiescence()
	var spawnedBackendURL string
	selfBackendURL := backendSelfURL(addr)
	homebrewInstall := isHomebrewInstall()
	autoOrch := upgrade.NewOrchestrator(&upgrade.OrchestratorArgs{
		Enabled:    cfg.AutoUpgrade.Enabled,
		Env:        upgrade.ApplyEnv{Homebrew: homebrewInstall, GOOS: ""},
		Quiescence: orchQuiescence,
		Detector:   upgradeDetector,
		LastActivity: func() time.Time {
			return lastMCPActivity.Get()
		},
		Apply: func(ctx context.Context) error {
			return runBrewUpgrade(ctx)
		},
		SpawnBackend: func(ctx context.Context) error {
			if homeForLease == "" || !upgrade.RouteConfigured(homeForLease) {
				slog.Info("auto-upgrade: single-daemon mode (no edge-route); new binary ready")
				return nil
			}
			url, err := spawnLoopbackBackend(ctx)
			if err != nil {
				return err
			}
			spawnedBackendURL = url
			slog.Info("auto-upgrade: spawned backend", "url", url)
			return nil
		},
		FlipPrimary: func(ctx context.Context) error {
			if homeForLease == "" || !upgrade.RouteConfigured(homeForLease) {
				return nil
			}
			if spawnedBackendURL == "" {
				return fmt.Errorf("auto-upgrade: no spawned backend to flip to")
			}
			r, err := upgrade.AppendBackend(homeForLease, spawnedBackendURL)
			if err != nil {
				return err
			}
			slog.Info("auto-upgrade: edge primary flipped (affinity pins unchanged)",
				"primary", r.Primary, "url", spawnedBackendURL)
			return nil
		},
		DrainOld: func(ctx context.Context) error {
			reg.ReleaseLease()
			if homeForLease == "" || !upgrade.RouteConfigured(homeForLease) {
				slog.Info("auto-upgrade: brew services restart mnemo")
				return runBrewServicesRestart(ctx)
			}
			// Affinity drain: keep serving pinned sessions until edge
			// reports pin_counts[self]==0, then graceful SIGTERM.
			selfIdx := -1
			if rf, err := upgrade.ReadRoute(homeForLease); err == nil {
				selfIdx = rf.IndexOfBackend(selfBackendURL)
			}
			if selfIdx < 0 {
				slog.Warn("auto-upgrade: self not in edge-route; signaling immediately",
					"self", selfBackendURL)
				return signalSelfSIGTERM()
			}
			maxWait := orchQuiescence
			if maxWait <= 0 {
				maxWait = upgrade.DefaultDrainMaxWait
			}
			if d := os.Getenv("MNEMO_DRAIN_MAX_WAIT"); d != "" {
				if parsed, err := time.ParseDuration(d); err == nil {
					maxWait = parsed
				}
			}
			slog.Info("auto-upgrade: affinity drain waiting for pins to clear",
				"self_idx", selfIdx, "max_wait", maxWait)
			return upgrade.AffinityDrain(ctx, &upgrade.AffinityDrainArgs{
				BackendIdx: selfIdx,
				Pins: func(idx int) int {
					rf, err := upgrade.ReadRoute(homeForLease)
					if err != nil {
						// Never treat I/O/parse failure as zero pins —
						// that would SIGTERM with live sessions.
						return upgrade.PinUnknown
					}
					return rf.PinCountAt(idx)
				},
				MaxWait:      maxWait,
				PollInterval: upgrade.DefaultDrainPoll,
				Reap: func(ctx context.Context) error {
					slog.Info("auto-upgrade: pins clear (or max wait); SIGTERM self")
					return signalSelfSIGTERM()
				},
			})
		},
		OnUpgrade: func(from, to string) {
			slog.Info("auto-upgrade: version change side effects", "from", from, "to", to)
			sessions := activeSessions.Snapshot()
			// Banners + best-effort tools/list_changed (🎯T97.6 / criterion 4).
			upgradeFX.OnVersionChange(sessions, from, to)
			if homeForLease == "" {
				return
			}
			if err := upgrade.WritePendingNotice(homeForLease, upgrade.PendingNotice{
				From:     from,
				To:       to,
				Sessions: sessions,
			}); err != nil {
				slog.Warn("auto-upgrade: write pending notice", "err", err)
			}
		},
	})
	if cfg.AutoUpgrade.Enabled {
		mode := "notify_only"
		if homebrewInstall {
			mode = "brew_upgrade+services_restart"
			if homeForLease != "" && upgrade.RouteConfigured(homeForLease) {
				mode = "edge_affinity_drain"
			}
		}
		slog.Info("auto-upgrade: armed",
			"quiescence", orchQuiescence, "mode", mode, "phase", autoOrch.Phase())
	}
	go runAutoApplyLoop(ctx, autoOrch)

	// Determine the default username — used when a request arrives
	// without an explicit ?user=<name> query parameter. On a Windows
	// Service deployment (running as LocalSystem) there is no
	// sensible default, so every request MUST carry a user.
	defaultUser, defErr := store.DefaultUsername()
	if defErr != nil {
		slog.Info("no default user — requests must include ?user=<name>", "reason", defErr)
	} else {
		slog.Info("default user", "user", defaultUser)
		// Eager-start the default user's per-user workers (compactor,
		// reviewer, CI poller, backup worker, etc.) at daemon boot. Without
		// this, ForUser is only invoked when the first MCP tool call lands
		// — so a daemon nobody pokes never starts its backup worker, never
		// runs ingest, etc. (🎯T62).
		//
		// Multi-user lazy startup still works: ForUser(otherUser) for a
		// non-default user keeps firing on demand.
		if _, err := reg.ForUser(defaultUser); err != nil {
			slog.Error("eager startup failed for default user", "user", defaultUser, "err", err)
			return err
		}
	}

	// Self-diagnostics (🎯T83). A registry of health checks (summariser
	// workdir, claude on PATH, configured roots, the compaction breaker,
	// backfill-since-startup, db responsiveness) driven by a scheduler:
	// the full suite at startup, fast checks every few minutes, the full
	// suite hourly. A fail-severity transition fires an opt-out OS
	// notification that deep-links to the dashboard health page. The same
	// registry backs the /health endpoint and the mnemo_doctor tool.
	daemonStart := time.Now()
	diagReg := reg.BuildDiagRegistry(defaultUser, daemonStart)
	dashHost := addr
	if strings.HasPrefix(addr, ":") {
		dashHost = "localhost" + addr
	}
	notifyCfg := diag.DefaultNotifierConfig("http://" + dashHost + "/#health")
	if cfg.DisableHealthNotifications {
		notifyCfg.Enabled = false
	}
	// The SSE hub (🎯T86) fans diagnostics out to the native menu-bar shim. The
	// notifier routes alerts to the shim when one is connected (a richer native
	// notification) and falls back to osascript/notify-send when headless; the
	// scheduler streams every report so the shim's dashboard panel and status
	// glyph stay live. The hub is also handed to the api handler below.
	eventHub := api.NewEventHub()
	notifier := diag.NewNotifier(notifyCfg)
	notifier.OnAlert(func(a diag.Alert) {
		eventHub.Publish(api.Event{Type: "alert", Data: a})
	})
	notifier.SetShimPresent(eventHub.HasSubscribers)
	diagScheduler := diag.NewScheduler(diagReg, notifier, 0, 0)
	diagScheduler.OnReport(func(rep diag.Report) {
		// Retained: a shim connecting between scheduler ticks gets the current
		// health snapshot immediately rather than waiting for the next tick.
		eventHub.PublishRetained(api.Event{Type: "health", Data: rep})
	})
	go diagScheduler.Run(ctx)

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

	// effectiveUser maps an empty username (a request that omitted
	// ?user=) to the default user, mirroring resolve's fallback. Returns
	// ("", false) only on the Windows-Service path where there is no
	// default identity (defErr != nil); callers then resolve to nil. The
	// per-tool vault/compactor resolvers below route through this so they
	// reach the default user's instance exactly like every Backend tool
	// (🎯T79) — without it they returned "not configured" whenever the
	// request omitted ?user=.
	effectiveUser := func(username string) (string, bool) {
		if username == "" {
			if defErr != nil {
				return "", false
			}
			return defaultUser, true
		}
		return username, true
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
	// Best-effort tools/list_changed after upgrade (🎯T97.6). listChanged
	// is void-safe when no clients are connected.
	listChangedHolder.Set(func(from, to string) {
		upgrade.BroadcastListChanged(mcpSrv, from, to)
		slog.Info("auto-upgrade: tools/list_changed notified", "from", from, "to", to)
	})
	// Sibling process that loaded upgrade-pending: notify any clients that
	// attach to this new backend (best-effort).
	if pendingUpgradeFrom != "" {
		listChangedHolder.Send(pendingUpgradeFrom, pendingUpgradeTo)
	}
	handler := tools.NewHandler(resolve)

	// Wire the per-user vault syncer resolver so mnemo_vault_sync and
	// mnemo_vault_status work. Returns nil when vault is not configured
	// for the requested user; the tool handlers gracefully report this.
	handler.SetVaultResolver(func(username string) tools.VaultSyncer {
		u, ok := effectiveUser(username)
		if !ok {
			return nil
		}
		v := reg.VaultFor(u)
		if v == nil {
			return nil // avoid (*vault.Exporter)(nil) wrapped in interface
		}
		return v
	})

	// Wire the per-user compactor health reporter so
	// mnemo_compactor_status (🎯T67) can surface live watcher state.
	// Returns nil when the user's workers haven't started yet; the
	// tool gracefully reports "not available" in that case.
	handler.SetCompactorResolver(func(username string) tools.CompactorHealthReporter {
		u, ok := effectiveUser(username)
		if !ok {
			return nil
		}
		w := reg.CompactWatcherFor(u)
		if w == nil {
			return nil
		}
		return compactorAdapter{w: w}
	})

	// Wire the mnemo_config tool to the live Registry. Get returns a
	// snapshot of the current Config; Put persists the new config to
	// disk via store.WriteConfig (which re-runs the same validation as
	// LoadConfig) and asks the Registry to adopt it across every
	// already-initialised per-user Store.
	// The menu-bar shim supervisor (🎯T85.5) honours the menu_bar_app flag and
	// is hot-reloadable: configController.Put pokes it on every config change,
	// so toggling menu_bar_app via mnemo_config takes effect immediately, with
	// no daemon restart.
	shimSup := newShimSupervisor()
	shimSup.SetEnabled(cfg.MenuBarApp)
	handler.SetConfigController(configController{reg: reg, shim: shimSup, autoOrch: autoOrch})

	// Wire the self-diagnostics registry into mnemo_doctor (🎯T83) — the
	// same registry that backs the /health endpoint and the scheduler.
	handler.SetDiagRunner(diagReg)

	// 🎯T97.6: one-time upgrade notice on tool results (allowlisted sessions).
	handler.SetUpgradeNotices(upgradeNotices)

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

	// httpSrv implements http.Handler so we can mount it inside our
	// own mux alongside the dashboard and REST API routes.
	httpSrv := server.NewStreamableHTTPServer(mcpSrv,
		server.WithStateful(true),
		server.WithHTTPContextFunc(tools.UsernameContextFunc),
		// Keep the GET (SSE) stream warm so NAT / OS keepalive doesn't
		// collapse it during idle stretches. Without this, an idle
		// Claude Code session sees "MCP error -32000: Connection closed"
		// on the first tool call after a few minutes of silence.
		server.WithHeartbeatInterval(30*time.Second),
	)

	// Wire up a single mux that serves:
	//   /mcp          → MCP streamable-HTTP endpoint (for Claude Code)
	//   /api/*        → JSON REST API (for the dashboard)
	//   /plugins/*    → reverse-proxy to ready plugin instances (🎯T102.5)
	//   /             → Web dashboard UI
	mux := http.NewServeMux()
	// Track MCP traffic for auto-apply quiescence (🎯T97.5) and remember
	// live session IDs so OnUpgrade can allowlist spanning sessions only.
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMCPActivity.Set(time.Now())
		if sid := r.Header.Get(edgeproxy.SessionIDHeader); sid != "" {
			activeSessions.Add(sid)
		}
		httpSrv.ServeHTTP(w, r)
	})
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler) // catch sub-paths used by the MCP transport
	apiHandler := api.New(resolve)
	apiHandler.SetDiagRunner(diagReg) // 🎯T83: serve GET /health from the diag registry
	apiHandler.SetEventHub(eventHub)  // 🎯T86: serve GET /api/events (SSE) from the hub
	apiHandler.RegisterRoutes(mux)

	// 🎯T102.5: reverse-proxy /plugins/<name>/* to ready instances (same
	// origin as the dashboard; WS/SSE pass through). Unknown/disabled
	// names 404. Manager may be nil when EffectiveHome failed at startup.
	mux.Handle("/plugins/", plugin.ProxyHandler(reg.PluginManager()))

	// 🎯T92: opt-in pprof on the local listener. Diagnosing the CPU burn
	// that motivated T91/T92 was slow because the release binary is stripped
	// and has no profiling surface — every investigation fell back to
	// `sample`/lldb across the cgo boundary. Gated behind MNEMO_PPROF=1
	// (off by default — pprof exposes runtime internals and a debug surface)
	// so the next "why is mnemo hot?" is a 30-second `go tool pprof`, not a
	// multi-hour delve session. Local listener only; never on the federated
	// mTLS port.
	if os.Getenv("MNEMO_PPROF") == "1" {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		slog.Info("pprof enabled (MNEMO_PPROF=1)", "url", "http://localhost"+addr+"/debug/pprof/")
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})

	httpServer := &http.Server{Addr: addr, Handler: mux}

	slog.Info("mnemo serve starting", "version", version, "addr", addr)

	// Run the local HTTP server in a goroutine so we can react to ctx
	// cancellation (triggered by the Windows Service handler on SCM
	// Stop, or never triggered in the foreground case).
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	// Start the menu-bar shim supervisor (🎯T85.5). It honours menu_bar_app
	// (initial value set above) and reacts to live toggles via Put, so the
	// menu-bar app is opt-in and the toggle needs no restart. The Threads
	// daemon API (mnemo_thread_* tools, the `mnemo thread` CLI, the HTTP
	// thread routes) stays available unconditionally; only the menu-bar app
	// is gated. Best-effort, macOS-only; a no-op when no Mnemo.app is found.
	go shimSup.run(ctx)

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
		slog.Info("mnemo serve draining", "deadline", drainDeadline)
		// Ordered graceful drain (🎯T97.1), bounded by drainDeadline:
		//   1. stop intake      — refuse new MCP/HTTP/federated requests,
		//                          let in-flight ones finish
		//   2. release lease     — seam for 🎯T97.4 (no-op until it exists)
		//   3. stop workers      — cancel per-user worker contexts
		//   4. quiesce read pool — close each readDB
		//   5. checkpoint writer — PRAGMA wal_checkpoint(TRUNCATE)
		// Steps 3–5 are driven by reg.Close(), which is idempotent, so the
		// deferred reg.Close() above is a harmless second call. If the whole
		// sequence overruns drainDeadline we hard-exit — safe under crash-only
		// durability (residual WAL frames replay on the next start).
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainDeadline)
		defer cancelDrain()
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			// 1. Stop intake, draining in-flight requests within the deadline.
			slog.Info("drain: stopping HTTP MCP intake")
			if err := httpSrv.Shutdown(drainCtx); err != nil {
				slog.Warn("HTTP MCP shutdown error", "err", err)
			}
			slog.Info("drain: stopping HTTP server intake")
			if err := httpServer.Shutdown(drainCtx); err != nil {
				slog.Warn("HTTP server shutdown error", "err", err)
			}
			if fedSrv != nil {
				slog.Info("drain: stopping federated intake")
				if err := fedSrv.Shutdown(drainCtx); err != nil {
					slog.Warn("federated HTTP shutdown error", "err", err)
				}
			}
			// 2. Release singleton background lease (🎯T97.4) so a peer
			// backend can acquire and start ingest/compaction.
			slog.Info("drain: releasing background lease")
			reg.ReleaseLease()
			// 3–5. Stop workers, quiesce read pools, checkpoint each writer.
			slog.Info("drain: stopping workers + checkpointing stores")
			reg.Close()
		}()
		select {
		case <-drained:
			slog.Info("mnemo serve drained cleanly")
			return nil
		case <-drainCtx.Done():
			slog.Warn("drain exceeded deadline; forcing exit (crash-only recovery on next start)",
				"deadline", drainDeadline)
			os.Exit(0)
			return nil // unreachable; os.Exit does not return
		}
	}
}

// atomicTime is a mutex-guarded time.Time for MCP activity tracking.
type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) Set(t time.Time) {
	a.mu.Lock()
	a.t = t
	a.mu.Unlock()
}

func (a *atomicTime) Get() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.t
}

func runUpgradeDetectorLoop(ctx context.Context, d *upgrade.Detector) {
	// Immediate check at startup, then every hour (detector still
	// enforces its own MinInterval for the gh call).
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	do := func() {
		cr := d.Check(ctx)
		if cr.Err != nil {
			slog.Debug("upgrade check", "err", cr.Err)
			return
		}
		if cr.UpgradeAvailable {
			slog.Info("upgrade available", "detail", cr.Detail)
		}
	}
	do()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			do()
		}
	}
}

func runAutoApplyLoop(ctx context.Context, o *upgrade.Orchestrator) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := o.Tick(ctx); err != nil {
				slog.Warn("auto-upgrade tick", "err", err)
			}
		}
	}
}

func isHomebrewInstall() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	exe, _ = filepath.EvalSymlinks(exe)
	return strings.Contains(exe, "/Cellar/mnemo/") ||
		strings.Contains(exe, "/homebrew/") ||
		strings.Contains(exe, "/linuxbrew/")
}

func runBrewUpgrade(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "brew", "upgrade", "mnemo")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("brew upgrade mnemo: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runBrewServicesRestart(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "brew", "services", "restart", "mnemo")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("brew services restart mnemo: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func signalSelfSIGTERM() error {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

// backendSelfURL is the loopback base URL peers/edge use for this
// process when it listens on addr (e.g. ":19421" → "http://127.0.0.1:19421").
func backendSelfURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// addr may be ":19421"
		if strings.HasPrefix(addr, ":") {
			return "http://127.0.0.1" + addr
		}
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// spawnLoopbackBackend starts a sibling mnemo daemon on a free loopback
// port for edge-mediated handoff (🎯T97.5). Returns the base URL.
func spawnLoopbackBackend(ctx context.Context) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	cmd := exec.CommandContext(ctx, exe, "--addr", addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("spawn backend: %w", err)
	}
	// Detach: do not wait; the new process is supervised by the OS /
	// brew. Record PID for diagnostics.
	slog.Info("spawned backend process", "pid", cmd.Process.Pid, "addr", addr)
	go func() { _ = cmd.Wait() }()
	// Brief readiness wait: poll TCP until accept or timeout.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return "http://" + addr, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return "http://" + addr, nil // return anyway; edge will 502 until ready
}

// compactorAdapter satisfies tools.CompactorHealthReporter by
// projecting a *compact.Watcher's HealthSnapshot into the tools
// package's CompactorHealth type. Lives in main so the tools and
// compact packages stay free of each other (compact has no business
// importing tools and vice versa).
type compactorAdapter struct {
	w *compact.Watcher
}

func (a compactorAdapter) Health() tools.CompactorHealth {
	hs := a.w.Health()
	return tools.CompactorHealth{
		LastScanAt:            hs.LastScanAt,
		LastScanCount:         hs.LastScanCount,
		Backlog:               hs.Backlog,
		Quarantined:           hs.Quarantined,
		LastTickAt:            hs.LastTickAt,
		LastTickOutcome:       hs.LastTickOutcome,
		InFlightSession:       hs.InFlightSession,
		Counts:                hs.Counts,
		ScanInterval:          hs.ScanInterval,
		TickTimeout:           hs.TickTimeout,
		AddendaBudgetTokens:   hs.AddendaBudgetTokens,
		MaxCompactionsPerScan: hs.MaxCompactionsPerScan,
		MaxTokenRatio:         hs.MaxTokenRatio,
	}
}

// configController adapts a *registry.Registry to tools.ConfigController.
// Defined in main rather than in registry to keep registry from
// importing the tools package (a cycle we already avoid for
// dependency hygiene).
type configController struct {
	reg      *registry.Registry
	shim     *shimSupervisor
	autoOrch *upgrade.Orchestrator
}

func (c configController) Get() store.Config {
	return c.reg.CurrentConfig()
}

func (c configController) Put(newCfg store.Config) (tools.ConfigReport, error) {
	old := c.reg.CurrentConfig()
	if err := store.WriteConfig(newCfg); err != nil {
		return tools.ConfigReport{}, err
	}
	rep := c.reg.Reload(newCfg)
	// Adopt menu_bar_app live: start/stop supervising the menu-bar app
	// without a daemon restart (🎯T85.5).
	if c.shim != nil {
		c.shim.SetEnabled(newCfg.MenuBarApp)
	}
	// Adopt auto_upgrade live (enabled + quiescence). Apply still only
	// runs on Homebrew non-Windows installs (orchestrator NotifyOnly).
	if c.autoOrch != nil {
		auChanged := old.AutoUpgrade.Enabled != newCfg.AutoUpgrade.Enabled ||
			old.AutoUpgrade.Quiescence != newCfg.AutoUpgrade.Quiescence
		if auChanged {
			rep.Changed = append(rep.Changed, "auto_upgrade")
			c.autoOrch.SetEnabled(newCfg.AutoUpgrade.Enabled)
			if q, err := newCfg.AutoUpgrade.EffectiveQuiescence(); err == nil {
				c.autoOrch.SetQuiescence(q)
			}
			rep.Adopted = append(rep.Adopted, "auto_upgrade")
		}
	}
	return tools.ConfigReport{
		Changed:         rep.Changed,
		Adopted:         rep.Adopted,
		RequiresRestart: rep.RequiresRestart,
		Warnings:        rep.Warnings,
	}, nil
}
