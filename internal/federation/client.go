// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/store"
)

// DefaultPeerTimeout caps any single CallTool round-trip when a
// LinkedInstance does not specify its own timeout. T15.5 fan-out
// relies on this to bound how long a slow peer can stall a query.
const DefaultPeerTimeout = 5 * time.Second

// Typed errors. Callers (notably 🎯T15.5 fan-out) inspect these to
// decide whether to drop a peer with a warning, retry once, or
// surface the failure to the user.
var (
	// ErrUnknownInstance — name passed to CallTool was never declared
	// in linked_instances.
	ErrUnknownInstance = errors.New("federation: unknown instance")

	// ErrConnectionRefused — TCP layer rejected the connect (peer
	// daemon not running, listener on different port, firewall).
	ErrConnectionRefused = errors.New("federation: connection refused")

	// ErrConnectFailed — generic connect-time failure that isn't
	// "refused" (DNS, network unreachable, TCP timeout). Distinct
	// from ErrTimeout, which wraps the per-call deadline.
	ErrConnectFailed = errors.New("federation: connect failed")

	// ErrTLSHandshake — TLS layer rejected the handshake (peer cert
	// not pinned, mTLS client rejected, protocol mismatch).
	ErrTLSHandshake = errors.New("federation: TLS handshake failed")

	// ErrTimeout — the per-peer timeout fired before the call
	// completed. Wraps the original ctx error.
	ErrTimeout = errors.New("federation: peer call timed out")

	// ErrServerError — peer returned a successful HTTP/MCP transport
	// but the tool itself produced an error result (or non-2xx HTTP).
	ErrServerError = errors.New("federation: peer returned error")

	// ErrMalformedResponse — peer sent a response we could not parse
	// as MCP. Implies a programming or protocol-version mismatch.
	ErrMalformedResponse = errors.New("federation: malformed peer response")
)

// Client invokes tools on the configured LinkedInstance peers over
// mTLS. It owns one persistent http.Client + mcp client per peer so
// repeated calls reuse the underlying TCP/TLS connection and avoid
// re-running the handshake on every call.
type Client struct {
	endpoint *endpoint.Endpoint
	peers    map[string]*peerSession
	mu       sync.Mutex // serialises lazy MCP Initialize across goroutines.
}

type peerSession struct {
	instance store.LinkedInstance
	timeout  time.Duration
	mcp      *mcpclient.Client

	initOnce sync.Once
	initErr  error
}

// NewClient builds a Client from the loaded endpoint and the
// LinkedInstances declared in ~/.mnemo/config.json. Per-peer mTLS
// material is resolved once; PerPeer trust pool is built per peer
// (each instance trusts ONLY its declared peer cert, not the union
// pool used by the federated server).
//
// Errors from peer-cert resolution are returned immediately rather
// than deferred — a misconfigured config.json should fail loud at
// startup, not on first federated call. Network errors are deferred
// to CallTool.
func NewClient(ep *endpoint.Endpoint, peers []store.LinkedInstance) (*Client, error) {
	c := &Client{
		endpoint: ep,
		peers:    make(map[string]*peerSession, len(peers)),
	}
	for _, li := range peers {
		peerCert, err := li.ResolvePeerCert(ep.PeersDir)
		if err != nil {
			return nil, err
		}
		tlsCfg, err := ep.PinnedClientTLSConfig(peerCert)
		if err != nil {
			return nil, fmt.Errorf("build TLS config for %q: %w", li.Name, err)
		}
		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:   tlsCfg,
				ForceAttemptHTTP2: true,
				IdleConnTimeout:   90 * time.Second,
			},
			Timeout: DefaultPeerTimeout,
		}
		mcpC, err := mcpclient.NewStreamableHttpClient(li.URL,
			transport.WithHTTPBasicClient(httpClient))
		if err != nil {
			return nil, fmt.Errorf("build MCP client for %q: %w", li.Name, err)
		}
		c.peers[li.Name] = &peerSession{
			instance: li,
			timeout:  DefaultPeerTimeout,
			mcp:      mcpC,
		}
	}
	return c, nil
}

// PeerNames returns the configured peer names, suitable for listing.
func (c *Client) PeerNames() []string {
	out := make([]string, 0, len(c.peers))
	for name := range c.peers {
		out = append(out, name)
	}
	return out
}

// Close releases per-peer connections.
func (c *Client) Close() {
	for _, p := range c.peers {
		_ = p.mcp.Close()
	}
}

// CallTool invokes toolName on the named peer with args. Returns the
// peer's CallToolResult on success, or one of the typed errors above
// (use errors.Is to match). The per-peer timeout is enforced via a
// derived context and bounds total wall time including TLS handshake,
// HTTP round-trip, and response parse.
func (c *Client) CallTool(
	ctx context.Context,
	instance, toolName string,
	args map[string]any,
) (*mcp.CallToolResult, error) {
	p, ok := c.peers[instance]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownInstance, instance)
	}

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	if err := c.ensureInitialized(callCtx, p); err != nil {
		return nil, classifyTransportError(p.instance.Name, err, callCtx, ctx)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args

	res, err := p.mcp.CallTool(callCtx, req)
	if err != nil {
		return nil, classifyTransportError(p.instance.Name, err, callCtx, ctx)
	}
	if res.IsError {
		return res, fmt.Errorf("%w: %q on %q", ErrServerError, toolName, instance)
	}
	return res, nil
}

// ensureInitialized runs MCP Start + Initialize once per peer session
// and caches the result. Subsequent calls become no-ops.
func (c *Client) ensureInitialized(ctx context.Context, p *peerSession) error {
	p.initOnce.Do(func() {
		if err := p.mcp.Start(ctx); err != nil {
			p.initErr = fmt.Errorf("start MCP client: %w", err)
			return
		}
		_, err := p.mcp.Initialize(ctx, mcp.InitializeRequest{
			Params: mcp.InitializeParams{
				ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
				ClientInfo: mcp.Implementation{
					Name:    "mnemo-federation-client",
					Version: "1",
				},
			},
		})
		if err != nil {
			p.initErr = fmt.Errorf("initialize MCP: %w", err)
		}
	})
	return p.initErr
}

// classifyTransportError maps an mcp-go / net / TLS error onto one of
// our typed sentinels. The original error is wrapped for context.
// callCtx is the per-call (timeout-bounded) context; parentCtx is the
// caller's; we look at callCtx.Err() to detect timeout vs other
// transport failures.
func classifyTransportError(name string, err error, callCtx, parentCtx context.Context) error {
	// Timeout takes precedence — the deadline fired before the
	// underlying I/O could surface a useful error.
	if callCtx.Err() == context.DeadlineExceeded && parentCtx.Err() == nil {
		slog.Warn("federation: peer call timed out", "instance", name)
		return fmt.Errorf("%w: %q: %v", ErrTimeout, name, err)
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "connection refused"):
		return fmt.Errorf("%w: %q: %v", ErrConnectionRefused, name, err)
	case containsAny(msg, "tls:", "x509:", "remote error", "bad certificate", "certificate required"):
		return fmt.Errorf("%w: %q: %v", ErrTLSHandshake, name, err)
	case isMalformedResponse(err):
		return fmt.Errorf("%w: %q: %v", ErrMalformedResponse, name, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return fmt.Errorf("%w: %q: %v", ErrConnectFailed, name, err)
	}
	return fmt.Errorf("%w: %q: %v", ErrConnectFailed, name, err)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) && contains(s, sub) {
			return true
		}
	}
	return false
}

// contains is a hot-path strings.Contains that avoids the import-cycle
// dance between this package and the test helpers.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// isMalformedResponse heuristically detects parse failures that
// indicate the peer spoke something other than MCP (HTML error page,
// truncated JSON, wrong content type, etc). The mcp-go transport
// surfaces these as wrapped json.SyntaxError or "unexpected" errors.
func isMalformedResponse(err error) bool {
	msg := err.Error()
	return containsAny(msg,
		"invalid character",
		"unexpected end of JSON",
		"failed to unmarshal",
		"unsupported content-type",
		"unexpected content type",
		"unexpected status",
	)
}
