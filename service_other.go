// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

// Non-Windows stubs for the Windows Service hooks declared in
// service_windows.go. The corresponding subcommands error out with a
// clear message so non-technical users who accidentally run
// `mnemo install-service` on macOS or Linux get a useful hint
// (brew services on macOS, systemd on Linux, rather than SCM).
package main

import "fmt"

func runAsServiceIfUnderSCM(string) (bool, error) { return false, nil }

func installService([]string) error {
	return fmt.Errorf("install-service is only supported on Windows " +
		"(use `brew services start mnemo` on macOS)")
}

func uninstallService([]string) error {
	return fmt.Errorf("uninstall-service is only supported on Windows " +
		"(use `brew services stop mnemo` on macOS)")
}
