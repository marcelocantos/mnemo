// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/store"
)

func TestClientCallToolHappyPath(t *testing.T) {
	server, client := stubFederationPair(t, withStubTool("mnemo_stub", `{"ok":true}`))
	t.Cleanup(server.shutdown)
	t.Cleanup(client.Close)

	res, err := client.CallTool(ctx(t), server.peerName, "mnemo_stub", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res)
	}
	got := textContent(res)
	if !strings.Contains(got, `"ok":true`) {
		t.Errorf("unexpected payload: %q", got)
	}
}

func TestClientCallToolUnknownInstance(t *testing.T) {
	server, client := stubFederationPair(t)
	t.Cleanup(server.shutdown)
	t.Cleanup(client.Close)

	_, err := client.CallTool(ctx(t), "not-a-peer", "mnemo_stub", nil)
	if !errors.Is(err, ErrUnknownInstance) {
		t.Errorf("got %v, want ErrUnknownInstance", err)
	}
}

func TestClientCallToolCertMismatch(t *testing.T) {
	// Build a stub server normally, then point the client at a peer
	// whose pinned cert is some OTHER cert — the TLS handshake must
	// fail with ErrTLSHandshake.
	server, _ := stubFederationPair(t, withStubTool("mnemo_stub", `{"ok":true}`))
	t.Cleanup(server.shutdown)

	// Spin up an unrelated endpoint to source a wrong cert PEM.
	wrongDir := filepath.Join(t.TempDir(), "wrong")
	wrongEP, err := endpoint.Load(wrongDir)
	if err != nil {
		t.Fatalf("load wrong endpoint: %v", err)
	}

	clientHostDir := filepath.Join(t.TempDir(), "client")
	clientEP, err := endpoint.Load(clientHostDir)
	if err != nil {
		t.Fatalf("load client endpoint: %v", err)
	}
	// Server still trusts the client (so client cert isn't the
	// failure mode), but client trusts the WRONG cert as the peer.
	addPeer(t, server.endpoint, "client", clientEP.CertPEM)
	server.reload(t)

	c, err := NewClient(clientEP, []store.LinkedInstance{{
		Name:     "stub",
		URL:      server.url,
		PeerCert: string(wrongEP.CertPEM),
	}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	_, err = c.CallTool(ctx(t), "stub", "mnemo_stub", nil)
	if !errors.Is(err, ErrTLSHandshake) {
		t.Errorf("got %v, want ErrTLSHandshake", err)
	}
}

func TestClientCallToolConnectionRefused(t *testing.T) {
	// Bind a port, close it, then aim the client at it. The connect
	// will fail with "connection refused".
	addr := freeAddr(t)

	clientDir := filepath.Join(t.TempDir(), "client")
	clientEP, err := endpoint.Load(clientDir)
	if err != nil {
		t.Fatalf("load client endpoint: %v", err)
	}
	otherDir := filepath.Join(t.TempDir(), "other")
	otherEP, err := endpoint.Load(otherDir)
	if err != nil {
		t.Fatalf("load other endpoint: %v", err)
	}
	c, err := NewClient(clientEP, []store.LinkedInstance{{
		Name:     "down",
		URL:      "https://" + addr + "/mcp",
		PeerCert: string(otherEP.CertPEM),
	}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	_, err = c.CallTool(ctx(t), "down", "mnemo_stub", nil)
	if !errors.Is(err, ErrConnectionRefused) {
		t.Errorf("got %v, want ErrConnectionRefused", err)
	}
}

func TestClientCallToolTimeout(t *testing.T) {
	server, client := stubFederationPair(t,
		withStubTool("mnemo_slow", `{"ok":true}`),
		withDelay(500*time.Millisecond),
	)
	t.Cleanup(server.shutdown)
	t.Cleanup(client.Close)

	// Override the client's per-peer timeout for this test.
	for _, p := range client.peers {
		p.timeout = 100 * time.Millisecond
	}

	_, err := client.CallTool(ctx(t), server.peerName, "mnemo_slow", nil)
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("got %v, want ErrTimeout", err)
	}
}

func TestClientCallToolServerError(t *testing.T) {
	server, client := stubFederationPair(t,
		withStubError("mnemo_fail", "boom"),
	)
	t.Cleanup(server.shutdown)
	t.Cleanup(client.Close)

	_, err := client.CallTool(ctx(t), server.peerName, "mnemo_fail", nil)
	if !errors.Is(err, ErrServerError) {
		t.Errorf("got %v, want ErrServerError", err)
	}
}

func TestClientCallToolMalformedResponse(t *testing.T) {
	// Stand up a TLS server that responds with non-MCP garbage on
	// every request, mirroring a corrupted/misconfigured peer.
	clientDir := filepath.Join(t.TempDir(), "client")
	clientEP, err := endpoint.Load(clientDir)
	if err != nil {
		t.Fatalf("load client endpoint: %v", err)
	}
	serverDir := filepath.Join(t.TempDir(), "server")
	serverEP, err := endpoint.Load(serverDir)
	if err != nil {
		t.Fatalf("load server endpoint: %v", err)
	}
	addPeer(t, serverEP, "client", clientEP.CertPEM)
	serverEP, err = endpoint.Load(serverDir)
	if err != nil {
		t.Fatalf("reload server: %v", err)
	}

	tlsCfg, err := serverEP.ServerTLSConfig()
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	addr := freeAddr(t)
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("not json at all"))
		}),
		TLSConfig: tlsCfg,
	}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	waitForListen(t, addr)

	c, err := NewClient(clientEP, []store.LinkedInstance{{
		Name:     "garbage",
		URL:      "https://" + addr + "/mcp",
		PeerCert: string(serverEP.CertPEM),
	}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	_, err = c.CallTool(ctx(t), "garbage", "mnemo_stub", nil)
	if !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("got %v, want ErrMalformedResponse", err)
	}
}

// --- test helpers ---------------------------------------------------

type stubServerOpt func(*stubServer)

type stubServer struct {
	url      string
	peerName string
	endpoint *endpoint.Endpoint
	dir      string
	srv      *http.Server
	ln       net.Listener
	tools    map[string]stubTool
	delay    time.Duration
}

type stubTool struct {
	resultText string
	isError    bool
}

func withStubTool(name, resultJSON string) stubServerOpt {
	return func(s *stubServer) { s.tools[name] = stubTool{resultText: resultJSON} }
}

func withStubError(name, message string) stubServerOpt {
	return func(s *stubServer) { s.tools[name] = stubTool{resultText: message, isError: true} }
}

func withDelay(d time.Duration) stubServerOpt {
	return func(s *stubServer) { s.delay = d }
}

func (s *stubServer) shutdown() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

func (s *stubServer) reload(t *testing.T) {
	t.Helper()
	ep, err := endpoint.Load(s.dir)
	if err != nil {
		t.Fatalf("reload server endpoint: %v", err)
	}
	s.endpoint = ep
}

// stubFederationPair stands up: (a) a stub MCP-over-mTLS server in a
// temp dir with cross-trust set up against a fresh client endpoint;
// (b) a Client wired with that one peer. Returns both.
func stubFederationPair(t *testing.T, opts ...stubServerOpt) (*stubServer, *Client) {
	t.Helper()

	clientDir := filepath.Join(t.TempDir(), "client")
	clientEP, err := endpoint.Load(clientDir)
	if err != nil {
		t.Fatalf("load client endpoint: %v", err)
	}

	serverDir := filepath.Join(t.TempDir(), "server")
	serverEP, err := endpoint.Load(serverDir)
	if err != nil {
		t.Fatalf("load server endpoint: %v", err)
	}
	addPeer(t, serverEP, "client", clientEP.CertPEM)
	serverEP, err = endpoint.Load(serverDir)
	if err != nil {
		t.Fatalf("reload server: %v", err)
	}

	s := &stubServer{
		dir:      serverDir,
		endpoint: serverEP,
		tools:    map[string]stubTool{},
		peerName: "stub",
	}
	for _, o := range opts {
		o(s)
	}

	tlsCfg, err := serverEP.ServerTLSConfig()
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	mcp := mcpserver.NewMCPServer("stub", "test",
		mcpserver.WithToolCapabilities(true))
	for name, tool := range s.tools {
		def := mcp_NewTool(name)
		registerStubTool(mcp, def, tool, s.delay)
	}

	httpHandler := mcpserver.NewStreamableHTTPServer(mcp,
		mcpserver.WithStateful(true))

	addr := freeAddr(t)
	srv := &http.Server{Addr: addr, Handler: httpHandler, TLSConfig: tlsCfg}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	waitForListen(t, addr)

	s.srv = srv
	s.ln = ln
	s.url = "https://" + addr + "/mcp"

	c, err := NewClient(clientEP, []store.LinkedInstance{{
		Name:     s.peerName,
		URL:      s.url,
		PeerCert: string(serverEP.CertPEM),
	}})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return s, c
}

// mcp_NewTool builds a no-input tool definition. The leading
// underscore convention keeps the helper out of mcp-go's namespace.
func mcp_NewTool(name string) mcp.Tool {
	return mcp.NewTool(name, mcp.WithDescription("federation stub tool"))
}

func registerStubTool(s *mcpserver.MCPServer, def mcp.Tool, tool stubTool, delay time.Duration) {
	s.AddTool(def, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if tool.isError {
			return mcp.NewToolResultError(tool.resultText), nil
		}
		return mcp.NewToolResultText(tool.resultText), nil
	})
}

func addPeer(t *testing.T, ep *endpoint.Endpoint, name string, certPEM []byte) {
	t.Helper()
	if err := os.MkdirAll(ep.PeersDir, 0o700); err != nil {
		t.Fatalf("mkdir peers: %v", err)
	}
	path := filepath.Join(ep.PeersDir, name+".pem")
	if err := os.WriteFile(path, certPEM, 0o644); err != nil {
		t.Fatalf("write peer cert: %v", err)
	}
}

func textContent(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if t, ok := res.Content[0].(mcp.TextContent); ok {
		return t.Text
	}
	// Fallback — JSON-encode whatever we got so the test failure
	// message is readable.
	b, _ := json.Marshal(res.Content[0])
	return string(b)
}

// PinnedClientTLSConfig sanity check — given a peer cert it must
// reject any other cert presented over the wire. Covers
// classifyTransportError indirectly.
func TestPinnedClientTLSConfigRejectsOtherCert(t *testing.T) {
	pinDir := filepath.Join(t.TempDir(), "pin")
	pin, err := endpoint.Load(pinDir)
	if err != nil {
		t.Fatalf("load pin endpoint: %v", err)
	}
	otherDir := filepath.Join(t.TempDir(), "other")
	other, err := endpoint.Load(otherDir)
	if err != nil {
		t.Fatalf("load other endpoint: %v", err)
	}
	cfg, err := pin.PinnedClientTLSConfig(other.Cert)
	if err != nil {
		t.Fatalf("PinnedClientTLSConfig: %v", err)
	}
	// Self-presenting "pin" cert should be rejected (only "other" is pinned).
	if err := cfg.VerifyPeerCertificate([][]byte{pin.Cert.Raw}, nil); err == nil {
		t.Error("expected verification to fail for non-pinned cert")
	}
	if err := cfg.VerifyPeerCertificate([][]byte{other.Cert.Raw}, nil); err != nil {
		t.Errorf("expected verification to succeed for pinned cert, got %v", err)
	}
	// Sanity: pool with no CertPool entries cannot verify.
	pool := x509.NewCertPool()
	pool.AddCert(other.Cert)
	if _, err := other.Cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("self-cert verify in own-root pool failed: %v", err)
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return c
}
