// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package httpguard rejects requests that a loopback-bound daemon should
// never have been asked to serve.
//
// Binding 127.0.0.1 is not, on its own, a security boundary. A web page
// the user visits can point a hostname it controls at 127.0.0.1 (DNS
// rebinding) and then have the browser issue same-origin requests to this
// daemon, which has no authentication because "only localhost can reach
// it". The browser still sends the attacker's name in Host, and its own
// page origin in Origin, so both are checkable — and checking them is the
// whole defence, because a real loopback client can always spell the
// address correctly and a rebinding page never can.
package httpguard

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

const (
	// localhostName is the one DNS name accepted in Host and Origin.
	// RFC 6761 reserves it for loopback and browsers resolve it without
	// consulting DNS, so it cannot be rebound the way an ordinary name
	// can. It has to be accepted: it is the address every client config
	// in the docs uses.
	localhostName = "localhost"

	// defaultHTTPPort is what a Host header with no port means. Spelled
	// out so the port comparison below never silently succeeds on "".
	defaultHTTPPort = "80"

	// nullOrigin is what a browser sends from a sandboxed iframe, a
	// data: document, or after a redirect it will not disclose. Nothing
	// legitimate reaches a local daemon that way, and it is exactly the
	// shape an attacker uses to dodge an origin check.
	nullOrigin = "null"

	forbiddenMessage = "forbidden: request was not addressed to this daemon's loopback address"
)

// Args configures the guard.
type Args struct {
	// Port is the port the daemon is bound to. A request whose Host names
	// a different port was addressed somewhere else, whatever the socket
	// it arrived on.
	Port string
}

// New wraps next with the Host/Origin guard.
func New(args *Args, next http.Handler) http.Handler {
	port := args.Port
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, port) || !originAllowed(r.Header.Get("Origin"), port) {
			http.Error(w, forbiddenMessage, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether the Host header names this daemon.
func hostAllowed(hostHeader, port string) bool {
	host, hostPort := splitHostPort(hostHeader)
	return isLoopbackHost(host) && hostPort == port
}

// originAllowed reports whether the Origin header, if any, belongs to a
// page this daemon itself served. Absent is allowed: only browsers send
// Origin, and every non-browser MCP client omits it.
func originAllowed(origin, port string) bool {
	if origin == "" {
		return true
	}
	if origin == nullOrigin {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return hostAllowed(u.Host, port)
}

// splitHostPort tolerates a missing port, which net.SplitHostPort treats
// as an error, and strips the brackets around a bare IPv6 literal.
func splitHostPort(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return strings.Trim(hostport, "[]"), defaultHTTPPort
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, localhostName) {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
