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
	"sync/atomic"
	"time"
)

// threadsShimCheckInterval is how often the supervisor re-checks that the
// menu-bar shim is still running.
const threadsShimCheckInterval = 30 * time.Second

// shimSupervisor launches and keeps alive the Mnemo menu-bar app (🎯T85.5,
// Integration §0.1), gated on the menu_bar_app config flag — which is
// hot-reloadable: SetEnabled wires the running daemon to the live config so
// toggling menu_bar_app via mnemo_config takes effect immediately, no
// restart. The shim is its own signed .app (a stable Accessibility TCC
// identity) but the daemon launches it (via `open -g`, so it never steals
// focus) and relaunches it if it exits, so there is no separate install step
// or second LaunchAgent.
//
// It is best-effort and conservative: it only does anything on macOS and
// only when a Mnemo.app is actually found (at $MNEMO_THREADS_APP or a known
// install location). A daemon without the app installed is a silent no-op,
// so pulling this code never makes a menu-bar item appear unexpectedly.
type shimSupervisor struct {
	app     string      // resolved Mnemo.app path; "" disables the supervisor entirely
	enabled atomic.Bool // mirrors menu_bar_app from live config
	wake    chan struct{}
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

// SetEnabled records the desired state (menu_bar_app) and pokes the
// supervisor to reconcile immediately. Called at startup with the initial
// config and from configController.Put on every live config change.
func (s *shimSupervisor) SetEnabled(on bool) {
	s.enabled.Store(on)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run reconciles the menu-bar app against the desired state on a ticker and
// on demand (SetEnabled). When enabled it keeps Mnemo.app running; when
// disabled it stops launching it (a running instance is left alone rather
// than force-quit, so a manually-launched app is never killed out from
// under the user — it simply won't be relaunched). Returns immediately when
// there is nothing to supervise.
func (s *shimSupervisor) run(ctx context.Context) {
	if s.app == "" {
		if runtime.GOOS == "darwin" {
			slog.Debug("threads shim: no Mnemo.app found; menu-bar supervision disabled")
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
	if s.enabled.Load() {
		launchThreadsShimIfNeeded(s.app)
	}
	// Disabled: do nothing. We deliberately don't force-quit a running app —
	// it may have been launched manually, and killing it on every tick would
	// be hostile. It just won't be relaunched once it exits.
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
