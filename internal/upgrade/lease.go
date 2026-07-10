// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultLeaseTTL is how long a holder may go without heartbeat
// before another process may steal the lease.
const DefaultLeaseTTL = 2 * time.Second

// DefaultHeartbeat is how often the holder rewrites its heartbeat.
const DefaultHeartbeat = 500 * time.Millisecond

// LeaseArgs configures a single-holder background lease (🎯T97.4).
type LeaseArgs struct {
	// Path is the lock file path (typically ~/.mnemo/background.lease).
	Path string
	// HolderID uniquely identifies this process (hostname:pid is fine).
	HolderID string
	// TTL is how long a heartbeat remains valid (default DefaultLeaseTTL).
	TTL time.Duration
	// Now is optional clock injection.
	Now func() time.Time
}

// Lease is a heartbeat file lock so exactly one backend runs
// singleton background work (ingest/compaction/mirrors/image pools).
type Lease struct {
	mu       sync.Mutex
	path     string
	holderID string
	ttl      time.Duration
	now      func() time.Time
	held     bool
	// runningBG is set by the registry when background workers are live.
	runningBG bool
}

// NewLease builds a Lease. Path and HolderID are required.
func NewLease(args *LeaseArgs) (*Lease, error) {
	if args == nil || args.Path == "" {
		return nil, fmt.Errorf("upgrade: lease path required")
	}
	if args.HolderID == "" {
		return nil, fmt.Errorf("upgrade: lease holder id required")
	}
	ttl := args.TTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	now := args.Now
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(args.Path), 0o755); err != nil {
		return nil, fmt.Errorf("upgrade: lease dir: %w", err)
	}
	return &Lease{
		path:     args.Path,
		holderID: args.HolderID,
		ttl:      ttl,
		now:      now,
	}, nil
}

// DefaultLeasePath returns ~/.mnemo/background.lease under home.
func DefaultLeasePath(home string) string {
	return filepath.Join(home, ".mnemo", "background.lease")
}

// DefaultHolderID returns "hostname:pid".
func DefaultHolderID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// TryAcquire attempts to take the lease. Returns true when this process
// holds it after the call.
func (l *Lease) TryAcquire() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tryAcquireLocked()
}

func (l *Lease) tryAcquireLocked() (bool, error) {
	now := l.now()
	data, err := os.ReadFile(l.path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil {
		holder, hb, ok := parseLeaseFile(string(data))
		if ok && holder != l.holderID {
			if now.Sub(hb) < l.ttl {
				l.held = false
				return false, nil // live holder
			}
			// expired — steal
		}
		if ok && holder == l.holderID {
			// refresh
			if err := l.writeLocked(now); err != nil {
				return false, err
			}
			l.held = true
			return true, nil
		}
	}
	if err := l.writeLocked(now); err != nil {
		return false, err
	}
	// Re-read to detect races: if another writer won, we may not hold.
	data2, err := os.ReadFile(l.path)
	if err != nil {
		l.held = false
		return false, err
	}
	holder, _, ok := parseLeaseFile(string(data2))
	if !ok || holder != l.holderID {
		l.held = false
		return false, nil
	}
	l.held = true
	return true, nil
}

func (l *Lease) writeLocked(now time.Time) error {
	content := fmt.Sprintf("%s\n%d\n", l.holderID, now.UnixNano())
	tmp := l.path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Heartbeat refreshes the lease if held. Safe to call periodically.
func (l *Lease) Heartbeat() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return nil
	}
	// Confirm we still own the file.
	data, err := os.ReadFile(l.path)
	if err != nil {
		l.held = false
		return err
	}
	holder, _, ok := parseLeaseFile(string(data))
	if !ok || holder != l.holderID {
		l.held = false
		return nil
	}
	return l.writeLocked(l.now())
}

// Release drops the lease if this process holds it.
func (l *Lease) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err == nil {
		holder, _, ok := parseLeaseFile(string(data))
		if ok && holder == l.holderID {
			_ = os.Remove(l.path)
		}
	}
	l.held = false
	l.runningBG = false
	return nil
}

// Held reports whether this process currently believes it holds the lease.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// SetRunningBackground records whether singleton background work is live.
func (l *Lease) SetRunningBackground(running bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runningBG = running
}

// RunningBackground reports the flag set by SetRunningBackground.
func (l *Lease) RunningBackground() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.runningBG
}

// Status is a diagnostic view of the lease file + local hold state.
type Status struct {
	Path           string
	HeldLocally    bool
	RunningBG      bool
	FileHolder     string
	FileHeartbeat  time.Time
	FilePresent    bool
	Expired        bool
	LocalHolderID  string
}

// Status inspects the lease file and local state for mnemo_doctor.
func (l *Lease) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := Status{
		Path:          l.path,
		HeldLocally:   l.held,
		RunningBG:     l.runningBG,
		LocalHolderID: l.holderID,
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return st
	}
	st.FilePresent = true
	holder, hb, ok := parseLeaseFile(string(data))
	if !ok {
		return st
	}
	st.FileHolder = holder
	st.FileHeartbeat = hb
	st.Expired = l.now().Sub(hb) >= l.ttl
	return st
}

// RunHeartbeatLoop refreshes the lease until ctx is done or the lease
// is released. interval defaults to DefaultHeartbeat.
func (l *Lease) RunHeartbeatLoop(ctxDone <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHeartbeat
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctxDone:
			return
		case <-t.C:
			_ = l.Heartbeat()
		}
	}
}

func parseLeaseFile(s string) (holder string, hb time.Time, ok bool) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 2 {
		return "", time.Time{}, false
	}
	holder = strings.TrimSpace(lines[0])
	ns, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil || holder == "" {
		return "", time.Time{}, false
	}
	return holder, time.Unix(0, ns), true
}
