// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// showMarkdownCLI renders a CLAUDE.md body for `thread show` (🎯T85.3,
// Integration §0.8). When w is a terminal and `glow` is on PATH, the markdown
// is piped through glow (which detects width from the connected TTY, with a
// $COLUMNS / 100-column fallback); otherwise the raw markdown is written.
// glow is terminal-only, so it serves the CLI path exclusively — the GUI
// preview goes through internal/render instead.
func showMarkdownCLI(w io.Writer, body string) {
	if f, ok := w.(*os.File); ok && isTerminal(f) {
		if path, err := exec.LookPath("glow"); err == nil {
			cmd := exec.Command(path, "-w", strconv.Itoa(glowWidth()), "-")
			cmd.Stdin = strings.NewReader(body)
			cmd.Stdout = w
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err == nil {
				return
			}
			// Fall through to raw on any glow failure.
		}
	}
	fmt.Fprint(w, body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Fprintln(w)
	}
}

// isTerminal reports whether f is a character device (a TTY).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// glowWidth returns the wrap width for glow: $COLUMNS when set and valid,
// else 100. (glow also self-detects from the TTY; this is the documented
// fallback chain.)
func glowWidth() int {
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 {
			return n
		}
	}
	return 100
}
