// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package fswatch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// lsofTimeout bounds the fallback probe. lsof enumerates descriptors
// across the system and can take seconds on a busy machine; an unbounded
// probe once pinned the watcher and, through it, every Stats caller
// (🎯T153). A probe that cannot answer promptly is worth less than the
// latency it costs, so it is abandoned rather than waited on.
const lsofTimeout = 2 * time.Second

func openFDCountLsofImpl() int {
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-p", fmt.Sprint(os.Getpid())).Output()
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
