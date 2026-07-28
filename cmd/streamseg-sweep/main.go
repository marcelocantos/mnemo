// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command streamseg-sweep measures the streaming segmenter's operating
// point (🎯T132.4).
//
// It replays historical sessions through the automaton as though they
// were live, then scores the boundaries it drew against the ones the
// batch summariser drew with the whole window in front of it. That gold
// already exists in the index, so the sweep needs no live session, no
// production watcher, and — importantly — no writes: the database is
// opened read-only and the replay collects spans in memory.
//
// Usage:
//
//	streamseg-sweep --dry-run              # harness check, no model calls
//	streamseg-sweep --sessions 4 --models sonnet,haiku
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/streamseg"
)

func main() {
	var (
		dbPath  = flag.String("db", defaultDB(), "path to mnemo.db (opened read-only)")
		nSess   = flag.Int("sessions", 3, "how many gold sessions to replay")
		models  = flag.String("models", "", "comma-separated models; empty means claudia's default")
		drips   = flag.String("drips", "8,16", "comma-separated drip sizes")
		ks      = flag.String("k", "2,4", "comma-separated seal-lookahead values")
		dryRun  = flag.Bool("dry-run", false, "use a deterministic fake summariser: exercises the harness with no model calls or spend")
		timeout = flag.Duration("timeout", 45*time.Minute, "overall budget")
		workDir = flag.String("workdir", "", "summariser working directory (default: a temp dir)")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	// A sweep is long and costs real model calls; Ctrl-C must stop it
	// promptly rather than leave agents running.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\ninterrupted; stopping")
		cancel()
		// Cancelling is a request, not a guarantee: a replay blocked in a
		// summariser round-trip will not notice for a while, and a sweep
		// that ignores Ctrl-C is worse than one that exits abruptly.
		select {
		case <-sig:
		case <-time.After(10 * time.Second):
		}
		os.Exit(130)
	}()

	gold, err := loadGold(*dbPath, *nSess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load gold sessions: %v\n", err)
		os.Exit(1)
	}
	if len(gold) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions with llm-method spans found — nothing to score against")
		os.Exit(1)
	}

	grid := buildGrid(*models, *drips, *ks)
	fmt.Printf("sweep: %d points x %d sessions = %d replays\n\n", len(grid), len(gold), len(grid)*len(gold))

	wd := *workDir
	if wd == "" {
		wd, err = os.MkdirTemp("", "streamseg-sweep-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "workdir: %v\n", err)
			os.Exit(1)
		}
	}

	mk := func(p streamseg.SweepPoint) streamseg.Summariser {
		if *dryRun {
			return newFakeSummariser(p)
		}
		return streamseg.NewClaudiaSummariser(wd, p.Model)
	}

	var results []streamseg.SweepResult
	for _, p := range grid {
		for _, g := range gold {
			if ctx.Err() != nil {
				break
			}
			r := streamseg.RunPoint(ctx, p, g, mk)
			status := "ok"
			if r.Err != nil {
				status = "ERR: " + r.Err.Error()
			}
			fmt.Printf("  %-34s %-42s Pk=%.3f WD=%.3f spans=%d %s\n",
				p, short(g.SessionID), r.Pk, r.WindowDiff, r.Spans, status)
			results = append(results, r)
		}
	}

	fmt.Printf("\n%-34s %8s %8s %7s %7s %8s %s\n",
		"point", "meanPk", "meanWD", "spans", "drips", "fails", "time")
	for _, a := range streamseg.AggregateResults(results) {
		fmt.Printf("%-34s %8.3f %8.3f %7.1f %7.1f %8d %s\n",
			a.Point, a.MeanPk, a.MeanWD, a.MeanSpans, a.MeanDrips, a.Failures,
			a.TotalTime.Round(time.Second))
	}
	fmt.Println("\nLower Pk/WD is better; both are in [0,1]. A point with failures is not " +
		"comparable on its mean alone.")
}

func short(id string) string {
	if len(id) > 40 {
		return id[:40]
	}
	return id
}

func defaultDB() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mnemo", "mnemo.db")
}

// loadGold reads sessions that carry llm-method spans, plus their
// substantive messages. Read-only, and via a separate connection rather
// than store.New: opening the store would run migrations and could start
// a pre-migration backup against a database the daemon is holding.
func loadGold(dbPath string, n int) ([]streamseg.GoldSession, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT ts.session_id
		FROM topic_segments ts
		WHERE ts.method = 'llm'
		GROUP BY ts.session_id
		HAVING COUNT(*) >= 3
		   AND (SELECT COUNT(*) FROM messages m
		        WHERE m.session_id = ts.session_id AND m.is_noise = 0) BETWEEN 60 AND 400
		ORDER BY COUNT(*) DESC
		LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	var out []streamseg.GoldSession
	for _, id := range ids {
		g := streamseg.GoldSession{SessionID: id}

		mrows, err := db.Query(`
			SELECT id, role, text, timestamp FROM messages
			WHERE session_id = ? AND is_noise = 0 ORDER BY id`, id)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var m store.StreamMessage
			if err := mrows.Scan(&m.ID, &m.Role, &m.Text, &m.Timestamp); err != nil {
				mrows.Close()
				return nil, err
			}
			g.Messages = append(g.Messages, m)
		}
		mrows.Close()

		crows, err := db.Query(`
			SELECT to_msg_id FROM topic_segments
			WHERE session_id = ? AND method = 'llm' ORDER BY to_msg_id`, id)
		if err != nil {
			return nil, err
		}
		for crows.Next() {
			var c int
			if err := crows.Scan(&c); err != nil {
				crows.Close()
				return nil, err
			}
			g.GoldCuts = append(g.GoldCuts, c)
		}
		crows.Close()

		if len(g.Messages) > 0 && len(g.GoldCuts) > 0 {
			out = append(out, g)
		}
	}
	return out, nil
}

func buildGrid(models, drips, ks string) []streamseg.SweepPoint {
	var out []streamseg.SweepPoint
	for _, m := range splitOrEmpty(models) {
		for _, d := range splitInts(drips, 8) {
			for _, k := range splitInts(ks, 2) {
				out = append(out, streamseg.SweepPoint{Model: m, DripSize: d, SealLookahead: k})
			}
		}
	}
	return out
}

// splitOrEmpty yields a single empty model when none is named, so the
// default model is a point in the grid rather than an absence from it.
func splitOrEmpty(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{""}
	}
	return strings.Split(s, ",")
}

func splitInts(s string, def int) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(p, "%d", &v); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []int{def}
	}
	return out
}
