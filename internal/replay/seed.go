// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"time"
)

// SeedSource names where a quarantine pre-image came from (forensics audit).
type SeedSource string

const (
	SeedWrite         SeedSource = "write"        // in-timeline Write op
	SeedCLI           SeedSource = "seed_from"    // --seed-from path/ref
	SeedWorkTree      SeedSource = "git_worktree" // file still on disk under cwd
	SeedGitCommit     SeedSource = "git_commit"   // git show <rev>:path
	SeedGitStash      SeedSource = "git_stash"    // stash@{n}:path
	SeedGrokRewind    SeedSource = "grok_rewind"  // rewind_points.jsonl snapshot
	SeedClaudeHistory SeedSource = "claude_file_history"
	SeedReadResult    SeedSource = "read_tool_result" // stitched Read/read_file results
)

// Seed is a pre-image for one absolute path, applied before patches for that path.
type Seed struct {
	AbsPath  string
	Body     []byte
	Source   SeedSource
	Captured time.Time // when the pre-image was observed (best effort)
	Detail   string    // rev, stash ref, rewind prompt_index, etc.
}

// SeedConfig enables forensics sources. Zero value enables all (forensics default).
type SeedConfig struct {
	// DisableAll turns off every external seed (Write-timeline only).
	DisableAll bool

	SeedFrom       string // dir, git rev, or stash ref (explicit wins over auto git)
	UseWorkTree    *bool  // nil = true
	UseGitCommit   *bool
	UseGitStash    *bool
	UseRewind      *bool
	UseFileHist    *bool
	UseReadResults *bool

	ClaudeHome string
	GrokHome   string // empty → GROK_HOME or ~/.grok
}

func (c SeedConfig) enabled(p *bool) bool {
	if c.DisableAll {
		return false
	}
	if p == nil {
		return true
	}
	return *p
}
