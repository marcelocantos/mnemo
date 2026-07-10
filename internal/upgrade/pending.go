// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// PendingNoticeFile is the basename under ~/.mnemo/ for a one-shot
// upgrade banner payload (🎯T97.6). The new process loads it once,
// queues notices only for listed sessions, and deletes the file.
const PendingNoticeFile = "upgrade-pending"

// PendingNotice is the on-disk form written before a backend swap/restart.
type PendingNotice struct {
	From     string
	To       string
	Sessions []string // only these session IDs get a banner
}

// PendingPath returns ~/.mnemo/upgrade-pending under home.
func PendingPath(home string) string {
	return filepath.Join(home, ".mnemo", PendingNoticeFile)
}

// WritePendingNotice atomically writes the pending notice file.
func WritePendingNotice(home string, p PendingNotice) error {
	if home == "" {
		return fmt.Errorf("upgrade: empty home")
	}
	dir := filepath.Join(home, ".mnemo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(p.From))
	b.WriteByte('\n')
	b.WriteString(strings.TrimSpace(p.To))
	b.WriteByte('\n')
	seen := map[string]struct{}{}
	for _, s := range p.Sessions {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	path := PendingPath(home)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadAndConsumePending reads the pending notice file, queues banners
// for the listed sessions only, and deletes the file. Safe if missing.
func LoadAndConsumePending(home string, tr *NoticeTracker) (PendingNotice, bool, error) {
	if home == "" || tr == nil {
		return PendingNotice{}, false, nil
	}
	path := PendingPath(home)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PendingNotice{}, false, nil
		}
		return PendingNotice{}, false, err
	}
	// Delete first so a crash mid-queue does not re-banner forever.
	_ = os.Remove(path)

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	// trim trailing empties
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return PendingNotice{}, false, fmt.Errorf("upgrade: malformed pending notice")
	}
	p := PendingNotice{
		From: strings.TrimSpace(lines[0]),
		To:   strings.TrimSpace(lines[1]),
	}
	for _, line := range lines[2:] {
		if s := strings.TrimSpace(line); s != "" {
			p.Sessions = append(p.Sessions, s)
		}
	}
	tr.MarkSessions(p.Sessions, p.From, p.To)
	return p, true, nil
}

// SessionSet tracks MCP session IDs seen by this process (for T97.6
// allowlists at upgrade time).
type SessionSet struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

// NewSessionSet builds an empty set.
func NewSessionSet() *SessionSet {
	return &SessionSet{ids: map[string]struct{}{}}
}

// Add records a session ID.
func (s *SessionSet) Add(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	s.ids[id] = struct{}{}
	s.mu.Unlock()
}

// Snapshot returns a stable copy of known session IDs.
func (s *SessionSet) Snapshot() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}
