// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marcelocantos/mnemo/internal/replay"
	"github.com/marcelocantos/mnemo/internal/store"
	_ "github.com/mattn/go-sqlite3"
)

func cmdReplayFiles(args []string) {
	fs := flag.NewFlagSet("replay-files", flag.ExitOnError)
	session := fs.String("session", "", "session id (exact or prefix-resolved via DB)")
	since := fs.String("since", "", "RFC3339 lower bound (inclusive)")
	until := fs.String("until", "", "RFC3339 upper bound (inclusive)")
	repo := fs.String("repo", "", "filter session_meta.repo")
	source := fs.String("source", "", "filter source: claude|codex|grok|cursor")
	out := fs.String("out", "", "quarantine output directory (default ~/.mnemo/replay/<run-id>)")
	dryRun := fs.Bool("dry-run", false, "plan ops without writing files")
	allowLive := fs.Bool("allow-live-tree", false, "allow writing inside a git work tree")
	fileHistory := fs.Bool("file-history", false, "enrich empty Claude Write bodies from ~/.claude/file-history (optional)")
	jsonOut := fs.Bool("json", false, "emit machine-readable report JSON")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo replay-files [flags]

Reconstruct agent file writes from indexed transcripts into a quarantine tree.
See docs/design/transcript-file-replay.md.

  --session <id>       replay one session (optional with --since/--until)
  --since / --until    time window (intra- or multi-session)
  --repo / --source    optional filters
  --out <dir>          quarantine root (default ~/.mnemo/replay/<run-id>)
  --dry-run            list planned ops without writing
  --allow-live-tree    override git worktree guard (dangerous)
  --json               print report as JSON

Examples:
  mnemo replay-files --session 84369401 --dry-run
  mnemo replay-files --since 2026-08-01T00:00:00Z --until 2026-08-26T00:00:00Z --source cursor
`)
	}
	_ = fs.Parse(args)

	if *session == "" && *since == "" && *until == "" {
		fmt.Fprintln(os.Stderr, "replay-files: specify --session and/or --since/--until")
		os.Exit(2)
	}

	home, err := store.EffectiveHome()
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay-files: home: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(home, ".mnemo", "mnemo.db")
	db, err := replay.OpenReadOnly(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay-files: open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	scope := replay.Scope{SessionID: *session, Repo: *repo, Source: *source}
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay-files: --since: %v\n", err)
			os.Exit(2)
		}
		scope.Since = &t
	}
	if *until != "" {
		t, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay-files: --until: %v\n", err)
			os.Exit(2)
		}
		scope.Until = &t
	}

	rdb := replay.NewDB(db)
	ops, warns, err := rdb.CollectOps(scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay-files: collect: %v\n", err)
		os.Exit(1)
	}
	if fhWarns := replay.EnrichFileHistory(replay.ClaudeHome(), ops, *fileHistory); len(fhWarns) > 0 {
		warns = append(warns, fhWarns...)
	}

	outDir := *out
	if outDir == "" {
		runID := time.Now().UTC().Format("20060102T150405Z")
		outDir = filepath.Join(home, ".mnemo", "replay", runID)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil && !*dryRun {
		fmt.Fprintf(os.Stderr, "replay-files: mkdir out: %v\n", err)
		os.Exit(1)
	}

	cfg := replay.DefaultConfig()
	cfg.DryRun = *dryRun
	cfg.AllowLiveTree = *allowLive
	eng := replay.NewEngine(cfg)
	report, err := eng.Run(outDir, ops)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay-files: %v\n", err)
		os.Exit(1)
	}
	report.RunID = filepath.Base(outDir)
	report.Warnings = append(report.Warnings, warns...)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Printf("replay-files: quarantine=%s dry_run=%v\n", report.QuarantineRoot, report.DryRun)
		fmt.Printf("  planned=%d applied=%d skipped=%d failed=%d files=%d\n",
			report.OpsPlanned, report.OpsApplied, report.OpsSkipped, report.OpsFailed, len(report.FilesWritten))
		for _, w := range report.Warnings {
			fmt.Printf("  warn: %s\n", w)
		}
	}
	if report.OpsFailed > 0 {
		os.Exit(1)
	}
}
