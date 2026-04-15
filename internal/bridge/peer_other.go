// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux

package mcpbridge

import (
	"fmt"
	"net"
)

// peerPID is a no-op on platforms without a clean peer-PID sockopt
// (notably Windows). Callers should fall back to the handshake-reported
// PID. Returns 0 and a non-nil error.
func peerPID(_ net.Conn) (int, error) {
	return 0, fmt.Errorf("peer PID not supported on this platform")
}
