//go:build scanbench

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Candidate-scan cost benchmark against the LIVE index (🎯T146).
//
//	go test -tags "sqlite_fts5 scanbench" -run TestScanVariants -v ./internal/store/
//
// Build-tagged because it reads ~/.mnemo/mnemo.db. Read-only: it opens
// with mode=ro and runs SELECTs. Exists because the cost being tuned
// only appears at real scale — 35k sessions over 2.2M assistant entries.
//
// ALTERNATIVES MEASURED AND REJECTED, recorded so they are not retried:
//
//   - `WITH ... AS MATERIALIZED`: syntax error. The bundled SQLite
//     predates the hint, so CTE materialisation cannot be forced.
//   - Grouped joins (one GROUP BY scan per source, joined once instead
//     of 35k correlated lookups): 966ms, SLOWER than the 630ms hinted
//     correlated form. The grouped form must aggregate all 2.2M
//     assistant entries before joining; the correlated form does 35k
//     narrow covering-index range scans and the ratio filter prunes
//     most rows before the expensive addenda clause runs.
//   - An upper-bound short-circuit (skip sessions whose total tokens
//     are below budget): WRONG, and it silently changed the answer.
//     Session tokens sum input+output while addenda sums
//     output+cache_creation, so addenda can exceed the total.
func TestScanVariants(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	path := filepath.Join(home, ".mnemo", "mnemo.db")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no live index at %s", path)
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Skipf("open: %v", err)
	}
	defer db.Close()

	build := func(materialized, hint bool) string {
		m, h := "", ""
		if materialized {
			m = " AS MATERIALIZED"
		}
		if hint {
			h = " INDEXED BY idx_entries_assistant_tokens"
		}
		return fmt.Sprintf(`WITH session_state%s AS (
  SELECT ss.session_id,
    COALESCE(sm.compactor_internal,0) AS ci,
    COALESCE((SELECT MAX(entry_id_to) FROM compactions WHERE session_id=ss.session_id),0) AS cur,
    COALESCE((SELECT SUM(prompt_tokens+output_tokens) FROM compactions WHERE session_id=ss.session_id),0) AS ct,
    COALESCE((SELECT SUM(input_tokens+output_tokens) FROM entries%s WHERE session_id=ss.session_id AND type='assistant'),0) AS st,
    COALESCE((SELECT COUNT(*) FROM messages WHERE session_id=ss.session_id AND is_noise=0),0) AS sm2
  FROM session_summary ss LEFT JOIN session_meta sm ON sm.session_id=ss.session_id)
SELECT COUNT(*) FROM session_state s
WHERE s.ci=0
 AND (CASE WHEN s.st>0 THEN s.st ELSE s.sm2*4000 END = 0
      OR s.ct*1.0/(CASE WHEN s.st>0 THEN s.st ELSE s.sm2*4000 END) < 0.10)
 AND (CASE WHEN s.st>0 THEN COALESCE((SELECT SUM(e.output_tokens+e.cache_creation_tokens) FROM entries e
        WHERE e.session_id=s.session_id AND e.type='assistant'
          AND e.id > COALESCE((SELECT m.entry_id FROM messages m WHERE m.id=s.cur),0)),0)
      ELSE COALESCE((SELECT COUNT(*) FROM messages m WHERE m.session_id=s.session_id AND m.is_noise=0 AND m.id>s.cur),0)*4000 END) >= 50000`, m, h)
	}

	run := func(label, q string) {
		best := time.Hour
		var got int
		for i := 0; i < 3; i++ {
			start := time.Now()
			if err := db.QueryRow(q).Scan(&got); err != nil {
				t.Errorf("%s: %v", label, err)
				return
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		t.Logf("%-34s %8s   backlog=%d", label, best.Round(time.Millisecond), got)
	}

	run("baseline (production before fix)", build(false, false))
	run("INDEXED BY only", build(false, true))

	// Grouped-join form: replace 35k correlated per-session lookups with
	// one grouped scan per source, joined once. Computes each aggregate
	// exactly once regardless of how many times the outer query
	// references it, and depends on no planner hint or SQLite version.
	grouped := `WITH tok AS (
  SELECT session_id, SUM(input_tokens + output_tokens) AS st,
         SUM(output_tokens + cache_creation_tokens) AS oc
  FROM entries INDEXED BY idx_entries_assistant_usage
  WHERE type='assistant' GROUP BY session_id),
msgs AS (
  SELECT session_id, COUNT(*) AS sm2 FROM messages WHERE is_noise=0 GROUP BY session_id),
comp AS (
  SELECT session_id, MAX(entry_id_to) AS cur, SUM(prompt_tokens+output_tokens) AS ct
  FROM compactions GROUP BY session_id),
session_state AS (
  SELECT ss.session_id,
    COALESCE(sm.compactor_internal,0) AS ci,
    COALESCE(comp.cur,0) AS cur, COALESCE(comp.ct,0) AS ct,
    COALESCE(tok.st,0) AS st, COALESCE(msgs.sm2,0) AS sm2
  FROM session_summary ss
  LEFT JOIN session_meta sm ON sm.session_id=ss.session_id
  LEFT JOIN tok  ON tok.session_id  = ss.session_id
  LEFT JOIN msgs ON msgs.session_id = ss.session_id
  LEFT JOIN comp ON comp.session_id = ss.session_id)
SELECT COUNT(*) FROM session_state s
WHERE s.ci=0
 AND (CASE WHEN s.st>0 THEN s.st ELSE s.sm2*4000 END = 0
      OR s.ct*1.0/(CASE WHEN s.st>0 THEN s.st ELSE s.sm2*4000 END) < 0.10)
 AND (CASE WHEN s.st>0 THEN COALESCE((SELECT SUM(e.output_tokens+e.cache_creation_tokens) FROM entries e
        WHERE e.session_id=s.session_id AND e.type='assistant'
          AND e.id > COALESCE((SELECT m.entry_id FROM messages m WHERE m.id=s.cur),0)),0)
      ELSE COALESCE((SELECT COUNT(*) FROM messages m WHERE m.session_id=s.session_id AND m.is_noise=0 AND m.id>s.cur),0)*4000 END) >= 50000`
	run("grouped joins (no correlated CTE)", grouped)
}
