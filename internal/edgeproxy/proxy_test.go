// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package edgeproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewRouterRejectsEmptyAndBadPrimary(t *testing.T) {
	t.Parallel()
	if _, err := NewRouter(nil, 0); err == nil {
		t.Fatal("expected error for empty backends")
	}
	if _, err := NewRouter([]string{"http://127.0.0.1:1"}, 1); err == nil {
		t.Fatal("expected error for out-of-range primary")
	}
	if _, err := NewRouter([]string{"ftp://x"}, 0); err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

func TestRouterPinAndSetPrimary(t *testing.T) {
	t.Parallel()
	r, err := NewRouter([]string{
		"http://127.0.0.1:19421",
		"http://127.0.0.1:19422",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.PrimaryIndex() != 0 {
		t.Fatalf("primary=%d want 0", r.PrimaryIndex())
	}
	r.Pin("sess-a", 0)
	if idx, ok := r.BackendForSession("sess-a"); !ok || idx != 0 {
		t.Fatalf("pin sess-a: idx=%d ok=%v", idx, ok)
	}
	if err := r.SetPrimary(1); err != nil {
		t.Fatal(err)
	}
	// New initializes (no pin) go to primary 1; existing pin stays.
	if idx, ok := r.BackendForSession(""); ok || idx != 1 {
		t.Fatalf("unpinned: idx=%d ok=%v want primary 1", idx, ok)
	}
	if idx, ok := r.BackendForSession("sess-a"); !ok || idx != 0 {
		t.Fatalf("pinned after primary flip: idx=%d ok=%v", idx, ok)
	}
}

func TestRouterUnixBackendURL(t *testing.T) {
	t.Parallel()
	r, err := NewRouter([]string{"unix:///tmp/mnemo-backend.sock"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := r.backendAt(0)
	if !ok || u.Scheme != "unix" || u.Path != "/tmp/mnemo-backend.sock" {
		t.Fatalf("unix url: %+v ok=%v", u, ok)
	}
}

func TestRouterAddBackendAndApplyRoute(t *testing.T) {
	t.Parallel()
	r, err := NewRouter([]string{"http://127.0.0.1:1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := r.AddBackend("http://127.0.0.1:2")
	if err != nil || idx != 1 {
		t.Fatalf("add: idx=%d err=%v", idx, err)
	}
	// Idempotent
	idx2, err := r.AddBackend("http://127.0.0.1:2")
	if err != nil || idx2 != 1 {
		t.Fatalf("add again: idx=%d err=%v", idx2, err)
	}
	prim, err := r.ApplyRoute([]string{"http://127.0.0.1:1", "http://127.0.0.1:2", "http://127.0.0.1:3"}, 2)
	if err != nil || prim != 2 {
		t.Fatalf("apply: prim=%d err=%v count=%d", prim, err, r.BackendCount())
	}
	if r.BackendCount() != 3 {
		t.Fatalf("count %d", r.BackendCount())
	}
	if r.PrimaryIndex() != 2 {
		t.Fatalf("primary %d", r.PrimaryIndex())
	}
}

func TestProxyFailoverRepinsToPrimary(t *testing.T) {
	t.Parallel()
	// Backend 0 dies; backend 1 is primary and healthy.
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("alive"))
	}))
	t.Cleanup(alive.Close)

	// Dead listener: close immediately so dial fails.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := deadLn.Addr().String()
	_ = deadLn.Close()

	router, err := NewRouter([]string{
		"http://" + deadAddr,
		alive.URL,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	router.Pin("sess-x", 0) // pinned to dead
	proxy := NewProxy(router)

	req := httptest.NewRequest(http.MethodGet, "http://edge/mcp", nil)
	req.Header.Set(SessionIDHeader, "sess-x")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "alive" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	idx, pinned := router.BackendForSession("sess-x")
	if !pinned || idx != 1 {
		t.Fatalf("expected repin to primary 1, got idx=%d pinned=%v", idx, pinned)
	}
}

func TestRepinAllToPrimary(t *testing.T) {
	t.Parallel()
	r, _ := NewRouter([]string{"http://127.0.0.1:1", "http://127.0.0.1:2"}, 1)
	r.Pin("a", 0)
	r.Pin("b", 0)
	if n := r.RepinAllToPrimary(); n != 2 {
		t.Fatalf("moved %d", n)
	}
	if idx, _ := r.BackendForSession("a"); idx != 1 {
		t.Fatalf("a idx %d", idx)
	}
}

// TestAffinityDrainKeepsPinOnOldBackend is the 🎯T97.5 acceptance bar:
// initialize on B0 → flip primary to B1 without repin → tool call with
// the same Mcp-Session-Id still hits B0 while B0 is up. Repin would
// break mcp-go stateful sessions.
func TestAffinityDrainKeepsPinOnOldBackend(t *testing.T) {
	t.Parallel()
	var hits [2]atomic.Int32
	b0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[0].Add(1)
		if r.Header.Get(SessionIDHeader) == "" {
			w.Header().Set(SessionIDHeader, "sess-aff")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("old-backend"))
	}))
	t.Cleanup(b0.Close)
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[1].Add(1)
		w.Header().Set(SessionIDHeader, "sess-new")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("new-backend"))
	}))
	t.Cleanup(b1.Close)

	router, err := NewRouter([]string{b0.URL, b1.URL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy(router)

	// initialize → B0, pin sess-aff
	initReq := httptest.NewRequest(http.MethodPost, "http://edge/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	initRec := httptest.NewRecorder()
	proxy.ServeHTTP(initRec, initReq)
	sid := initRec.Header().Get(SessionIDHeader)
	if sid != "sess-aff" {
		t.Fatalf("sid %q", sid)
	}
	if hits[0].Load() != 1 || hits[1].Load() != 0 {
		t.Fatalf("after init hits %d/%d", hits[0].Load(), hits[1].Load())
	}

	// Flip primary to B1 — NO repin (affinity drain).
	if err := router.SetPrimary(1); err != nil {
		t.Fatal(err)
	}
	if router.PinCountForBackend(0) != 1 || router.PinCountForBackend(1) != 0 {
		t.Fatalf("pins after flip: %v", router.PinCounts())
	}

	// Tool call with same session must still hit B0.
	toolReq := httptest.NewRequest(http.MethodPost, "http://edge/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	toolReq.Header.Set(SessionIDHeader, sid)
	toolRec := httptest.NewRecorder()
	proxy.ServeHTTP(toolRec, toolReq)
	if toolRec.Body.String() != "old-backend" {
		t.Fatalf("body %q want old-backend (repin would break stateful MCP)", toolRec.Body.String())
	}
	if hits[0].Load() != 2 || hits[1].Load() != 0 {
		t.Fatalf("after tool hits %d/%d — pin must stay on B0", hits[0].Load(), hits[1].Load())
	}

	// New initialize goes to B1.
	init2 := httptest.NewRequest(http.MethodPost, "http://edge/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{}}`))
	init2Rec := httptest.NewRecorder()
	proxy.ServeHTTP(init2Rec, init2)
	if init2Rec.Body.String() != "new-backend" {
		t.Fatalf("new init body %q", init2Rec.Body.String())
	}

	// After unpin, B0 pin count is 0 (safe to affinity-drain reap).
	router.Unpin(sid)
	if router.PinCountForBackend(0) != 0 {
		t.Fatalf("after unpin pins %v", router.PinCounts())
	}
}

// Proves 🎯T97.5 edge grow: start with one backend, ApplyRoute adds a
// second URL as primary, and new initialize traffic hits the new one.
func TestProxyGrowsBackendAndRoutesNewInitToPrimary(t *testing.T) {
	t.Parallel()
	var hits [2]atomic.Int32
	b0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[0].Add(1)
		w.Header().Set(SessionIDHeader, "sid-old")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("b0"))
	}))
	t.Cleanup(b0.Close)
	b1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[1].Add(1)
		w.Header().Set(SessionIDHeader, "sid-new")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("b1"))
	}))
	t.Cleanup(b1.Close)

	router, err := NewRouter([]string{b0.URL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy(router)

	// Grow + flip as edge-route would after spawn.
	if _, err := router.ApplyRoute([]string{b0.URL, b1.URL}, 1); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://edge/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Body.String() != "b1" || rec.Header().Get(SessionIDHeader) != "sid-new" {
		t.Fatalf("body=%q sid=%q hits=%d/%d", rec.Body.String(), rec.Header().Get(SessionIDHeader), hits[0].Load(), hits[1].Load())
	}
	if hits[1].Load() != 1 || hits[0].Load() != 0 {
		t.Fatalf("hits b0=%d b1=%d", hits[0].Load(), hits[1].Load())
	}
}

func TestProxyInitializePinsSession(t *testing.T) {
	t.Parallel()
	var hits [2]atomic.Int32
	backends := make([]*httptest.Server, 2)
	for i := range backends {
		i := i
		backends[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[i].Add(1)
			if r.Method == http.MethodPost && r.Header.Get(SessionIDHeader) == "" {
				w.Header().Set(SessionIDHeader, fmt.Sprintf("sid-%d", i))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf("backend-%d", i)))
		}))
		t.Cleanup(backends[i].Close)
	}

	router, err := NewRouter([]string{backends[0].URL, backends[1].URL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy(router)

	// initialize → primary 0, pin session
	req := httptest.NewRequest(http.MethodPost, "http://edge/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status %d", rec.Code)
	}
	sid := rec.Header().Get(SessionIDHeader)
	if sid != "sid-0" {
		t.Fatalf("session id %q", sid)
	}
	if hits[0].Load() != 1 || hits[1].Load() != 0 {
		t.Fatalf("hits after init: %d %d", hits[0].Load(), hits[1].Load())
	}

	// Flip primary; pinned session still hits backend 0
	if err := router.SetPrimary(1); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest(http.MethodPost, "http://edge/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req2.Header.Set(SessionIDHeader, sid)
	rec2 := httptest.NewRecorder()
	proxy.ServeHTTP(rec2, req2)
	if body := rec2.Body.String(); body != "backend-0" {
		t.Fatalf("pinned body %q", body)
	}
	if hits[0].Load() != 2 || hits[1].Load() != 0 {
		t.Fatalf("hits after pinned call: %d %d", hits[0].Load(), hits[1].Load())
	}

	// New initialize goes to primary 1
	req3 := httptest.NewRequest(http.MethodPost, "http://edge/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{}}`))
	rec3 := httptest.NewRecorder()
	proxy.ServeHTTP(rec3, req3)
	if rec3.Header().Get(SessionIDHeader) != "sid-1" {
		t.Fatalf("new init session %q", rec3.Header().Get(SessionIDHeader))
	}
	if hits[1].Load() != 1 {
		t.Fatalf("backend 1 hits %d want 1", hits[1].Load())
	}
}

func TestProxySSEFlushThrough(t *testing.T) {
	t.Parallel()
	// Real TCP servers so streaming + flush is exercised end-to-end.
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backendLn.Close() })

	// Gate: first event is written immediately; second waits until the
	// test has observed the first (proves edge flushes hop-by-hop).
	firstSeen := make(chan struct{})
	go func() {
		_ = http.Serve(backendLn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flush", 500)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: one\n\n")
			flusher.Flush()
			select {
			case <-firstSeen:
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Second):
				return
			}
			fmt.Fprint(w, "data: two\n\n")
			flusher.Flush()
		}))
	}()

	router, err := NewRouter([]string{"http://" + backendLn.Addr().String()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = edgeLn.Close() })
	go func() { _ = http.Serve(edgeLn, NewProxy(router)) }()

	resp, err := http.Get("http://" + edgeLn.Addr().String() + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	gotFirst := make(chan string, 1)
	go func() {
		var buf strings.Builder
		for {
			line, err := reader.ReadString('\n')
			buf.WriteString(line)
			if strings.Contains(buf.String(), "data: one") {
				gotFirst <- buf.String()
				return
			}
			if err != nil {
				gotFirst <- buf.String() + "\nerr:" + err.Error()
				return
			}
		}
	}()

	select {
	case first := <-gotFirst:
		if !strings.Contains(first, "data: one") {
			t.Fatalf("first SSE event missing (flush broken?): %q", first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first SSE event through edge")
	}
	close(firstSeen)

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("second SSE event missing: %q", string(rest))
	}
}

func TestProxyUnixBackend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := dir + "/backend.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var once sync.Once
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("unix-ok"))
	}))

	router, err := NewRouter([]string{"unix://" + sock}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://edge/api/health", nil)
	NewProxy(router).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "unix-ok" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestIsInitializeRequest(t *testing.T) {
	t.Parallel()
	if !isInitializeRequest([]byte(`{"method":"initialize"}`)) {
		t.Fatal("want initialize true")
	}
	if isInitializeRequest([]byte(`{"method":"tools/list"}`)) {
		t.Fatal("want tools/list false")
	}
	if isInitializeRequest([]byte(`not-json`)) {
		t.Fatal("want garbage false")
	}
}
