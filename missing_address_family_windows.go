// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

func missingAddressFamilyErrno(errno syscall.Errno) bool {
	return errno == windows.WSAEAFNOSUPPORT ||
		errno == windows.WSAEADDRNOTAVAIL ||
		errno == windows.WSAEPROTONOSUPPORT
}
