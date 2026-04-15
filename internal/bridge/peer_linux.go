// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package mcpbridge

import (
	"fmt"
	"net"
	"syscall"
)

// peerPID returns the PID of the peer process on the far end of a
// Unix domain socket connection via SO_PEERCRED. Returns 0 and a
// non-nil error if the connection is not a UDS or the sockopt fails.
func peerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix conn: %T", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("syscall conn: %w", err)
	}
	var ucred *syscall.Ucred
	var sockErr error
	ctlErr := raw.Control(func(fd uintptr) {
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if ctlErr != nil {
		return 0, fmt.Errorf("raw.Control: %w", ctlErr)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("SO_PEERCRED: %w", sockErr)
	}
	return int(ucred.Pid), nil
}
