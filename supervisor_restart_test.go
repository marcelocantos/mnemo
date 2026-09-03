// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

func TestUnderSupervisor(t *testing.T) {
	t.Setenv("SUPERVISOR_ENABLED", "")
	if underSupervisor() {
		t.Fatal("empty SUPERVISOR_ENABLED must not look like supervisord")
	}
	t.Setenv("SUPERVISOR_ENABLED", "1")
	if !underSupervisor() {
		t.Fatal("SUPERVISOR_ENABLED=1 is how supervisord marks its children")
	}
}

// TestRestartManagedDaemonGatesBrewServices is the 🎯T164 ratchet:
// brew services restart from auto-upgrade would reload launchd and
// fight supervisord for :19419. The only call site must sit behind
// underSupervisor().
func TestRestartManagedDaemonGatesBrewServices(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "func restartManagedDaemon") {
		t.Fatal("restartManagedDaemon is gone — auto-upgrade has no supervisor-safe bounce")
	}
	if strings.Count(body, "runBrewServicesRestart(") != 2 {
		t.Fatalf("runBrewServicesRestart must be defined once and called once from restartManagedDaemon; found %d",
			strings.Count(body, "runBrewServicesRestart("))
	}
	idx := strings.Index(body, "func restartManagedDaemon")
	next := strings.Index(body[idx+1:], "\nfunc ")
	if next < 0 {
		t.Fatal("could not bound restartManagedDaemon")
	}
	fn := body[idx : idx+1+next]
	if !strings.Contains(fn, "underSupervisor()") {
		t.Fatal("restartManagedDaemon no longer checks underSupervisor")
	}
	if !strings.Contains(fn, "runBrewServicesRestart") {
		t.Fatal("restartManagedDaemon no longer owns the brew-services path")
	}
	if !strings.Contains(body, "return restartManagedDaemon(ctx)") {
		t.Fatal("DrainOld no longer goes through restartManagedDaemon")
	}
}
