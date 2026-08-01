// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import "time"

// Op is a bitset of path change kinds delivered to ingest.
type Op uint32

const (
	// OpCreate is a new path (file or directory entry).
	OpCreate Op = 1 << iota
	// OpWrite is content or metadata change on an existing path.
	OpWrite
	// OpRemove is deletion.
	OpRemove
	// OpRename is rename/move.
	OpRename
)

// Has reports whether o includes bit.
func (o Op) Has(bit Op) bool { return o&bit != 0 }

// Event is one path-oriented filesystem notification after filtering.
type Event struct {
	Path string
	Op   Op
}

// WatchMode selects which paths Interest accepts.
type WatchMode int

const (
	// ModeTranscript is Claude/Codex/Grok trees plus repo context sources.
	ModeTranscript WatchMode = iota
	// ModeVault is human-editable vault markdown (any .md under the root,
	// skipping hidden directories).
	ModeVault
)

// MaxDirWatches is the hard cap on recursive directory watches for fsnotify
// backends (Linux/Windows). Darwin FSEvents does not use per-dir watches.
// Exceeding the cap fails soft: further dirs are not added; safety poll and
// already-watched subtrees continue.
const MaxDirWatches = 4096

// Config configures a tree Watcher.
type Config struct {
	// Roots are absolute (or resolvable) directory paths to watch.
	Roots []string
	// Mode selects path interest filtering (default ModeTranscript).
	Mode WatchMode
	// MaxDirWatches overrides MaxDirWatches for fsnotify backends; 0 means default.
	MaxDirWatches int
	// Latency is coalescing delay for backends that support it (FSEvents).
	// Zero means a small default (~100ms).
	Latency time.Duration
}
