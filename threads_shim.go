// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// threadsShimCheckInterval is how often the supervisor re-checks that the
// menu-bar shim is still running.
const threadsShimCheckInterval = 30 * time.Second

// superviseThreadsShim launches and keeps alive the Mnemo menu-bar app
// (🎯T85.5, Integration §0.1). The shim is its own signed .app — for a stable
// Accessibility TCC identity — but the daemon launches it (via `open -g`, so it
// never steals focus) and relaunches it if it exits, so there is no separate
// install step or second LaunchAgent.
//
// It is best-effort and conservative: it only does anything on macOS and only
// when a Mnemo.app is actually found (at $MNEMO_THREADS_APP or a known
// install location). A daemon without the app installed is a silent no-op, so
// pulling this code never makes a menu-bar item appear unexpectedly.
func superviseThreadsShim(ctx context.Context) {
	if runtime.GOOS != "darwin" {
		return
	}
	app := resolveThreadsApp()
	if app == "" {
		slog.Debug("threads shim: no Mnemo.app found; menu-bar app not launched")
		return
	}
	slog.Info("threads shim: supervising menu-bar app", "app", app)

	launchThreadsShimIfNeeded(app)
	ticker := time.NewTicker(threadsShimCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			launchThreadsShimIfNeeded(app)
		}
	}
}

// resolveThreadsApp returns the path to Mnemo.app, or "" when none is
// found. $MNEMO_THREADS_APP wins; otherwise a few install-relative locations
// are probed (Homebrew libexec, alongside the daemon binary).
func resolveThreadsApp() string {
	if p := os.Getenv("MNEMO_THREADS_APP"); p != "" {
		if isDir(p) {
			return p
		}
		return ""
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "Mnemo.app"),
			filepath.Join(exeDir, "..", "libexec", "Mnemo.app"),
		)
	}
	candidates = append(candidates,
		"/opt/homebrew/opt/mnemo/libexec/Mnemo.app",
		"/usr/local/opt/mnemo/libexec/Mnemo.app",
	)
	for _, c := range candidates {
		if isDir(c) {
			return c
		}
	}
	return ""
}

// launchThreadsShimIfNeeded starts the shim with `open -g` unless an instance
// is already running.
func launchThreadsShimIfNeeded(app string) {
	if threadsShimRunning() {
		return
	}
	// -g: open in the background, without bringing the app to the foreground.
	if err := exec.Command("open", "-g", app).Run(); err != nil {
		slog.Warn("threads shim: launch failed", "app", app, "err", err)
	}
}

// threadsShimRunning reports whether a Mnemo process is alive.
func threadsShimRunning() bool {
	// pgrep -x matches the process name exactly; exit 0 means at least one.
	return exec.Command("pgrep", "-x", "Mnemo").Run() == nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
