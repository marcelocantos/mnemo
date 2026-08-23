// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import "syscall"

func missingAddressFamilyErrno(errno syscall.Errno) bool {
	return errno == syscall.EAFNOSUPPORT ||
		errno == syscall.EADDRNOTAVAIL ||
		errno == syscall.EPROTONOSUPPORT
}
