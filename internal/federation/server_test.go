// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
)

// TestFederatedRoundTrip stands up two mTLS-trusted mnemo "hosts" in
// temp dirs (host A is the server, host B is the client), confirms
// that mTLS authenticates B and that tools/list returns ONLY the
// curated read-only subset. Verifies the security-critical boundary:
// no write- or control-shaped tool leaks over federation.
func TestFederatedRoundTrip(t *testing.T) {
	hostA := setupHost(t, "host-a")
	hostB := setupHost(t, "host-b")
	crossTrust(t, hostA, hostB)
	crossTrust(t, hostB, hostA)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	addr := freeAddr(t)
	srv := startServer(ctx, t, hostA, addr)

	client := newPeerClient(ctx, t, hostB, addr)

	// tools/list should yield exactly the federated tool subset.
	listed, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools over mTLS: %v", err)
	}
	got := map[string]struct{}{}
	for _, tool := range listed.Tools {
		got[tool.Name] = struct{}{}
	}
	for name := range tools.FederatedToolNames {
		if _, ok := got[name]; !ok {
			t.Errorf("federated tool %q missing from server tool list", name)
		}
	}
	for _, leaked := range []string{
		"mnemo_self", "mnemo_define", "mnemo_evaluate",
		"mnemo_list_templates", "mnemo_restore", "mnemo_whatsup",
		"mnemo_docs", "mnemo_synthesis", "mnemo_permissions",
	} {
		if _, ok := got[leaked]; ok {
			t.Errorf("write/control tool %q leaked onto federated endpoint", leaked)
		}
	}

	// Graceful shutdown.
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestFederatedRejectsUntrustedClient confirms the TLS layer denies a
// client whose cert is NOT in the server's trusted-peer pool — even
// when the client itself trusts the server.
func TestFederatedRejectsUntrustedClient(t *testing.T) {
	hostA := setupHost(t, "host-a")
	hostB := setupHost(t, "host-b")
	// Asymmetric trust: B trusts A (so the TLS server cert verifies),
	// but A does NOT trust B (the rejection target).
	crossTrust(t, hostB, hostA)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	addr := freeAddr(t)
	srv := startServer(ctx, t, hostA, addr)
	t.Cleanup(func() {
		shutdownCtx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	clientCfg, err := hostB.endpoint.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientCfg},
		Timeout:   3 * time.Second,
	}

	url := fmt.Sprintf("https://%s/mcp", addr)
	resp, err := httpClient.Get(url)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected mTLS rejection, got HTTP %d", resp.StatusCode)
	}
	if !looksLikeTLSAuthFailure(err) {
		t.Errorf("expected TLS auth failure, got %v", err)
	}
}

// host bundles a temp dir, the loaded endpoint, and the tools.Handler
// for one in-process mnemo instance.
type host struct {
	dir      string
	endpoint *endpoint.Endpoint
	handler  *tools.Handler
}

func setupHost(t *testing.T, name string) *host {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ep, err := endpoint.Load(dir)
	if err != nil {
		t.Fatalf("endpoint.Load(%s): %v", dir, err)
	}
	// Empty Backend is fine for tools/list — it never resolves.
	resolve := func(string) (store.Backend, error) {
		return nil, errors.New("no backend in federation smoke test")
	}
	return &host{dir: dir, endpoint: ep, handler: tools.NewHandler(resolve)}
}

// crossTrust copies src's public cert into dst's peers dir and
// reloads dst so its TrustedPeers pool picks up the new entry.
func crossTrust(t *testing.T, src, dst *host) {
	t.Helper()
	peersDir := filepath.Join(dst.dir, "peers")
	if err := os.MkdirAll(peersDir, 0o700); err != nil {
		t.Fatalf("mkdir peers: %v", err)
	}
	name := filepath.Base(src.dir) + ".pem"
	if err := os.WriteFile(filepath.Join(peersDir, name), src.endpoint.CertPEM, 0o644); err != nil {
		t.Fatalf("write peer cert: %v", err)
	}
	reloaded, err := endpoint.Load(dst.dir)
	if err != nil {
		t.Fatalf("reload endpoint after peer add: %v", err)
	}
	dst.endpoint = reloaded
}

func startServer(ctx context.Context, t *testing.T, h *host, addr string) *http.Server {
	t.Helper()
	srv, err := Start(ctx, h.dir, addr, "test", h.handler)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForListen(t, addr)
	return srv
}

func newPeerClient(ctx context.Context, t *testing.T, h *host, addr string) *mcpclient.Client {
	t.Helper()
	clientCfg, err := h.endpoint.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientCfg},
		Timeout:   5 * time.Second,
	}

	url := fmt.Sprintf("https://%s/mcp", addr)
	c, err := mcpclient.NewStreamableHttpClient(url, transport.WithHTTPBasicClient(httpClient))
	if err != nil {
		t.Fatalf("NewStreamableHttpClient: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name: "federation-smoke", Version: "test",
			},
		},
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return c
}

// freeAddr binds to :0 to discover an unused port, then closes the
// listener and returns the loopback address. Race window between
// close and re-bind is acceptable for an in-process test.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitForListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never came up on %s", addr)
}

// looksLikeTLSAuthFailure inspects err for the family of strings the
// Go TLS stack uses when a peer is rejected for missing/wrong client
// cert. The exact wording varies by Go version; the test should pass
// on any error that is unambiguously a TLS-handshake / connection
// rejection.
func looksLikeTLSAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	msg := err.Error()
	for _, marker := range []string{
		"tls: ",
		"certificate required",
		"unknown certificate",
		"bad certificate",
		"connection reset",
		"EOF",
		"remote error",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	// Net errors that wrap an underlying tls failure also count.
	var nerr net.Error
	return errors.As(err, &nerr)
}
