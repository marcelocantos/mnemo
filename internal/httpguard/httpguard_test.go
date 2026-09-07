// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package httpguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const boundPort = "19419"

func guarded(t *testing.T) http.Handler {
	t.Helper()
	return New(&Args{Port: boundPort}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	}))
}

// TestRejectsRebindingHost is the DNS-rebinding oracle: a loopback bind is
// only a boundary if the daemon checks who the request was addressed to.
func TestRejectsRebindingHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Host = "evil.example:" + boundPort
	rec := httptest.NewRecorder()
	guarded(t).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Host: evil.example → %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec.Body.String() == "reached" {
		t.Fatal("rebinding request reached the wrapped handler")
	}
}

func TestAcceptsLoopbackHost(t *testing.T) {
	for _, host := range []string{
		"127.0.0.1:" + boundPort,
		"[::1]:" + boundPort,
		"localhost:" + boundPort,
		"LocalHost:" + boundPort,
		"localhost.:" + boundPort,
		"127.0.0.2:" + boundPort,
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		guarded(t).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host: %s → %d, want %d", host, rec.Code, http.StatusOK)
		}
	}
}

func TestRejectsWrongPort(t *testing.T) {
	for _, host := range []string{"127.0.0.1:19420", "localhost", "127.0.0.1"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		guarded(t).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host: %s → %d, want %d", host, rec.Code, http.StatusForbidden)
		}
	}
}

func TestRejectsCrossOrigin(t *testing.T) {
	for _, origin := range []string{
		"https://evil.example",
		"http://evil.example:" + boundPort,
		"null",
		"http://127.0.0.1:3000",
		"file://",
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Host = "127.0.0.1:" + boundPort
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		guarded(t).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Origin: %s → %d, want %d", origin, rec.Code, http.StatusForbidden)
		}
	}
}

func TestAcceptsSameOrigin(t *testing.T) {
	for _, origin := range []string{
		"http://127.0.0.1:" + boundPort,
		"http://localhost:" + boundPort,
		"http://[::1]:" + boundPort,
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Host = "127.0.0.1:" + boundPort
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		guarded(t).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Origin: %s → %d, want %d", origin, rec.Code, http.StatusOK)
		}
	}
}

// TestAbsentOriginIsAccepted keeps non-browser MCP clients working: they
// send no Origin at all, and only browsers are subject to rebinding.
func TestAbsentOriginIsAccepted(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Host = "localhost:" + boundPort
	rec := httptest.NewRecorder()
	guarded(t).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no Origin → %d, want %d", rec.Code, http.StatusOK)
	}
}
