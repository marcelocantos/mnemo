// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package rpc provides mnemo-specific RPC wrappers around mcpbridge.
package rpc

import (
	"os"
	"path/filepath"

	"github.com/marcelocantos/mcpbridge"
)

// SocketPath returns the default Unix domain socket path for mnemo.
func SocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mnemo", "mnemo.sock")
}

// Dial connects to the mnemo daemon at the default socket path.
func Dial() (*mcpbridge.Client, error) {
	return mcpbridge.Dial(SocketPath())
}

// DialAt connects to the mnemo daemon at a specific socket path.
func DialAt(sockPath string) (*mcpbridge.Client, error) {
	return mcpbridge.Dial(sockPath)
}
