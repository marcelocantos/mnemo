// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
)

// CurrentUsername returns the username of the process owner. Used as
// the default identity when an incoming MCP request does not carry a
// `?user=...` query parameter.
//
// On a Windows Service running as LocalSystem this returns "SYSTEM"
// (or similar) — which is not a useful identity for mnemo's purposes,
// so the service explicitly rejects the default path and requires
// every request to carry an explicit user.
func CurrentUsername() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	return normaliseUsername(u.Username), nil
}

// MnemoHomeEnv is the environment variable name that overrides the
// daemon's notion of "home" — the directory under which sidecar state
// (~/.mnemo/...) and the default user's data tree (~/.claude/...) are
// resolved (🎯T73). Tests and the snapshot helper set this so a test
// daemon launched with MNEMO_HOME=<tempdir> is *incapable* of reading
// or mutating the real $HOME-rooted state.
//
// MNEMO_HOME affects only the default-identity path. Explicit
// per-user lookups via ResolveHomeFor("alice") still consult the OS
// user database (multi-user Windows-Service deployments preserved).
const MnemoHomeEnv = "MNEMO_HOME"

// EffectiveHome returns the directory the daemon should treat as
// $HOME for sidecar state and default-user data. MNEMO_HOME wins
// when set; otherwise os.UserHomeDir() is consulted (🎯T73).
//
// This is the ONLY function in the codebase that should consult
// MNEMO_HOME / os.UserHomeDir() for daemon-state purposes. All other
// call sites must route through here (or through ResolveHomeFor)
// so MNEMO_HOME isolation holds end-to-end.
func EffectiveHome() (string, error) {
	if h := os.Getenv(MnemoHomeEnv); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve current home: %w", err)
	}
	return home, nil
}

// ResolveHomeFor returns the absolute home directory for the named
// user. On Windows this uses user.Lookup which, under the hood, calls
// LookupAccountName + GetUserProfileDirectory. On Unix it reads the
// user's entry from /etc/passwd (or getpwnam_r).
//
// The empty username resolves to EffectiveHome — the daemon's notion
// of "home," respecting MNEMO_HOME for test isolation.
//
// An explicit username that matches the process owner ALSO routes
// through EffectiveHome so MNEMO_HOME isolation holds end-to-end
// (the daemon's eager-start path passes the resolved default
// username, which would otherwise bypass MNEMO_HOME and load the
// real DB). A different explicit username — Alice on a Windows
// Service daemon running as SYSTEM — still consults the OS user
// database so multi-user deployments continue to read each user's
// real home.
func ResolveHomeFor(username string) (string, error) {
	if username == "" {
		return EffectiveHome()
	}
	if cur, err := CurrentUsername(); err == nil && cur == normaliseUsername(username) {
		return EffectiveHome()
	}
	u, err := user.Lookup(username)
	if err != nil {
		return "", fmt.Errorf("resolve home for %q: %w", username, err)
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("user %q has no home directory", username)
	}
	return u.HomeDir, nil
}

// ErrNoDefaultUser is returned by DefaultUsername when the process is
// running as a system service (no sensible implicit identity).
var ErrNoDefaultUser = errors.New("no default user (service context)")

// DefaultUsername returns the username to use when no `?user=` query
// parameter is provided. Under a Windows Service (LocalSystem),
// returns ErrNoDefaultUser — callers must reject such requests with a
// clear error rather than indexing the SYSTEM profile by accident.
func DefaultUsername() (string, error) {
	name, err := CurrentUsername()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" && isSystemAccount(name) {
		return "", ErrNoDefaultUser
	}
	return name, nil
}

// isSystemAccount recognises the well-known Windows service accounts
// whose profiles should never be used as mnemo's default identity.
func isSystemAccount(name string) bool {
	switch normaliseUsername(name) {
	case "SYSTEM",
		"LOCAL SERVICE",
		"NETWORK SERVICE",
		"NT AUTHORITY\\SYSTEM",
		"NT AUTHORITY\\LOCAL SERVICE",
		"NT AUTHORITY\\NETWORK SERVICE":
		return true
	}
	return false
}

// normaliseUsername uppercases well-known service accounts but leaves
// real user names (domain\user, bare names) alone. Matching is
// case-insensitive on Windows so we fold before comparing.
func normaliseUsername(name string) string {
	if runtime.GOOS == "windows" {
		// DOMAIN\user on Windows; keep the separator but upper-case
		// for case-insensitive comparison against service-account
		// names.
		return toUpperASCII(name)
	}
	return name
}

func toUpperASCII(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
