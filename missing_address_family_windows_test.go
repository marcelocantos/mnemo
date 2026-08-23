// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestMissingAddressFamilyWindowsWinsockErrnos(t *testing.T) {
	tests := []struct {
		name  string
		errno error
		want  bool
	}{
		{name: "address family unsupported", errno: windows.WSAEAFNOSUPPORT, want: true},
		{name: "address unavailable", errno: windows.WSAEADDRNOTAVAIL, want: true},
		{name: "protocol unsupported", errno: windows.WSAEPROTONOSUPPORT, want: true},
		{name: "address in use", errno: windows.WSAEADDRINUSE, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &net.OpError{
				Op:  "listen",
				Net: "tcp",
				Err: &os.SyscallError{Syscall: "bind", Err: tt.errno},
			}
			if got := missingAddressFamily(err); got != tt.want {
				t.Fatalf("missingAddressFamily(%v) = %v, want %v", tt.errno, got, tt.want)
			}
		})
	}
}
