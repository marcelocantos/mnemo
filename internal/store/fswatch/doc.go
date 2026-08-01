// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package fswatch provides FD-bounded tree watching for mnemo realtime ingest (🎯T142).
//
// # Platform backends
//
//   - Darwin: FSEvents on configured roots (O(roots) cost; not fsnotify/kqueue).
//   - Linux:  inotify via fsnotify, recursive directory watches with a hard cap.
//   - Windows: ReadDirectoryChangesW via fsnotify, recursive directory watches with a hard cap.
//
// fsnotify on Darwin uses kqueue, which opens every file in each watched
// directory to emulate inotify child writes. That failure mode produced the
// 2026-08-01 host panic (initproc exited / SIGBUS) when mnemo held ~72k FDs
// under ~/.claude/projects and ~/.grok/sessions (mnemo.db ~24–25GB; brew
// service homebrew.mxcl.mnemo left stopped as mitigation).
//
// # Steady-state bound
//
// Watch-related open FDs must stay well under 5k for a full corpus (target class
// O(roots) on Darwin, O(min(dirs, MaxDirWatches)) on Linux/Windows). Historical
// files are covered by the tree subscription or by the safety poll, never as
// individual permanent watches.
//
// # Safety poll
//
// All platforms run a bounded poll over incomplete / live / recent paths only —
// not a full cold-archive walk every tick — so open/write/close writers (e.g.
// Grok updates.jsonl) still reach ingest when tree events are coalesced or missed.
package fswatch
