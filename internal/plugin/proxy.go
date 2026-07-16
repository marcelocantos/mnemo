// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// ProxyHandler reverse-proxies /plugins/<name>/* to the ready instance's
// BaseURL (🎯T102.5). The /plugins/<name> prefix is stripped so
// GET /plugins/lab/manifest becomes GET {BaseURL}/manifest.
//
// Only StateReady instances with a non-empty BaseURL are proxied;
// unknown, disabled, or not-yet-ready names return 404 (never a proxy
// panic). WebSocket and SSE upgrades pass through: httputil.ReverseProxy
// forwards Upgrade headers and hijacks 101 responses, and FlushInterval
// is set for immediate SSE delivery.
//
// m may be nil; every request then 404s.
func ProxyHandler(m *Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, rest, ok := splitPluginPath(r.URL.Path)
		if !ok || m == nil {
			http.NotFound(w, r)
			return
		}
		snap, found := m.Get(name)
		if !found || snap.State != StateReady || snap.BaseURL == "" {
			http.NotFound(w, r)
			return
		}
		target, err := url.Parse(snap.BaseURL)
		if err != nil || target.Scheme == "" || target.Host == "" {
			http.NotFound(w, r)
			return
		}
		// One ReverseProxy per request so concurrent plugins never share
		// Director state (target is closed over below).
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = target.Scheme
				req.URL.Host = target.Host
				req.URL.Path = joinProxyPath(target.Path, rest)
				req.URL.RawPath = ""
				// Preserve original query; join any BaseURL query params.
				if target.RawQuery == "" || req.URL.RawQuery == "" {
					req.URL.RawQuery = target.RawQuery + req.URL.RawQuery
				} else {
					req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
				}
				req.Host = target.Host
				// Avoid injecting Go's default User-Agent when the client
				// sent none (same behaviour as NewSingleHostReverseProxy).
				if _, hasUA := req.Header["User-Agent"]; !hasUA {
					req.Header.Set("User-Agent", "")
				}
			},
			// Immediate flush so SSE event streams reach the browser
			// without buffering at the proxy.
			FlushInterval: -1,
			ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, _ error) {
				// Backend dial/transport failure — not "unknown plugin".
				http.Error(rw, "Bad Gateway", http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

// splitPluginPath parses /plugins/<name>[/<rest>...] into name and the
// path to forward (always starting with /). ok is false for malformed
// paths (missing name, empty segment, wrong prefix).
func splitPluginPath(p string) (name, rest string, ok bool) {
	const prefix = "/plugins/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rem := p[len(prefix):]
	if rem == "" {
		return "", "", false
	}
	name, after, found := strings.Cut(rem, "/")
	if name == "" {
		return "", "", false
	}
	if !found {
		return name, "/", true
	}
	return name, "/" + after, true
}

// joinProxyPath joins a target base path with the stripped request path.
func joinProxyPath(base, rest string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	if base == "" {
		return rest
	}
	return base + rest
}
