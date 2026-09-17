// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package teampush

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/endpoint"
)

// Client transmits pushes to a central team instance over mTLS.
type Client struct {
	httpc  *http.Client
	url    string
	author string
}

// NewClient dials with the local endpoint identity, pinned to the team
// server's certificate.
//
// Pinning to the one cert rather than the union trusted-peer pool is
// deliberate: a contributor may have several federation peers trusted
// for read fan-out, and none of those should be able to impersonate the
// team server and receive a push. Trust for reading someone's index and
// trust for receiving your transcripts are different grants.
func NewClient(ep *endpoint.Endpoint, teamURL string, serverCert *x509.Certificate, author string) (*Client, error) {
	if teamURL == "" {
		return nil, fmt.Errorf("team instance url is empty")
	}
	u, err := url.Parse(teamURL)
	if err != nil {
		return nil, fmt.Errorf("parse team url %q: %w", teamURL, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("team url scheme must be https, got %q", u.Scheme)
	}
	tlsCfg, err := ep.PinnedClientTLSConfig(serverCert)
	if err != nil {
		return nil, fmt.Errorf("build client TLS config: %w", err)
	}
	return &Client{
		httpc: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
			Timeout:   10 * time.Minute,
		},
		url:    teamURL,
		author: author,
	}, nil
}

// PushURL derives the /push endpoint from a configured team URL, which
// conventionally points at /mcp. Both forms are accepted so a
// contributor can paste either into config without it mattering.
func PushURL(teamURL string) string {
	trimmed := strings.TrimRight(teamURL, "/")
	if strings.HasSuffix(trimmed, "/mcp") {
		return strings.TrimSuffix(trimmed, "/mcp") + PushPath
	}
	if strings.HasSuffix(trimmed, PushPath) {
		return trimmed
	}
	return trimmed + PushPath
}

// Push transmits the sessions and returns the server's structured
// response. The payload is validated locally first so an obviously
// malformed push fails with a message that names the offending session
// rather than a 400 from the far end.
func (c *Client) Push(ctx context.Context, sessions []Session) (*PushResponse, error) {
	req := PushRequest{Version: ProtocolVersion, Sessions: sessions}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, PushURL(c.url), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.author != "" {
		httpReq.Header.Set(AuthorHeader, c.author)
	}

	resp, err := c.httpc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("push to %s: %w", PushURL(c.url), err)
	}
	defer resp.Body.Close()

	// Cap the response read: the far end is authenticated but a
	// misbehaving or wedged server should not be able to exhaust the
	// contributor's memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read push response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("team server refused the push (%s): %s", resp.Status, e.Error)
		}
		return nil, fmt.Errorf("team server refused the push (%s): %s",
			resp.Status, strings.TrimSpace(string(raw)))
	}

	var out PushResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode push response: %w", err)
	}
	return &out, nil
}

// Reachability is the outcome of a Ping.
type Reachability int

// Ping outcomes. These three are genuinely different situations and a
// contributor who cannot tell them apart will misdiagnose the middle
// one — which is the EXPECTED state immediately after onboarding — as a
// broken install, and file a bug instead of messaging their admin.
const (
	// Unreachable: nothing answered, or TLS failed before identity
	// could be established.
	Unreachable Reachability = iota

	// NotTrusted: the server answered and rejected this certificate.
	// Normal before an admin installs it.
	NotTrusted

	// Ready: authenticated; pushes will be accepted.
	Ready
)

// PingResult reports what the team server said about this contributor.
type PingResult struct {
	Reachability Reachability

	// Author is the identity the server knows this contributor by —
	// worth surfacing at onboarding time, because it is what their
	// pushes will be filed under.
	Author string

	// Err carries the underlying transport or HTTP error, if any.
	Err error
}

// Ping performs an authenticated round trip against the team server's
// health endpoint.
//
// It exists because the obvious cheap check — pushing nothing — never
// reaches the network: an empty push fails local validation first, so
// it reports success against a server that would have refused the
// contributor outright. A connectivity check that cannot fail is not a
// check.
func (c *Client) Ping(ctx context.Context) PingResult {
	base := strings.TrimRight(c.url, "/")
	base = strings.TrimSuffix(base, "/mcp")
	base = strings.TrimSuffix(base, PushPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return PingResult{Reachability: Unreachable, Err: err}
	}
	if c.author != "" {
		req.Header.Set(AuthorHeader, c.author)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		if isRemoteCertRejection(err) {
			return PingResult{Reachability: NotTrusted, Err: err}
		}
		return PingResult{Reachability: Unreachable, Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			You string `json:"you"`
		}
		_ = json.Unmarshal(raw, &body)
		return PingResult{Reachability: Ready, Author: body.You}
	case http.StatusForbidden:
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		return PingResult{Reachability: NotTrusted, Err: fmt.Errorf("%s", body.Error)}
	default:
		return PingResult{
			Reachability: Unreachable,
			Err:          fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw))),
		}
	}
}

// isRemoteCertRejection reports whether the far end refused OUR
// certificate during the handshake.
//
// This is the common shape of "not trusted yet", not the HTTP 403: the
// team server requires and verifies the client certificate, so an
// unknown contributor is turned away by TLS before any request is
// sent. The 403 covers the narrower case of a certificate that chains
// to the trust pool without being one of the installed peer files.
//
// The "remote error:" prefix is load-bearing. Go writes it only for
// alerts the PEER sent, so it distinguishes the server rejecting us
// from our own pinning check rejecting the server — which produces a
// local error and genuinely does mean something is misconfigured here.
func isRemoteCertRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "remote error") {
		return false
	}
	for _, alert := range []string{
		"unknown certificate authority",
		"bad certificate",
		"certificate required",
		"unknown certificate",
		"certificate unknown",
		"handshake failure",
	} {
		if strings.Contains(msg, alert) {
			return true
		}
	}
	return false
}
