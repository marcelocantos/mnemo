// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package store

import (
	"os/exec"
	"runtime"
	"strings"
)

// runLsof runs lsof to find JSONL files open by any claude process under dir.
// Parameterised as a package variable so tests can override it without
// requiring the actual binary; parseLsofOutput is tested directly against
// synthetic fixtures.
var runLsof = func(projectsDir string) []byte {
	out, _ := exec.Command("lsof", "-c", "claude", "-a", "+D", projectsDir).Output()
	return out
}

// runPsEnv is a test seam: in production it invokes ps to capture each PID's
// environment block (for PWD extraction).
var runPsEnv = func(pids []string) []byte {
	// ps -wwEo pid,command shows env vars appended after the command line.
	args := append([]string{"-wwEo", "pid,command", "-p"}, strings.Join(pids, ","))
	out, _ := exec.Command("ps", args...).Output()
	return out
}

// runPsMetrics is a test seam: in production it invokes ps to capture each
// PID's resident set size, CPU percent, and accumulated CPU time.
var runPsMetrics = func(pids []string) []byte {
	args := append([]string{"-o", "pid=,rss=,%cpu=,time=", "-p"}, strings.Join(pids, ","))
	out, _ := exec.Command("ps", args...).Output()
	return out
}

// collectVMStat produces macOS memory pressure via vm_stat on darwin. On other
// Unix platforms the caller (Whatsup) gates on runtime.GOOS == "darwin" before
// calling this, but the stub here is harmless and returns zeros.
func collectVMStat() SystemMetrics {
	if runtime.GOOS != "darwin" {
		return SystemMetrics{}
	}
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return SystemMetrics{}
	}
	return parseVMStatOutput(out)
}
