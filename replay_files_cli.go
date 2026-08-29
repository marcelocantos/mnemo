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
	seedFrom := fs.String("seed-from", "", "pre-image dir, git rev, or stash ref (tried first per path)")
	noSeed := fs.Bool("no-seed", false, "disable all forensics pre-image sources (Write/patch timeline only)")
	jsonOut := fs.Bool("json", false, "emit machine-readable report JSON")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo replay-files [flags]

Forensics recovery: reconstruct agent file activity into a quarantine tree,
seeding missing pre-images from every available source (git worktree/commit/
stash, Grok rewind_points, Claude file-history, stitched Read tool_results),
then applying Write/patch/delete ops in timestamp order.

See docs/design/transcript-file-replay.md.

  --session <id>       replay one session (optional with --since/--until)
  --since / --until    time window (intra- or multi-session)
  --repo / --source    optional filters
  --out <dir>          quarantine root (default ~/.mnemo/replay/<run-id>)
  --seed-from <ref>    explicit pre-image: directory, git rev, or stash@{n}
  --no-seed            Write/patch only (legacy MVP behaviour)
  --dry-run            list planned ops without writing
  --allow-live-tree    override git worktree guard (dangerous)
  --json               print report as JSON

Examples:
  mnemo replay-files --session 01a013f8 --dry-run
  mnemo replay-files --session 01a013f8 --seed-from HEAD
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

	seedCfg := replay.SeedConfig{
		DisableAll: *noSeed,
		SeedFrom:   *seedFrom,
		ClaudeHome: replay.ClaudeHome(),
	}
	seeds, seedWarns := replay.ResolveSeeds(db, ops, seedCfg)
	warns = append(warns, seedWarns...)

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
	report, err := eng.Run(outDir, ops, seeds...)
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
		fmt.Printf("replay-files: quarantine=%s dry_run=%v seeds=%d\n",
			report.QuarantineRoot, report.DryRun, len(report.Seeds))
		fmt.Printf("  planned=%d applied=%d skipped=%d failed=%d files=%d\n",
			report.OpsPlanned, report.OpsApplied, report.OpsSkipped, report.OpsFailed, len(report.FilesWritten))
		bySrc := map[replay.SeedSource]int{}
		for _, s := range report.Seeds {
			bySrc[s.Source]++
			fmt.Printf("  seed %s %s (%d bytes) %s\n", s.Source, s.AbsPath, len(s.Body), s.Detail)
		}
		for _, w := range report.Warnings {
			fmt.Printf("  warn: %s\n", w)
		}
	}
	if report.OpsFailed > 0 {
		os.Exit(1)
	}
}
