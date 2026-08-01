// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Watcher is a platform tree subscription that emits filtered path events.
// Production Store.Watch and vault watching use this interface only.
type Watcher interface {
	// Events delivers filtered path events. Closed after Close.
	Events() <-chan Event
	// Errors delivers backend errors (non-fatal unless channel closes).
	Errors() <-chan error
	// Close stops the backend and closes Events/Errors.
	Close() error
	// Backend returns a stable name: "fsevents", "fsnotify", or "none".
	Backend() string
	// DirWatchCount is the number of directory watches installed (fsnotify);
	// FSEvents reports the number of root streams (≈ roots).
	DirWatchCount() int
	// CapHit is true if the hard dir-watch cap stopped further expansion.
	CapHit() bool
	// Roots returns the roots actually watched (existing dirs only).
	Roots() []string
}

// New starts a platform-appropriate Watcher for cfg.Roots.
// Missing roots are skipped; if no root exists, a no-op watcher is returned.
func New(cfg Config) (Watcher, error) {
	roots := normalizeRoots(cfg.Roots)
	if len(roots) == 0 {
		return newNopWatcher(), nil
	}
	cfg.Roots = roots
	if cfg.MaxDirWatches <= 0 {
		cfg.MaxDirWatches = MaxDirWatches
	}
	if cfg.Latency <= 0 {
		cfg.Latency = 100 * time.Millisecond
	}
	return newPlatformWatcher(cfg)
}

func normalizeRoots(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range in {
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			abs = r
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		fi, err := os.Stat(abs)
		if err != nil || !fi.IsDir() {
			continue
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

type nopWatcher struct {
	events chan Event
	errors chan error
}

func newNopWatcher() *nopWatcher {
	ev := make(chan Event)
	er := make(chan error)
	close(ev)
	close(er)
	return &nopWatcher{events: ev, errors: er}
}

func (w *nopWatcher) Events() <-chan Event { return w.events }
func (w *nopWatcher) Errors() <-chan error { return w.errors }
func (w *nopWatcher) Close() error         { return nil }
func (w *nopWatcher) Backend() string      { return "none" }
func (w *nopWatcher) DirWatchCount() int   { return 0 }
func (w *nopWatcher) CapHit() bool         { return false }
func (w *nopWatcher) Roots() []string      { return nil }

// OpenFDCount returns the number of open FDs in this process. Used by the
// T142 FD oracle. Tries /dev/fd first, then lsof -p <pid>. Returns -1 if
// unavailable.
func OpenFDCount() int {
	if n := openFDCountDevFD(); n >= 0 {
		return n
	}
	return openFDCountLsof()
}

func openFDCountDevFD() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

func openFDCountLsof() int {
	// Avoid importing os/exec at package init; local import keeps watch.go light
	// enough for tiny GOOS builds.
	return openFDCountLsofImpl()
}

// SyntheticCorpus builds dirCount directories under root, each with filesPerDir
// empty files. Used by the FD-bound oracle.
func SyntheticCorpus(root string, dirCount, filesPerDir int) (fileCount int, err error) {
	for i := 0; i < dirCount; i++ {
		d := filepath.Join(root, fmt.Sprintf("d%04d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fileCount, err
		}
		for j := 0; j < filesPerDir; j++ {
			p := filepath.Join(d, fmt.Sprintf("f%04d.jsonl", j))
			if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
				return fileCount, err
			}
			fileCount++
		}
	}
	return fileCount, nil
}
