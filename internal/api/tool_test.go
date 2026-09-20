// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newToolBridge returns a mux serving only the tool bridge, plus a
// pointer to the arguments the last call received.
func newToolBridge(t *testing.T, fn ToolCaller) (*http.ServeMux, *map[string]any) {
	t.Helper()
	h := New(nil)
	seen := map[string]any{}
	h.SetToolCaller(func(ctx context.Context, user, name string, args map[string]any) (string, bool, error) {
		seen = args
		return fn(ctx, user, name, args)
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux, &seen
}

func TestToolBridgeCallsTheNamedTool(t *testing.T) {
	var gotName, gotUser string
	mux, args := newToolBridge(t, func(_ context.Context, user, name string, _ map[string]any) (string, bool, error) {
		gotName, gotUser = name, user
		return "the answer", false, nil
	})

	req := httptest.NewRequest("POST", "/api/tool/mnemo_search?user=alice",
		strings.NewReader(`{"query":"qr pairing","limit":5}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if gotName != "mnemo_search" {
		t.Errorf("tool name %q, want mnemo_search", gotName)
	}
	if gotUser != "alice" {
		t.Errorf("user %q, want alice", gotUser)
	}
	var res toolCallResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.OK || res.Text != "the answer" {
		t.Errorf("envelope = %+v, want ok with the tool's text", res)
	}

	// The trap this endpoint exists to avoid: Handler.Call type-asserts
	// numeric arguments as float64, so a number that arrives as anything
	// else is silently replaced by the tool's default.
	if _, ok := (*args)["limit"].(float64); !ok {
		t.Errorf("limit arrived as %T, want float64", (*args)["limit"])
	}
}

// A tool that rejects its arguments is not a transport failure. It must
// come back as HTTP 200 with ok=false, because that is what lets the CLI
// set an exit status without parsing prose.
func TestToolBridgeReportsToolErrorsInTheEnvelope(t *testing.T) {
	mux, _ := newToolBridge(t, func(context.Context, string, string, map[string]any) (string, bool, error) {
		return "query is required", true, nil
	})
	req := httptest.NewRequest("POST", "/api/tool/mnemo_search", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 — a tool-level error is not a transport error", w.Code)
	}
	var res toolCallResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.OK {
		t.Error("ok = true for a tool that reported an error")
	}
	if res.Text != "query is required" {
		t.Errorf("text = %q, want the tool's message", res.Text)
	}
}

func TestToolBridgeUnknownToolIs404(t *testing.T) {
	mux, _ := newToolBridge(t, func(_ context.Context, _, name string, _ map[string]any) (string, bool, error) {
		return "", false, errors.New("unknown tool: " + name)
	})
	req := httptest.NewRequest("POST", "/api/tool/mnemo_nope", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}

// Most tools have an all-optional schema, so `mnemo stats` sends nothing.
func TestToolBridgeAcceptsAnEmptyBody(t *testing.T) {
	mux, args := newToolBridge(t, func(context.Context, string, string, map[string]any) (string, bool, error) {
		return "fine", false, nil
	})
	req := httptest.NewRequest("POST", "/api/tool/mnemo_stats", strings.NewReader(""))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if len(*args) != 0 {
		t.Errorf("args = %v, want empty", *args)
	}
}

func TestToolBridgeRejectsNonObjectArguments(t *testing.T) {
	mux, _ := newToolBridge(t, func(context.Context, string, string, map[string]any) (string, bool, error) {
		t.Error("tool must not be called with malformed arguments")
		return "", false, nil
	})
	req := httptest.NewRequest("POST", "/api/tool/mnemo_stats", strings.NewReader(`["not","an","object"]`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestToolBridgeIsPostOnly(t *testing.T) {
	mux, _ := newToolBridge(t, func(context.Context, string, string, map[string]any) (string, bool, error) {
		t.Error("GET must not reach the tool")
		return "", false, nil
	})
	req := httptest.NewRequest("GET", "/api/tool/mnemo_stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", w.Code)
	}
}

// Unwired, the endpoint must say so rather than panicking on a nil call.
func TestToolBridgeUnwiredIsNotImplemented(t *testing.T) {
	h := New(nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest("POST", "/api/tool/mnemo_stats", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", w.Code)
	}
}
