// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestRespCacheHitsAndExpiry verifies the 🎯T92 TTL cache: identical
// requests within the TTL are served from cache (the underlying handler
// runs once), a hit is flagged, and the handler runs again after expiry.
func TestRespCacheHitsAndExpiry(t *testing.T) {
	var calls atomic.Int64
	c := newRespCache(50 * time.Millisecond)
	h := c.wrap(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	do := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/api/usage?days=7", nil))
		return rr
	}

	// First request: miss → handler runs, body correct, no hit marker.
	r1 := do()
	if calls.Load() != 1 {
		t.Fatalf("first request should invoke handler once, got %d", calls.Load())
	}
	if r1.Body.String() != `{"ok":true}` {
		t.Fatalf("unexpected body: %q", r1.Body.String())
	}
	if r1.Header().Get("X-Mnemo-Cache") == "hit" {
		t.Fatalf("first request must not be a cache hit")
	}

	// Second request within TTL: hit → handler NOT re-invoked, same body.
	r2 := do()
	if calls.Load() != 1 {
		t.Fatalf("cached request must not re-invoke handler, got %d calls", calls.Load())
	}
	if r2.Header().Get("X-Mnemo-Cache") != "hit" {
		t.Fatalf("second request should be a cache hit")
	}
	if r2.Body.String() != `{"ok":true}` {
		t.Fatalf("cached body mismatch: %q", r2.Body.String())
	}

	// A different query key is a separate entry → miss.
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/api/usage?days=30", nil))
	if calls.Load() != 2 {
		t.Fatalf("distinct query must miss and invoke handler, got %d", calls.Load())
	}

	// After the TTL elapses the original key expires → handler runs again.
	time.Sleep(60 * time.Millisecond)
	do()
	if calls.Load() != 3 {
		t.Fatalf("expired entry should re-invoke handler, got %d", calls.Load())
	}
}

// TestRespCacheSkipsErrors verifies an error response is never memoised, so
// a transient failure does not get pinned for the whole TTL.
func TestRespCacheSkipsErrors(t *testing.T) {
	var calls atomic.Int64
	c := newRespCache(time.Minute)
	h := c.wrap(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	req := func() {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	}
	req()
	req()
	if calls.Load() != 2 {
		t.Fatalf("error responses must not be cached: expected 2 calls, got %d", calls.Load())
	}
}
