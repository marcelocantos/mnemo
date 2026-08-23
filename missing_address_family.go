// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"syscall"
)

func missingAddressFamily(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && missingAddressFamilyErrno(errno)
}
