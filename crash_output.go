// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"time"
)

// crashKeep is how many crash dumps ~/.mnemo/crash retains.
const crashKeep = 5

// installCrashOutput copies the runtime's fatal output (panics, thread
// exhaustion) into its own file under dir, in addition to stderr. A
// thread-exhaustion dump on 15 Oct 2026 ran past the supervisor log's
// whole rotation (10 MB x 3), so the log kept only the tail of the dump
// and none of what led up to it. Each daemon start opens a fresh file;
// empty files from clean runs and dumps beyond crashKeep are removed.
func installCrashOutput(dir string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	pruneCrashDumps(dir, crashKeep)
	path := filepath.Join(dir, fmt.Sprintf("crash-%s-%d.log", now.Format("20060102-150405"), os.Getpid()))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close() // SetCrashOutput dups the descriptor
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// pruneCrashDumps removes empty dumps (clean exits) and keeps the
// newest keep non-empty ones.
func pruneCrashDumps(dir string, keep int) {
	matches, _ := filepath.Glob(filepath.Join(dir, "crash-*.log"))
	sort.Strings(matches) // timestamped names sort oldest first
	var full []string
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.Size() == 0 {
			os.Remove(m)
			continue
		}
		full = append(full, m)
	}
	for len(full) > keep {
		os.Remove(full[0])
		full = full[1:]
	}
}

func setupCrashOutput() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path, err := installCrashOutput(filepath.Join(home, ".mnemo", "crash"), time.Now())
	if err != nil {
		slog.Warn("crash output not installed", "err", err)
		return
	}
	slog.Info("crash output", "path", path)
}
