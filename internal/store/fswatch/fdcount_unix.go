// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package fswatch

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

func openFDCountLsofImpl() int {
	out, err := exec.Command("lsof", "-nP", "-p", fmt.Sprint(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	// Header + one line per FD (and sometimes cwd/txt). Count non-empty lines
	// after the header as a process open-file proxy.
	n := 0
	for i, line := range bytes.Split(out, []byte("\n")) {
		if i == 0 || len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		n++
	}
	return n
}
