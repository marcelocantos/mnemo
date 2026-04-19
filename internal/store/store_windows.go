// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package store

// runLsof is a no-op on Windows: there is no preinstalled equivalent of `lsof`
// that reports a process's open files without requiring an external utility
// (handle.exe from Sysinternals). Live-session discovery degrades gracefully —
// mnemo_whatsup returns an empty session list rather than crashing. The full
// transcript indexing pipeline is unaffected.
var runLsof = func(projectsDir string) []byte { return nil }

// runPsEnv is a no-op on Windows. Reading another process's environment block
// on Windows requires NtQueryInformationProcess or WMI and is not currently
// wired up; PWD-derived transcript resolution is disabled on Windows.
var runPsEnv = func(pids []string) []byte { return nil }

// runPsMetrics is a no-op on Windows. Per-PID RSS/CPU metrics would require
// tasklist /V parsing or WMI; not wired up yet.
var runPsMetrics = func(pids []string) []byte { return nil }

// collectVMStat returns zeroed system metrics on Windows. macOS memory
// pressure has no meaningful analogue here; Whatsup already gates the call
// on runtime.GOOS == "darwin", so this stub exists only to satisfy the
// linker on Windows.
func collectVMStat() SystemMetrics { return SystemMetrics{} }
