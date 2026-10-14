// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"sort"
	"strings"
)

// hitFilter is applied inside the FTS query, before LIMIT, so a flood
// of non-matching rows cannot fill the candidate window (🎯T195, 🎯T196).
type hitFilter struct {
	sessionType    string
	repo           string
	excludeSession string
	// shape selects the content table the EXISTS clause joins.
	// "" means no session filter. "vault" keeps docs of kind vault.
	shape string
}

type rankedHit struct {
	rowid   int64
	rank    float64
	fair    float64
	useFair bool
}

// relaxQueryStemmed is relaxQuery without the trailing-star stopgap.
// Porter already folds inflections. A prefix on the last token of a
// phrase would widen it past the stem ("the cat"* matches "the catalog";
// porter does not stem catalog to cat). The legacy unicode61 side keeps
// the star so plurals still match below the marker.
func relaxQueryStemmed(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	if fts5Operators.MatchString(q) {
		return q
	}
	words := strings.Fields(q)
	if len(words) <= 1 {
		return q
	}
	return strings.Join(words, " OR ")
}

// searchCorpus runs the query against the index that currently owns
// each row. During the migration that is two queries merged on a
// per-side calibrated quantile; before it starts, the legacy index;
// after it finishes, the stem index alone, with raw BM25 so a finished
// store ranks the way Search always has.
func (s *Store) searchCorpus(corpus, legacy, stem, query string, limit int, filt hitFilter) ([]rankedHit, error) {
	if limit <= 0 {
		limit = 20
	}
	bound := s.stemBoundary(corpus)
	switch bound.mode {
	case stemDone:
		if stem == "" {
			return s.matchSide(legacy, relaxQuery(query), "", 0, limit, filt, false)
		}
		return s.matchSide(stem, relaxQueryStemmed(query), "", 0, limit, filt, false)
	case stemSplit:
		return s.matchSplit(corpus, legacy, stem, query, limit, filt, bound.marker)
	default:
		return s.matchSide(legacy, relaxQuery(query), "", 0, limit, filt, false)
	}
}

func (s *Store) matchSplit(corpus, legacy, stem, query string, limit int, filt hitFilter, marker int64) ([]rankedHit, error) {
	hi, err := s.matchSide(stem, relaxQueryStemmed(query), ">=", marker, limit, filt, true)
	if err != nil {
		return nil, err
	}
	lo, err := s.matchSide(legacy, relaxQuery(query), "<", marker, limit, filt, true)
	if err != nil {
		return nil, err
	}
	cals, err := s.LoadCalibrations()
	if err != nil {
		cals = nil
	}
	terms := float64(queryTermCount(query))
	scoreSide := func(hits []rankedHit, key string) {
		cal := cals[key]
		weak := cal == nil || cal.Evidence() < fusionEvidenceFloor
		for i := range hits {
			if weak {
				hits[i].fair = neutralQuantile - float64(i)*rankTiebreak
				continue
			}
			hits[i].fair = cal.Score(-hits[i].rank/terms, i)
		}
	}
	// Position within the side is the rank argument: matchSide already
	// returns best-rank first.
	scoreSide(hi, corpus+"#stem")
	scoreSide(lo, corpus)
	merged := make([]rankedHit, 0, len(hi)+len(lo))
	merged = append(merged, hi...)
	merged = append(merged, lo...)
	// Higher fair score is better. No dedupe: a row that appears on
	// both sides is a bug the split tests are there to see.
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].fair > merged[j].fair
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// matchSide is one FTS query. cmp is "", ">=" or "<"; the rowid
// predicate and the session filter are both AND-ed before LIMIT so
// truncation happens after they have been applied.
func (s *Store) matchSide(table, matchQuery, cmp string, bound int64, limit int, filt hitFilter, useFair bool) ([]rankedHit, error) {
	if cmp != "" && cmp != ">=" && cmp != "<" {
		return nil, fmt.Errorf("bad rowid comparison %q", cmp)
	}
	var b strings.Builder
	args := []any{matchQuery}
	// FTS5 rejects a table alias on the left of MATCH ("no such column").
	// The rowid bound and the session EXISTS sit in the same WHERE,
	// ahead of LIMIT, so truncation counts only rows that already match.
	fmt.Fprintf(&b, `SELECT %s.rowid, %s.rank FROM %s WHERE %s MATCH ?`, table, table, table, table)
	if cmp != "" {
		fmt.Fprintf(&b, ` AND %s.rowid %s ?`, table, cmp)
		args = append(args, bound)
	}
	clause, extra := filterClause(table, filt)
	b.WriteString(clause)
	args = append(args, extra...)
	fmt.Fprintf(&b, ` ORDER BY %s.rank LIMIT ?`, table)
	args = append(args, limit)

	rows, err := s.readDB.Query(b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rankedHit
	for rows.Next() {
		var h rankedHit
		if err := rows.Scan(&h.rowid, &h.rank); err != nil {
			continue
		}
		h.useFair = useFair
		out = append(out, h)
	}
	return out, rows.Err()
}

func filterClause(fts string, f hitFilter) (string, []any) {
	row := fts + ".rowid"
	switch f.shape {
	case "message":
		return sessionExists("messages", "m.id = "+row, "m.session_id", f)
	case "tool_output":
		return sessionExists("tool_outputs", "t.id = "+row, "t.session_id", f)
	case "compaction":
		return sessionExists("compactions", "c.id = "+row, "c.session_id", f)
	case "segment":
		if f.excludeSession == "" {
			return "", nil
		}
		return ` AND EXISTS (SELECT 1 FROM topic_segments t WHERE t.rowid = ` + row + ` AND t.session_id != ?)`, []any{f.excludeSession}
	case "vault":
		return ` AND EXISTS (SELECT 1 FROM docs d WHERE d.id = ` + row + ` AND d.kind = 'vault')`, nil
	default:
		return "", nil
	}
}

// sessionExists builds one EXISTS that a candidate must satisfy before
// it counts toward LIMIT. An empty filter is omitted entirely so the
// unfiltered path stays a plain FTS scan.
func sessionExists(table, idPred, sessionCol string, f hitFilter) (string, []any) {
	typeOn := f.sessionType != "" && f.sessionType != "all"
	repoOn := f.repo != ""
	exclOn := f.excludeSession != ""
	if !typeOn && !repoOn && !exclOn {
		return "", nil
	}
	alias := string(table[0])
	// The first letter is not unique across tables (messages/memories).
	// Callers pass the alias inside idPred; use a fixed alias per shape
	// by taking it from idPred's left side.
	if i := strings.IndexByte(idPred, '.'); i > 0 {
		alias = idPred[:i]
	}
	var b strings.Builder
	var args []any
	fmt.Fprintf(&b, ` AND EXISTS (SELECT 1 FROM %s %s WHERE %s`, table, alias, idPred)
	if exclOn {
		fmt.Fprintf(&b, ` AND %s != ?`, sessionCol)
		args = append(args, f.excludeSession)
	}
	if typeOn {
		fmt.Fprintf(&b, ` AND EXISTS (SELECT 1 FROM session_summary ss WHERE ss.session_id = %s AND ss.session_type = ?)`, sessionCol)
		args = append(args, f.sessionType)
	}
	if repoOn {
		fmt.Fprintf(&b, ` AND EXISTS (SELECT 1 FROM session_meta sm WHERE sm.session_id = %s AND (sm.cwd LIKE ? OR sm.repo LIKE ?))`, sessionCol)
		p := "%" + f.repo + "%"
		args = append(args, p, p)
	}
	b.WriteByte(')')
	return b.String(), args
}
