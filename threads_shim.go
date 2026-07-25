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
// multi-purpose native shim is still running.
const threadsShimCheckInterval = 30 * time.Second

// shimSupervisor launches and keeps alive the multi-purpose Mnemo.app
// (🎯T85.5, Integration §0.1). The shim is the sole presenter for health
// notifications (and optional menu-bar chrome, dashboard, threads UI).
// The daemon always supervises it when the app is installed — there is
// no menu_bar_app process gate. menu_bar_app only toggles whether the
// status item is shown (retained "ui" SSE event from main).
//
// The shim is its own signed .app (stable Accessibility / notification
// TCC identity). The daemon launches it via `open -g` (never steals
// focus) and relaunches it if it exits, so there is no separate install
// step or second LaunchAgent.
//
// Best-effort and conservative: only does anything on macOS and only
// when a Mnemo.app is found (at $MNEMO_THREADS_APP or a known install
// location). A daemon without the app is a silent no-op.
type shimSupervisor struct {
	app  string // resolved Mnemo.app path; "" disables the supervisor entirely
	wake chan struct{}
}

// newShimSupervisor resolves Mnemo.app once. The supervisor is inert (a
// permanent no-op) off macOS or when no app is found.
func newShimSupervisor() *shimSupervisor {
	s := &shimSupervisor{wake: make(chan struct{}, 1)}
	if runtime.GOOS == "darwin" {
		s.app = resolveThreadsApp()
	}
	return s
}

// run keeps Mnemo.app alive on a ticker (and on demand via poke). Returns
// immediately when there is nothing to supervise.
func (s *shimSupervisor) run(ctx context.Context) {
	if s.app == "" {
		if runtime.GOOS == "darwin" {
			slog.Debug("threads shim: no Mnemo.app found; native shim supervision disabled")
		}
		return
	}
	slog.Info("threads shim: supervisor started", "app", s.app)
	ticker := time.NewTicker(threadsShimCheckInterval)
	defer ticker.Stop()
	s.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile()
		case <-s.wake:
			s.reconcile()
		}
	}
}

func (s *shimSupervisor) reconcile() {
	launchThreadsShimIfNeeded(s.app)
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
