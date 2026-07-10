// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package edgeproxy implements the transparent MCP edge that owns client
// connections and routes by Mcp-Session-Id to one or more backends
// (🎯T97.3). Edge↔backend traffic is standard MCP streamable HTTP with
// no custom protocol.
package edgeproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SessionIDHeader is the MCP streamable-HTTP session header (🎯T27).
const SessionIDHeader = "Mcp-Session-Id"

// Router maps MCP session IDs to backends and picks the primary backend
// for new initialize requests.
type Router struct {
	backends []*url.URL
	primary  atomic.Uint32
	pins     sync.Map // sessionID string -> uint32 backend index
}

// NewRouter parses backend base URLs and records the primary index used
// for new initialize requests.
func NewRouter(backendURLs []string, primary int) (*Router, error) {
	if len(backendURLs) == 0 {
		return nil, fmt.Errorf("edgeproxy: at least one backend URL is required")
	}
	if primary < 0 || primary >= len(backendURLs) {
		return nil, fmt.Errorf("edgeproxy: primary index %d out of range [0,%d)", primary, len(backendURLs))
	}
	backends := make([]*url.URL, len(backendURLs))
	for i, raw := range backendURLs {
		u, err := parseBackendURL(raw)
		if err != nil {
			return nil, fmt.Errorf("edgeproxy: backend %d: %w", i, err)
		}
		backends[i] = u
	}
	r := &Router{backends: backends}
	r.primary.Store(uint32(primary))
	return r, nil
}

// BackendCount returns the number of configured backends.
func (r *Router) BackendCount() int { return len(r.backends) }

// PrimaryIndex returns the backend index used for new initialize requests.
func (r *Router) PrimaryIndex() int { return int(r.primary.Load()) }

// SetPrimary changes which backend receives new initialize requests.
func (r *Router) SetPrimary(idx int) error {
	if idx < 0 || idx >= len(r.backends) {
		return fmt.Errorf("edgeproxy: primary index %d out of range [0,%d)", idx, len(r.backends))
	}
	r.primary.Store(uint32(idx))
	return nil
}

// Pin records session affinity for a session ID.
func (r *Router) Pin(sessionID string, backendIdx int) {
	if sessionID == "" || backendIdx < 0 || backendIdx >= len(r.backends) {
		return
	}
	r.pins.Store(sessionID, uint32(backendIdx))
}

// BackendForSession returns the pinned backend index and whether a pin
// existed. When no pin exists, the primary index is returned with false.
func (r *Router) BackendForSession(sessionID string) (idx int, pinned bool) {
	if sessionID == "" {
		return int(r.primary.Load()), false
	}
	if v, ok := r.pins.Load(sessionID); ok {
		return int(v.(uint32)), true
	}
	return int(r.primary.Load()), false
}

// Proxy is an http.Handler that forwards MCP and ancillary HTTP traffic
// to backends with session-aware routing on /mcp paths.
type Proxy struct {
	router  *Router
	clients []*http.Client
}

// NewProxy builds a Proxy for the given router.
func NewProxy(router *Router) *Proxy {
	clients := make([]*http.Client, len(router.backends))
	for i, u := range router.backends {
		clients[i] = newBackendClient(u)
	}
	return &Proxy{router: router, clients: clients}
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backendIdx, pinOnResponse, body, err := p.routeRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.forward(backendIdx, w, r, body, pinOnResponse)
}

func (p *Proxy) routeRequest(r *http.Request) (backendIdx int, pinOnResponse bool, body []byte, err error) {
	backendIdx = p.router.PrimaryIndex()
	sessionID := r.Header.Get(SessionIDHeader)

	if sessionID != "" {
		backendIdx, _ = p.router.BackendForSession(sessionID)
		return backendIdx, false, nil, nil
	}

	if r.Method == http.MethodPost && r.Body != nil {
		body, err = io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return 0, false, nil, fmt.Errorf("read request body: %w", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if isInitializeRequest(body) {
			return p.router.PrimaryIndex(), true, body, nil
		}
	}

	return backendIdx, false, body, nil
}

func (p *Proxy) forward(backendIdx int, w http.ResponseWriter, r *http.Request, body []byte, pinOnResponse bool) {
	backend := p.router.backends[backendIdx]
	outURL := cloneRequestURL(r.URL, backend)

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL, nil)
	if err != nil {
		http.Error(w, "build backend request", http.StatusInternalServerError)
		return
	}
	outReq.Header = r.Header.Clone()
	stripHopByHop(outReq.Header)
	if body != nil {
		outReq.Body = io.NopCloser(bytes.NewReader(body))
		outReq.ContentLength = int64(len(body))
	} else if r.Body != nil {
		outReq.Body = r.Body
		outReq.ContentLength = r.ContentLength
	}

	resp, err := p.clients[backendIdx].Do(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("backend unreachable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if pinOnResponse {
		if sid := resp.Header.Get(SessionIDHeader); sid != "" {
			p.router.Pin(sid, backendIdx)
		}
	}

	copyHeader(w.Header(), resp.Header)
	stripHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func isInitializeRequest(body []byte) bool {
	var msg struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return false
	}
	return msg.Method == "initialize"
}

func parseBackendURL(raw string) (*url.URL, error) {
	if strings.HasPrefix(raw, "unix:") {
		path := strings.TrimPrefix(raw, "unix:")
		path = strings.TrimPrefix(path, "//")
		if path == "" {
			return nil, fmt.Errorf("unix backend URL missing socket path")
		}
		return &url.URL{Scheme: "unix", Path: path}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" {
		u, err = url.Parse("http://" + raw)
		if err != nil {
			return nil, err
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q (want http, https, or unix:)", u.Scheme)
	}
	return u, nil
}

func cloneRequestURL(in *url.URL, backend *url.URL) string {
	path := in.Path
	if path == "" {
		path = "/"
	}
	out := &url.URL{
		Scheme:   backendScheme(backend),
		Host:     backendHost(backend),
		Path:     path,
		RawQuery: in.RawQuery,
	}
	return out.String()
}

func backendScheme(backend *url.URL) string {
	if backend.Scheme == "unix" {
		return "http"
	}
	return backend.Scheme
}

func backendHost(backend *url.URL) string {
	if backend.Scheme == "unix" {
		// http over unix is dialled by path; host is a placeholder.
		return "unix"
	}
	return backend.Host
}

func newBackendClient(backend *url.URL) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if backend.Scheme == "unix" {
		socketPath := backend.Path
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}
	}
	return &http.Client{
		Transport: transport,
		// No global timeout — GET/SSE streams may be long-lived.
	}
}

func copyHeader(dst, src http.Header) {
	for k, vals := range src {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// hopByHop lists RFC 7230 hop-by-hop headers that must not be forwarded
// between edge and backend (or client).
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func stripHopByHop(h http.Header) {
	// Connection may list additional hop-by-hop header names.
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for name := range hopByHop {
		h.Del(name)
	}
}
