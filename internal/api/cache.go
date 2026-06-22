// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// respCache is a tiny TTL cache for read-only analytics responses (🎯T92).
// The dashboard's heavy endpoints (usage/activity/context/stats/dbstats)
// run full aggregate scans of a large DB and change slowly; without a
// cache, an open dashboard re-runs them on every poll/refresh, sustaining
// multi-second queries against the read pool. Keying by full request URL
// (path + query) collapses identical requests within the TTL into one
// query. Values are immutable response bodies, so reads are lock-light.
type respCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]cacheEntry
}

type cacheEntry struct {
	at    time.Time
	body  []byte
	ctype string
}

func newRespCache(ttl time.Duration) *respCache {
	return &respCache{ttl: ttl, m: map[string]cacheEntry{}}
}

// wrap returns a handler that serves a fresh cached body when one exists,
// otherwise runs next, streaming its output to the client while buffering
// it for the cache. Only 200 responses are cached, so an error is never
// memoised. A cache hit sets X-Mnemo-Cache: hit for observability.
func (c *respCache) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.String()
		c.mu.Lock()
		e, ok := c.m[key]
		fresh := ok && time.Since(e.at) < c.ttl
		c.mu.Unlock()
		if fresh {
			if e.ctype != "" {
				w.Header().Set("Content-Type", e.ctype)
			}
			w.Header().Set("X-Mnemo-Cache", "hit")
			_, _ = w.Write(e.body)
			return
		}

		rec := &bufRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		if rec.status == http.StatusOK {
			c.mu.Lock()
			c.m[key] = cacheEntry{at: time.Now(), body: rec.buf.Bytes(), ctype: w.Header().Get("Content-Type")}
			c.mu.Unlock()
		}
	}
}

// bufRecorder tees a handler's response to the client and an internal
// buffer so a successful body can be cached without delaying the client.
type bufRecorder struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (b *bufRecorder) WriteHeader(status int) {
	b.status = status
	b.ResponseWriter.WriteHeader(status)
}

func (b *bufRecorder) Write(p []byte) (int, error) {
	b.buf.Write(p)
	return b.ResponseWriter.Write(p)
}
