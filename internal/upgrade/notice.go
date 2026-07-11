// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"fmt"
	"sync"
)

// NoticeTracker records per-session upgrade banners so each session
// sees `mnemo upgraded vN -> vN+1` at most once (🎯T97.6).
type NoticeTracker struct {
	mu      sync.Mutex
	pending map[string]string // sessionID -> message
	shown   map[string]string // sessionID -> last delivered message (dedupe)
}

// NewNoticeTracker builds an empty tracker.
func NewNoticeTracker() *NoticeTracker {
	return &NoticeTracker{
		pending: map[string]string{},
		shown:   map[string]string{},
	}
}

// FormatNotice builds the canonical upgrade banner text.
func FormatNotice(from, to string) string {
	return fmt.Sprintf("mnemo upgraded %s -> %s", formatV(from), formatV(to))
}

// MarkSession queues a one-time notice for sessionID.
func (t *NoticeTracker) MarkSession(sessionID, from, to string) {
	if sessionID == "" || from == "" || to == "" {
		return
	}
	msg := FormatNotice(from, to)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.shown[sessionID] == msg {
		return // already delivered this upgrade
	}
	t.pending[sessionID] = msg
}

// MarkSessions queues the same notice for many sessions.
func (t *NoticeTracker) MarkSessions(sessionIDs []string, from, to string) {
	for _, id := range sessionIDs {
		t.MarkSession(id, from, to)
	}
}

// Consume returns the pending notice for sessionID and clears it.
// ok is false when there is nothing to show.
func (t *NoticeTracker) Consume(sessionID string) (msg string, ok bool) {
	if sessionID == "" {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	msg, ok = t.pending[sessionID]
	if !ok {
		return "", false
	}
	delete(t.pending, sessionID)
	t.shown[sessionID] = msg
	return msg, true
}

// PendingCount is for tests/diagnostics.
func (t *NoticeTracker) PendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}
