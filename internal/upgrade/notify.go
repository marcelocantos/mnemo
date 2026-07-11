// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import "sync"

// ToolsListChangedMethod is the MCP notification method for tool-list
// updates (MCP spec notifications/tools/list_changed). Kept as a string
// constant so this package does not import mcp-go.
const ToolsListChangedMethod = "notifications/tools/list_changed"

// ListChangedFunc is best-effort tools/list_changed after a version
// change (🎯T97.6 / plan criterion 4). Nil is a no-op.
type ListChangedFunc func(from, to string)

// NotificationBroadcaster is the mcp-go MCPServer surface we need for
// list_changed. Implemented by *server.MCPServer.
type NotificationBroadcaster interface {
	SendNotificationToAllClients(method string, params map[string]any)
}

// BroadcastListChanged sends tools/list_changed to all connected MCP
// clients. Safe if b is nil (no-op). Best-effort: callers ignore errors
// from the transport layer (SendNotificationToAllClients is void).
func BroadcastListChanged(b NotificationBroadcaster, from, to string) {
	if b == nil {
		return
	}
	b.SendNotificationToAllClients(ToolsListChangedMethod, map[string]any{
		"reason": "mnemo_upgrade",
		"from":   from,
		"to":     to,
	})
}

// SideEffects bundles per-upgrade session banners and list_changed.
// Used by auto-apply OnUpgrade and by the new process after loading
// upgrade-pending so both notice injection and list_changed fire on
// the real path.
type SideEffects struct {
	Notices     *NoticeTracker
	ListChanged ListChangedFunc
}

// OnVersionChange marks allowlisted sessions for one-time banners and
// best-effort notifies clients that tools may have changed.
func (s *SideEffects) OnVersionChange(sessions []string, from, to string) {
	if s == nil {
		return
	}
	if s.Notices != nil {
		s.Notices.MarkSessions(sessions, from, to)
	}
	if s.ListChanged != nil {
		s.ListChanged(from, to)
	}
}

// ListChangedHolder lets main register the MCP server's notification
// sender after construction (orchestrator is wired before the MCP
// server exists). Concurrent Set/Send is safe.
type ListChangedHolder struct {
	mu   sync.Mutex
	send ListChangedFunc
}

// Set installs the list_changed sender (may be nil to clear).
func (h *ListChangedHolder) Set(fn ListChangedFunc) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.send = fn
	h.mu.Unlock()
}

// Send invokes the registered sender if any. Never panics on nil.
func (h *ListChangedHolder) Send(from, to string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	fn := h.send
	h.mu.Unlock()
	if fn != nil {
		fn(from, to)
	}
}
