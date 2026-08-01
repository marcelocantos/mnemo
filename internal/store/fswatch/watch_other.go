// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package fswatch

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// fsnotifyWatcher walks roots and Add's directories up to MaxDirWatches.
// On Linux this is inotify (no per-file FD). On Windows, ReadDirectoryChangesW.
// Cap fail-soft: stop adding dirs, set CapHit, keep running.
type fsnotifyWatcher struct {
	cfg      Config
	watcher  *fsnotify.Watcher
	events   chan Event
	errors   chan error
	roots    []string
	dirCount int
	capHit   bool

	mu     sync.Mutex
	closed bool
}

func newPlatformWatcher(cfg Config) (Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &fsnotifyWatcher{
		cfg:     cfg,
		watcher: fw,
		events:  make(chan Event, 256),
		errors:  make(chan error, 8),
		roots:   append([]string(nil), cfg.Roots...),
	}
	for _, root := range cfg.Roots {
		if err := w.addTree(root); err != nil {
			slog.Warn("fswatch: addTree failed", "root", root, "err", err)
		}
	}
	go w.loop()
	slog.Info("fswatch: fsnotify backend started",
		"roots", len(cfg.Roots),
		"dirs", w.dirCount,
		"cap_hit", w.capHit,
		"backend", "fsnotify",
	)
	return w, nil
}

func (w *fsnotifyWatcher) addTree(root string) error {
	max := w.cfg.MaxDirWatches
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if w.dirCount >= max {
			w.capHit = true
			return fs.SkipAll
		}
		if addErr := w.watcher.Add(path); addErr != nil {
			slog.Warn("fswatch: failed to watch directory", "path", path, "err", addErr)
			return nil
		}
		w.dirCount++
		return nil
	})
}

func (w *fsnotifyWatcher) loop() {
	defer close(w.events)
	defer close(w.errors)
	for {
		select {
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			select {
			case w.errors <- err:
			default:
			}
		}
	}
}

func (w *fsnotifyWatcher) handleEvent(ev fsnotify.Event) {
	name := ev.Name
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(name); err == nil && fi.IsDir() {
			base := filepath.Base(name)
			if base != "" && !strings.HasPrefix(base, ".") {
				w.mu.Lock()
				under := !w.closed && w.dirCount < w.cfg.MaxDirWatches
				w.mu.Unlock()
				if under {
					if err := w.watcher.Add(name); err == nil {
						w.mu.Lock()
						w.dirCount++
						if w.dirCount >= w.cfg.MaxDirWatches {
							w.capHit = true
						}
						w.mu.Unlock()
					}
				} else {
					w.mu.Lock()
					w.capHit = true
					w.mu.Unlock()
				}
			}
		}
	}

	var op Op
	if ev.Has(fsnotify.Create) {
		op |= OpCreate
	}
	if ev.Has(fsnotify.Write) {
		op |= OpWrite
	}
	if ev.Has(fsnotify.Remove) {
		op |= OpRemove
	}
	if ev.Has(fsnotify.Rename) {
		op |= OpRename
	}
	if op == 0 {
		return
	}
	if !Interest(name, w.cfg.Mode) {
		return
	}
	select {
	case w.events <- Event{Path: name, Op: op}:
	default:
	}
}

func (w *fsnotifyWatcher) Events() <-chan Event { return w.events }
func (w *fsnotifyWatcher) Errors() <-chan error { return w.errors }

func (w *fsnotifyWatcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	return w.watcher.Close()
}

func (w *fsnotifyWatcher) Backend() string { return "fsnotify" }
func (w *fsnotifyWatcher) DirWatchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dirCount
}
func (w *fsnotifyWatcher) CapHit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.capHit
}
func (w *fsnotifyWatcher) Roots() []string {
	return append([]string(nil), w.roots...)
}
