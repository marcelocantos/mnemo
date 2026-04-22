// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

// Windows subsystem handling.
//
// mnemo.exe is built with `-ldflags -H=windowsgui` on Windows so the
// Scheduled Task launched at logon does NOT pop a console window —
// which was the v0.23.0 symptom when launched via `schtasks /run`.
//
// But `-H=windowsgui` also means that when a user runs
// `mnemo --version` or `mnemo --help-agent` from PowerShell / cmd,
// there's no attached console so stdout/stderr go nowhere and the
// user sees silence. That's a worse CLI UX than the console-window
// regression we're fixing.
//
// Workaround: call AttachConsole(ATTACH_PARENT_PROCESS) at init
// time. If the process was launched from a shell (parent has a
// console), we attach to it and reopen the Go standard streams
// against CONOUT$/CONIN$, so `mnemo --version` prints as expected.
// If the process was launched without a parent console (Scheduled
// Task, double-click, etc.), the call fails silently and we stay
// headless — no window ever appears.
package main

import (
	"os"
	"syscall"
)

// ATTACH_PARENT_PROCESS per
// https://learn.microsoft.com/windows/console/attachconsole — the
// WinAPI constant is DWORD(-1), i.e. the uintptr with all bits set.
const attachParentProcess = ^uintptr(0)

func init() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	attach := kernel32.NewProc("AttachConsole")
	ret, _, _ := attach.Call(attachParentProcess)
	if ret == 0 {
		// No parent console (Scheduled Task, Explorer double-click,
		// etc.). Stay headless — Go's default stdout/stderr *Files are
		// already no-ops in that case, which is fine.
		return
	}

	// Parent console attached. Re-wire Go's stdio to it so fmt.Println
	// and log.* actually show up in the parent shell.
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = f
	}
}
