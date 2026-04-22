// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

// Windows user-agent registration for mnemo.
//
// mnemo does NOT run as a Windows Service. A Service runs as
// LocalSystem (or a configured non-user account), and
// os.UserHomeDir() in that context returns
// C:\Windows\System32\config\systemprofile — not the user's real
// home — so the transcript indexer looks in the wrong place and
// finds zero sessions. This was how v0.22.0 shipped and why it
// indexed nothing on Windows.
//
// Instead, the installer registers mnemo as a per-user Scheduled
// Task triggered AtLogon. The task runs in the user's own session,
// so os.UserHomeDir() correctly returns C:\Users\<them> and the
// indexer finds ~/.claude/projects/. This is the standard Windows
// idiom for a persistent per-user background agent (Dropbox,
// OneDrive, cloud-sync utilities, etc.).
//
// Two entry points, each wired up from main.go:
//
//   - installAgent: registers the "mnemo" task to run mnemo.exe
//     at the invoking user's logon with limited (non-elevated)
//     privileges. Called by `mnemo install-agent` from the installer.
//     The installer invokes this with `runasoriginaluser` so the task
//     is registered for the real user, not the installer's elevated
//     SYSTEM/Administrator context.
//
//   - uninstallAgent: removes the "mnemo" task. Called by
//     `mnemo uninstall-agent` from the uninstaller (or by the new
//     installer's pre-install uninstall step when upgrading over a
//     v0.22.0 Service-based install).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// taskName is the Scheduled Task identifier used by install-agent /
// uninstall-agent. Task names are case-insensitive on Windows.
const taskName = "mnemo"

// installAgent registers mnemo as a Scheduled Task that runs at the
// invoking user's logon. Idempotent: the /f flag overwrites any
// existing task of the same name. Also cleans up any v0.22.0-era
// Windows Service of the same name so upgrade-in-place works.
func installAgent(args []string) error {
	fs := flag.NewFlagSet("install-agent", flag.ExitOnError)
	exePath := fs.String("exe", "", "path to mnemo.exe (default: current executable)")
	_ = fs.Parse(args)

	path, err := resolveExe(*exePath)
	if err != nil {
		return err
	}

	// Clean up any v0.22.0 Windows Service with the same name before
	// registering the Scheduled Task. Ignore failures — if there's no
	// service, that's the expected fresh-install path.
	cleanupLegacyService()

	// /sc onlogon with no /ru defaults to the current (invoking) user,
	// which is what we want — this subcommand is expected to run via
	// Inno Setup's `runasoriginaluser` flag, so the current user IS
	// the end user.
	//
	// /rl limited keeps the task from inheriting admin rights (it
	// shouldn't need any — it binds to localhost:19419 and reads the
	// user's own files).
	//
	// /f forces overwrite of an existing task of the same name.
	cmd := exec.Command("schtasks.exe",
		"/create",
		"/tn", taskName,
		"/tr", "\""+path+"\"",
		"/sc", "onlogon",
		"/rl", "limited",
		"/f",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /create failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("scheduled task %q registered (exe=%s)\n", taskName, path)

	// Start the task immediately so the user doesn't have to log out
	// and back in. schtasks /run invokes the task with its registered
	// user context, so mnemo launches in the user's session with the
	// correct HOME.
	run := exec.Command("schtasks.exe", "/run", "/tn", taskName)
	if runOut, runErr := run.CombinedOutput(); runErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not start task immediately: %v\n%s\n",
			runErr, strings.TrimSpace(string(runOut)))
	} else {
		fmt.Printf("scheduled task %q started\n", taskName)
	}
	return nil
}

// uninstallAgent removes the mnemo Scheduled Task and any v0.22.0-era
// Windows Service of the same name. Idempotent — missing task is
// fine.
func uninstallAgent(args []string) error {
	_ = args

	// Terminate any running instance (launched by the logon trigger or
	// `schtasks /run`) before deleting the task, so the .exe file is
	// releasable for uninstaller cleanup.
	end := exec.Command("schtasks.exe", "/end", "/tn", taskName)
	_, _ = end.CombinedOutput() // best effort

	cmd := exec.Command("schtasks.exe", "/delete", "/tn", taskName, "/f")
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		// schtasks returns an error when the task does not exist. That
		// is the idempotent happy path for uninstall.
		if strings.Contains(outStr, "cannot find") || strings.Contains(outStr, "does not exist") {
			fmt.Printf("scheduled task %q not registered\n", taskName)
		} else {
			fmt.Fprintf(os.Stderr, "warning: schtasks /delete: %v\n%s\n", err, outStr)
		}
	} else {
		fmt.Printf("scheduled task %q removed\n", taskName)
	}

	cleanupLegacyService()
	return nil
}

// cleanupLegacyService removes a v0.22.0-era Windows Service named
// "mnemo", if present. v0.23.0 switched from Service to Scheduled
// Task; upgrades over a v0.22.0 install need the old service torn
// down so its registration doesn't sit there as a phantom.
//
// Uses sc.exe directly rather than the x/sys/windows/svc/mgr
// package: the SCM dependency is gone otherwise, and two `sc.exe`
// invocations are trivial.
func cleanupLegacyService() {
	// `sc stop` returns nonzero if the service is already stopped or
	// doesn't exist — either outcome is fine.
	stop := exec.Command("sc.exe", "stop", taskName)
	_, _ = stop.CombinedOutput()
	del := exec.Command("sc.exe", "delete", taskName)
	if out, err := del.CombinedOutput(); err == nil {
		fmt.Printf("legacy Windows Service %q removed\n", taskName)
		_ = out
	}
}

func resolveExe(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve --exe: %w", err)
		}
		return abs, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	return path, nil
}
