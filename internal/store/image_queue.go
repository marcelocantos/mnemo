// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Session image extraction runs off the ingest path. Before this queue
// every transcript append started its own goroutine; each blocked in
// SQLite or cgo (OCR) holds an OS thread, and on 15 Oct 2026 a burst of
// appends during the stem backfill reached Go's 10,000-thread limit and
// the daemon exited 2. The queue bounds concurrency to imageWorkers and
// coalesces work per session: an append for a session that is already
// queued adds nothing (the scan reads the session's latest entries),
// and one for a session being scanned schedules a single rerun.

// imageWorkers is the most session-image scans that run at once.
var imageWorkers = 2

// imageScanHook replaces the scan in tests. Production leaves it nil.
var imageScanHook func(sessionID string)

type imageQueue struct {
	mu      sync.Mutex
	queue   []string
	state   map[string]uint8 // imgQueued, imgRunning, imgRerun
	workers int
	active  atomic.Int32 // scans in progress, read by the stem backfill
	peak    atomic.Int32 // high-water mark of workers, for tests
}

const (
	imgQueued uint8 = iota + 1
	imgRunning
	imgRerun
)

// enqueueSessionImages schedules an image scan for sessionID.
func (s *Store) enqueueSessionImages(sessionID string) {
	q := &s.imgQ
	q.mu.Lock()
	if q.state == nil {
		q.state = map[string]uint8{}
	}
	switch q.state[sessionID] {
	case imgQueued, imgRerun:
		q.mu.Unlock()
		return
	case imgRunning:
		q.state[sessionID] = imgRerun
		q.mu.Unlock()
		return
	}
	q.state[sessionID] = imgQueued
	q.queue = append(q.queue, sessionID)
	spawn := q.workers < imageWorkers
	if spawn {
		q.workers++
		if n := int32(q.workers); n > q.peak.Load() {
			q.peak.Store(n)
		}
	}
	q.mu.Unlock()
	if spawn {
		s.goOnce("session-images", s.imageWorker)
	}
}

// imageWorker drains the queue and exits when it is empty.
func (s *Store) imageWorker(ctx context.Context) error {
	q := &s.imgQ
	for {
		q.mu.Lock()
		if len(q.queue) == 0 || ctx.Err() != nil {
			q.workers--
			q.mu.Unlock()
			return nil
		}
		sid := q.queue[0]
		q.queue = q.queue[1:]
		q.state[sid] = imgRunning
		q.active.Add(1)
		q.mu.Unlock()

		err := s.scanSessionImages(sid)

		q.mu.Lock()
		q.active.Add(-1)
		if q.state[sid] == imgRerun {
			q.state[sid] = imgQueued
			q.queue = append(q.queue, sid)
		} else {
			delete(q.state, sid)
		}
		q.mu.Unlock()
		if err != nil {
			slog.Warn("background work failed", "worker", "session-images", "err", err)
		}
	}
}

// imagesBusy reports whether a session-image scan is running or queued.
func (s *Store) imagesBusy() bool {
	q := &s.imgQ
	if q.active.Load() > 0 {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue) > 0
}

// yieldToImages holds the stem backfill while image scans are pending,
// up to stemMaxPause, so a backfill batch's decompression does not
// compete with them for the write lock and OS threads.
func (s *Store) yieldToImages(ctx context.Context) {
	deadline := time.Now().Add(stemMaxPause)
	for s.imagesBusy() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
