// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package mcpbridge

import (
	"fmt"
	"net"
	"syscall"
)

// SOL_LOCAL / LOCAL_PEERPID are macOS-specific sockopts that return
// the peer's PID on a Unix domain socket. Defined in
// <sys/un.h>. Not exposed by the Go syscall package on Darwin, so
// we use their literal values.
const (
	darwinSOL_LOCAL     = 0 // SOL_LOCAL
	darwinLOCAL_PEERPID = 2 // LOCAL_PEERPID
)

// peerPID returns the PID of the peer process on the far end of a
// Unix domain socket connection. Returns 0 and a non-nil error if the
// connection is not a UDS or the sockopt is unavailable.
func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix conn: %T", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("syscall conn: %w", err)
	}
	var pid int
	var sockErr error
	ctlErr := raw.Control(func(fd uintptr) {
		pid, sockErr = syscall.GetsockoptInt(int(fd), darwinSOL_LOCAL, darwinLOCAL_PEERPID)
	})
	if ctlErr != nil {
		return 0, fmt.Errorf("raw.Control: %w", ctlErr)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("LOCAL_PEERPID: %w", sockErr)
	}
	return pid, nil
}
