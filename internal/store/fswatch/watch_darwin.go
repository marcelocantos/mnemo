// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package fswatch

import (
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsevents"
)

type darwinWatcher struct {
	cfg    Config
	stream *fsevents.EventStream
	events chan Event
	errors chan error
	roots  []string
	stop   chan struct{}

	mu     sync.Mutex
	closed bool
}

func newPlatformWatcher(cfg Config) (Watcher, error) {
	w := &darwinWatcher{
		cfg:    cfg,
		events: make(chan Event, 256),
		errors: make(chan error, 8),
		roots:  append([]string(nil), cfg.Roots...),
		stop:   make(chan struct{}),
	}

	// Device=0: absolute paths; events return absolute paths (often /private/...).
	es := &fsevents.EventStream{
		Paths:   append([]string(nil), cfg.Roots...),
		Latency: cfg.Latency,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}
	if err := es.Start(); err != nil {
		return nil, err
	}
	w.stream = es
	// Capture channel before Close can clear stream (loop must not deref nil).
	rawEvents := es.Events

	go w.loop(rawEvents)
	slog.Info("fswatch: FSEvents backend started", "roots", len(cfg.Roots), "backend", "fsevents")
	return w, nil
}

func (w *darwinWatcher) loop(rawEvents <-chan []fsevents.Event) {
	defer close(w.events)
	defer close(w.errors)
	for {
		select {
		case <-w.stop:
			return
		case batch, ok := <-rawEvents:
			if !ok {
				return
			}
			for _, raw := range batch {
				path := raw.Path
				if path == "" {
					continue
				}
				if !filepath.IsAbs(path) {
					path = "/" + strings.TrimPrefix(path, "/")
				}
				op := fseventsOp(raw.Flags)
				if op == 0 {
					if raw.Flags&fsevents.MustScanSubDirs != 0 {
						op = OpWrite
					} else {
						continue
					}
				}
				if !Interest(path, w.cfg.Mode) {
					continue
				}
				select {
				case w.events <- Event{Path: path, Op: op}:
				case <-w.stop:
					return
				default:
					// Drop under extreme flood; safety poll recovers.
				}
			}
		}
	}
}

func fseventsOp(flags fsevents.EventFlags) Op {
	var op Op
	if flags&fsevents.ItemCreated != 0 {
		op |= OpCreate
	}
	if flags&fsevents.ItemModified != 0 || flags&fsevents.ItemInodeMetaMod != 0 {
		op |= OpWrite
	}
	if flags&fsevents.ItemRemoved != 0 {
		op |= OpRemove
	}
	if flags&fsevents.ItemRenamed != 0 {
		op |= OpRename
	}
	return op
}

func (w *darwinWatcher) Events() <-chan Event { return w.events }
func (w *darwinWatcher) Errors() <-chan error { return w.errors }

func (w *darwinWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	close(w.stop)
	if w.stream != nil {
		w.stream.Stop()
		w.stream = nil
	}
	return nil
}

func (w *darwinWatcher) Backend() string    { return "fsevents" }
func (w *darwinWatcher) DirWatchCount() int { return len(w.roots) }
func (w *darwinWatcher) CapHit() bool       { return false }
func (w *darwinWatcher) Roots() []string {
	return append([]string(nil), w.roots...)
}
