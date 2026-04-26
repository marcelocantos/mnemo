// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package federation runs mnemo's mTLS-authenticated federated MCP
// endpoint (🎯T15.3). It composes the endpoint package's TLS material
// with the tools package's curated read-only tool subset and the
// mark3labs/mcp-go streamable HTTP server.
//
// The federated server is a separate http.Server bound to its own
// listen address (default :19420), distinct from the local
// :19419 endpoint that local Claude Code instances use. Only tools
// in tools.FederatedToolNames are registered, so write- or
// control-shaped tools cannot be invoked over federation regardless
// of caller identity.
//
// Trust is per-peer mTLS: each side's self-signed cert is exchanged
// out of band and placed under ~/.mnemo/peers/. The TLS handshake
// rejects any client whose certificate is not in the trusted-peer
// pool; tool authorisation beyond that is delegated to whatever the
// individual tool handlers enforce (read-only by construction).
package federation

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/tools"
)

// Start brings up the federated mTLS listener. mnemoDir is the
// daemon's state directory (typically ~/.mnemo) — endpoint material
// (cert, key, trusted peers) is loaded from it. addr is the listen
// address; an empty string is treated as "federation disabled" by the
// caller and should never reach Start. version is the daemon version
// reported in MCP server metadata.
//
// Returns the running *http.Server (call Shutdown for graceful stop)
// and the listener it bound to. Errors from this function are
// non-fatal — main.go logs and continues serving the local endpoint.
//
// The returned server stops automatically when ctx is cancelled.
func Start(
	ctx context.Context,
	mnemoDir, addr, version string,
	h *tools.Handler,
) (*http.Server, error) {
	ep, err := endpoint.Load(mnemoDir)
	if err != nil {
		return nil, fmt.Errorf("load endpoint: %w", err)
	}
	tlsCfg, err := ep.ServerTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("build server TLS config: %w", err)
	}

	mcp := mcpserver.NewMCPServer(
		"mnemo-federated",
		version,
		mcpserver.WithToolCapabilities(true),
	)
	h.RegisterFederatedTools(mcp)

	httpHandler := mcpserver.NewStreamableHTTPServer(mcp,
		mcpserver.WithStateful(true),
		mcpserver.WithHTTPContextFunc(tools.UsernameContextFunc),
		mcpserver.WithHeartbeatInterval(30*time.Second),
	)

	httpSrv := &http.Server{
		Addr:      addr,
		Handler:   httpHandler,
		TLSConfig: tlsCfg,
	}

	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}

	slog.Info("mnemo federated serve starting",
		"addr", addr, "trusted_peers", ep.PeerNames)

	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("federated HTTP server failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	return httpSrv, nil
}
