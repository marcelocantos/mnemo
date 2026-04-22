// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

// Non-Windows stubs for the Windows user-agent hooks in
// agent_windows.go. The corresponding subcommands return a friendly
// error pointing at brew services / systemd rather than silently
// succeeding.
package main

import "fmt"

func installAgent([]string) error {
	return fmt.Errorf("install-agent is only supported on Windows " +
		"(use `brew services start mnemo` on macOS)")
}

func uninstallAgent([]string) error {
	return fmt.Errorf("uninstall-agent is only supported on Windows " +
		"(use `brew services stop mnemo` on macOS)")
}
