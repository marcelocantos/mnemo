// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"os"
	"path/filepath"
	"strings"
)

const claudeFileHistoryRoot = ".claude/file-history"

// EnrichFileHistory fills empty Write bodies from Claude file-history sidecars
// when enabled (🎯T150.3 / E12). Off by default.
func EnrichFileHistory(home string, ops []Op, enabled bool) []string {
	if !enabled || home == "" {
		return nil
	}
	var warns []string
	for i := range ops {
		op := &ops[i]
		if op.Source != "claude" && op.Source != "" {
			continue
		}
		if op.Kind != KindWrite || len(op.Body) > 0 {
			continue
		}
		base := filepath.Base(op.Path)
		if base == "" || op.SessionID == "" {
			warns = append(warns, ReasonFileHistoryMiss+": "+op.Path)
			continue
		}
		body, ok := lookupFileHistory(home, op.SessionID, base)
		if !ok {
			warns = append(warns, ReasonFileHistoryMiss+": "+op.Path)
			continue
		}
		op.Body = body
	}
	return warns
}

func lookupFileHistory(home, sessionID, basename string) ([]byte, bool) {
	root := filepath.Join(home, claudeFileHistoryRoot, sessionID)
	var found []byte
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) == basename {
			b, err := os.ReadFile(path)
			if err == nil {
				found = b
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found, len(found) > 0
}

// ClaudeHome returns the Claude config root (honours CLAUDE_HOME when set).
func ClaudeHome() string {
	if h := strings.TrimSpace(os.Getenv("CLAUDE_HOME")); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
