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
// for new initialize requests. Backends may grow at runtime (🎯T97.5).
type Router struct {
	mu       sync.RWMutex
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
func (r *Router) BackendCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.backends)
}

// PrimaryIndex returns the backend index used for new initialize requests.
func (r *Router) PrimaryIndex() int { return int(r.primary.Load()) }

// BackendURL returns the string form of backend i, or "" if out of range.
func (r *Router) BackendURL(i int) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i < 0 || i >= len(r.backends) {
		return ""
	}
	return r.backends[i].String()
}

// BackendURLs returns a copy of all backend URL strings.
func (r *Router) BackendURLs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.backends))
	for i, u := range r.backends {
		out[i] = u.String()
	}
	return out
}

// SetPrimary changes which backend receives new initialize requests.
func (r *Router) SetPrimary(idx int) error {
	r.mu.RLock()
	n := len(r.backends)
	r.mu.RUnlock()
	if idx < 0 || idx >= n {
		return fmt.Errorf("edgeproxy: primary index %d out of range [0,%d)", idx, n)
	}
	r.primary.Store(uint32(idx))
	return nil
}

// AddBackend appends a backend URL if not already present and returns
// its index. Safe for concurrent use with routing (🎯T97.5 edge grow).
func (r *Router) AddBackend(raw string) (int, error) {
	u, err := parseBackendURL(raw)
	if err != nil {
		return -1, err
	}
	key := u.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, b := range r.backends {
		if b.String() == key {
			return i, nil
		}
	}
	r.backends = append(r.backends, u)
	return len(r.backends) - 1, nil
}

// ApplyRoute ensures every URL in backends is registered, then sets
// primary. Returns the resolved primary index.
func (r *Router) ApplyRoute(backends []string, primary int) (int, error) {
	if len(backends) == 0 {
		return -1, fmt.Errorf("edgeproxy: empty backend list")
	}
	for _, b := range backends {
		if _, err := r.AddBackend(b); err != nil {
			return -1, err
		}
	}
	// Map primary by URL when possible (indices may have shifted if
	// order differed); prefer the primary-th entry of the file list.
	if primary < 0 || primary >= len(backends) {
		return -1, fmt.Errorf("edgeproxy: primary %d out of range for route list", primary)
	}
	idx, err := r.AddBackend(backends[primary])
	if err != nil {
		return -1, err
	}
	if err := r.SetPrimary(idx); err != nil {
		return -1, err
	}
	return idx, nil
}

// Pin records session affinity for a session ID.
func (r *Router) Pin(sessionID string, backendIdx int) {
	r.mu.RLock()
	n := len(r.backends)
	r.mu.RUnlock()
	if sessionID == "" || backendIdx < 0 || backendIdx >= n {
		return
	}
	r.pins.Store(sessionID, uint32(backendIdx))
}

// RepinAllToPrimary moves every pin onto the current primary.
// Crash-failover only (🎯T97.5 FailoverRepin) — do NOT use on the
// happy-path upgrade drain; that must keep pins on the old backend
// until they clear so mcp-go stateful sessions stay valid.
func (r *Router) RepinAllToPrimary() (moved int) {
	prim := r.PrimaryIndex()
	r.pins.Range(func(key, value any) bool {
		if int(value.(uint32)) != prim {
			r.pins.Store(key, uint32(prim))
			moved++
		}
		return true
	})
	return moved
}

// Unpin removes session affinity (session closed / DELETE).
func (r *Router) Unpin(sessionID string) {
	if sessionID == "" {
		return
	}
	r.pins.Delete(sessionID)
}

// PinCountForBackend returns how many sessions are pinned to idx.
func (r *Router) PinCountForBackend(idx int) int {
	if idx < 0 {
		return 0
	}
	n := 0
	r.pins.Range(func(_, value any) bool {
		if int(value.(uint32)) == idx {
			n++
		}
		return true
	})
	return n
}

// PinCounts returns per-backend pin counts (length == BackendCount).
func (r *Router) PinCounts() []int {
	n := r.BackendCount()
	out := make([]int, n)
	r.pins.Range(func(_, value any) bool {
		i := int(value.(uint32))
		if i >= 0 && i < n {
			out[i]++
		}
		return true
	})
	return out
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

func (r *Router) backendAt(i int) (*url.URL, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if i < 0 || i >= len(r.backends) {
		return nil, false
	}
	return r.backends[i], true
}

// Proxy is an http.Handler that forwards MCP and ancillary HTTP traffic
// to backends with session-aware routing on /mcp paths.
type Proxy struct {
	router  *Router
	mu      sync.Mutex
	clients []*http.Client
}

// NewProxy builds a Proxy for the given router.
func NewProxy(router *Router) *Proxy {
	p := &Proxy{router: router}
	p.syncClients()
	return p
}

// syncClients grows the client pool to match router backends.
func (p *Proxy) syncClients() {
	n := p.router.BackendCount()
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.clients) < n {
		u, ok := p.router.backendAt(len(p.clients))
		if !ok {
			break
		}
		p.clients = append(p.clients, newBackendClient(u))
	}
}

func (p *Proxy) clientAt(i int) *http.Client {
	p.syncClients()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.clients) {
		return nil
	}
	return p.clients[i]
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backendIdx, pinOnResponse, body, err := p.routeRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.forward(backendIdx, w, r, body, pinOnResponse, true)
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

func (p *Proxy) forward(backendIdx int, w http.ResponseWriter, r *http.Request, body []byte, pinOnResponse bool, allowFailover bool) {
	backend, ok := p.router.backendAt(backendIdx)
	if !ok {
		http.Error(w, "backend index out of range", http.StatusBadGateway)
		return
	}
	client := p.clientAt(backendIdx)
	if client == nil {
		http.Error(w, "no client for backend", http.StatusBadGateway)
		return
	}
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

	resp, err := client.Do(outReq)
	if err != nil {
		// Fail over pinned sessions to primary when a backend dies mid-drain.
		if allowFailover {
			prim := p.router.PrimaryIndex()
			if prim != backendIdx {
				if sid := r.Header.Get(SessionIDHeader); sid != "" {
					p.router.Pin(sid, prim)
				}
				// Retry once on primary with a fresh body reader.
				var retryBody []byte
				if body != nil {
					retryBody = body
				}
				p.forward(prim, w, r, retryBody, false, false)
				return
			}
		}
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
