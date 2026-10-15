// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	stemTableRe = regexp.MustCompile(`FROM (\w+)`)
	stemColsRe  = regexp.MustCompile(`\(rowid, (\w+)`)
)

// insertGenericRow inserts a row into table with every NOT NULL column
// that has no default given a type-shaped value, and col set to text.
func insertGenericRow(t *testing.T, s *Store, table, col, text string, seq int) int64 {
	t.Helper()
	rows, err := s.writeDB.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var args []any
	for rows.Next() {
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		switch {
		case name == col:
			names, args = append(names, quoteIdent(name)), append(args, text)
		case pk == 1:
		case notnull == 1 && dflt == nil:
			names = append(names, quoteIdent(name))
			if u := strings.ToUpper(typ); strings.Contains(u, "INT") || strings.Contains(u, "REAL") {
				args = append(args, seq)
			} else {
				args = append(args, fmt.Sprintf("generic-%s-%d", name, seq))
			}
		}
	}
	rows.Close()
	q := `INSERT INTO ` + table + ` (` + strings.Join(names, ",") + `) VALUES (` + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`
	res, err := s.writeDB.Exec(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TestStemUpdateBelowMarker: an UPDATE or DELETE of a row the backfill
// has not reached yet must not issue an FTS5 'delete' for a rowid the
// stem index never held. Before the fix SQLite raised "database disk
// image is malformed" and rolled the write back.
func TestStemUpdateBelowMarker(t *testing.T) {
	defer resetStemHooks()
	stemNoYield = true
	s := newTestStore(t, t.TempDir())
	type probe struct {
		job          stemJob
		table, col   string
		updID, delID int64
		token        string
	}
	var probes []probe
	for i, job := range stemJobs() {
		switch job.corpus {
		case "message", "tool_output", "compaction": // insert-only stem triggers
			continue
		}
		p := probe{job: job, table: stemTableRe.FindStringSubmatch(job.maxSQL)[1], col: stemColsRe.FindStringSubmatch(job.insertSQL)[1]}
		p.token = fmt.Sprintf("zqxrevised%c", 'a'+i)
		p.updID = insertGenericRow(t, s, p.table, p.col, "generic original words", 2*i+1)
		p.delID = insertGenericRow(t, s, p.table, p.col, "generic doomed words", 2*i+2)
		// Put the rows below a running marker: empty stem index, marker
		// above every row, as after an upgrade before the pass starts.
		mustExec(t, s, `INSERT INTO `+job.stem+`(`+job.stem+`) VALUES('delete-all')`)
		mustExec(t, s, `UPDATE fts_stem_progress SET status = 'running', marker_rowid = ?, initial_marker = ? WHERE corpus = ?`,
			p.delID+1, p.delID+1, job.corpus)
		probes = append(probes, p)
	}
	for _, p := range probes {
		if _, err := s.writeDB.Exec(`UPDATE `+p.table+` SET `+quoteIdent(p.col)+` = ? WHERE rowid = ?`, "generic "+p.token+" words", p.updID); err != nil {
			t.Fatalf("%s: update below the marker failed: %v", p.job.corpus, err)
		}
		if _, err := s.writeDB.Exec(`DELETE FROM `+p.table+` WHERE rowid = ?`, p.delID); err != nil {
			t.Fatalf("%s: delete below the marker failed: %v", p.job.corpus, err)
		}
	}
	checkIntegrity(t, s)

	if err := s.stemBackfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkIntegrity(t, s)
	for _, p := range probes {
		if n := ftsCount(t, s, p.job.stem, p.token); n != 1 {
			t.Errorf("%s: stem index holds the updated row %d times, want 1", p.job.corpus, n)
		}
		var docs int
		if err := s.readDB.QueryRow(`SELECT count(*) FROM `+p.job.stem+`_docsize WHERE id = ?`, p.delID).Scan(&docs); err != nil {
			t.Fatal(err)
		}
		if docs != 0 {
			t.Errorf("%s: deleted row is still in the stem index", p.job.corpus)
		}
		res, err := s.UnifiedSearch(p.token, []string{p.job.corpus}, 50, time.Now())
		if err != nil {
			t.Fatalf("%s: search: %v", p.job.corpus, err)
		}
		n := 0
		for _, h := range res.Hits {
			if h.ID == p.updID {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: search returned row %d %d times, want exactly once (hits %+v)", p.job.corpus, p.updID, n, res.Hits)
		}
	}
}

func checkIntegrity(t *testing.T, s *Store) {
	t.Helper()
	var ok string
	if err := s.readDB.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok != "ok" {
		t.Fatalf("integrity_check = %q", ok)
	}
	t.Log("PRAGMA integrity_check: ok")
}
