// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Porter dual-index migration (🎯T197).
//
// Each searchable corpus keeps its unicode61 FTS table and gains a
// sibling tokenized with `porter unicode61`. New rows are written to
// both by the stem triggers from the moment the schema applies. A
// background pass copies historical rows, newest rowid first, and
// records the frontier in fts_stem_progress. Search reads the stem
// index at and above the marker and the legacy index below it, so each
// row is answered by exactly one index.
//
// FTS rebuild is not used. Compressed rows store '' in the content
// column the rebuild would read; the backfill passes mnemo_text, the
// same expression the triggers pass.
//
// Corpora stemmed: every table mnemo_search queries, plus compactions
// (searched inside Search). Not stemmed, and why:
//   - github_issues_fts, ci_runs_fts: not in mnemo_search (the tools
//     that queried them were removed; rows stay reachable via mnemo_query)
//   - snapshot_files_fts: file paths; stemming folds path segments
//   - image_ocr_fts: exact identifiers and error codes
//   - image_descriptions_fts: the image-search path, not mnemo_search
//   - todos_fts: the todo tools were removed with the index's only reader
//   - notes_fts: the notes inbox, not mnemo_search
//   - themes_fts, patterns_fts: clustering, not mnemo_search

const (
	stemDuty            = 0.5
	stemMaxPause        = 5 * time.Second
	stemMinPause        = 2 * time.Millisecond
	stemCheckpointBytes = 64 << 20
	stemMessageBatch    = 400
	stemDocBatch        = 100
	stemToolOutputBatch = 40
	stemDefaultBatch    = 500
)

// errStemPause tells the startup phase to return without failing.
// Tests use it to stop a backfill between batches; the progress row
// is already committed, so the next call resumes.
var errStemPause = errors.New("stem backfill paused")

// Test hooks. Store tests are not parallel. Production leaves them zero.
var (
	stemNoYield       bool
	stemSkipCalibrate bool
	stemBatchOverride int
	stemAfterBatch    func(corpus string, marker, rowsDone int64) error
)

type stemMode int

const (
	stemLegacy stemMode = iota
	stemSplit
	stemDone
)

type stemBound struct {
	mode   stemMode
	marker int64
}

// StemCorpusStatus is one corpus's migration frontier.
type StemCorpusStatus struct {
	Corpus   string
	Status   string
	Marker   int64
	Initial  int64
	RowsDone int64
}

// StemSnapshot is the migration's published state. /health reads this
// and does no SQL.
type StemSnapshot struct {
	Phase   string
	Corpora []StemCorpusStatus
	Updated time.Time
}

// Detail is the informational line the health check reports.
func (s StemSnapshot) Detail() string {
	if len(s.Corpora) == 0 {
		return "stem migration has not started"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "stem migration %s", s.Phase)
	for _, c := range s.Corpora {
		frac := ""
		if c.Status == "running" && c.Initial > 0 {
			pct := float64(c.Initial-c.Marker) / float64(c.Initial) * 100
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			frac = fmt.Sprintf(" %.0f%%", pct)
		}
		fmt.Fprintf(&b, "; %s %s%s (%d rows)", c.Corpus, c.Status, frac, c.RowsDone)
	}
	if s.Phase == "complete" {
		b.WriteString("; legacy indexes retained")
	}
	return b.String()
}

type stemJob struct {
	corpus    string
	stem      string
	maxSQL    string
	selectSQL string
	insertSQL string
	ncols     int
	batch     int
}

type stemRec struct {
	id    int64
	cols  []string
	bytes int64
}

// stemJobs lists every corpus that gets a porter index. The NOT EXISTS
// clause reads the FTS docsize shadow table. An external-content FTS
// table answers a bare rowid lookup from the content table, so that
// probe cannot tell an indexed row from one still below the marker.
func stemJobs() []stemJob {
	return []stemJob{
		{
			corpus: "message", stem: "messages_fts_stem", batch: stemMessageBatch, ncols: 5,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM messages`,
			selectSQL: `SELECT m.id, mnemo_text(m.text, m.text_z), COALESCE(m.role,''), COALESCE(m.project,''), COALESCE(m.session_id,'')
				FROM messages m
				WHERE m.id < ? AND m.is_noise = 0
				  AND NOT EXISTS (SELECT 1 FROM messages_fts_stem_docsize ds WHERE ds.id = m.id)
				ORDER BY m.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO messages_fts_stem(rowid, text, role, project, session_id) VALUES (?,?,?,?,?)`,
		},
		{
			corpus: "tool_output", stem: "tool_outputs_fts_stem", batch: stemToolOutputBatch, ncols: 3,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM tool_outputs`,
			selectSQL: `SELECT t.id, mnemo_text(t.text, t.text_z), COALESCE(t.session_id,'')
				FROM tool_outputs t
				WHERE t.id < ? AND t.source != 'none'
				  AND NOT EXISTS (SELECT 1 FROM tool_outputs_fts_stem_docsize ds WHERE ds.id = t.id)
				ORDER BY t.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO tool_outputs_fts_stem(rowid, text, session_id) VALUES (?,?,?)`,
		},
		{
			corpus: "segment", stem: "topic_segments_fts_stem", batch: stemDefaultBatch, ncols: 3,
			maxSQL: `SELECT COALESCE(MAX(rowid), 0) FROM topic_segments`,
			selectSQL: `SELECT t.rowid, COALESCE(t.label,''), COALESCE(t.summary,'')
				FROM topic_segments t
				WHERE t.rowid < ?
				  AND NOT EXISTS (SELECT 1 FROM topic_segments_fts_stem_docsize ds WHERE ds.id = t.rowid)
				ORDER BY t.rowid DESC LIMIT ?`,
			insertSQL: `INSERT INTO topic_segments_fts_stem(rowid, label, summary) VALUES (?,?,?)`,
		},
		{
			corpus: "decision", stem: "decisions_fts_stem", batch: stemDefaultBatch, ncols: 4,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM decisions`,
			selectSQL: `SELECT d.id, COALESCE(d.proposal_text,''), COALESCE(d.confirmation_text,''), COALESCE(d.repo,'')
				FROM decisions d
				WHERE d.id < ?
				  AND NOT EXISTS (SELECT 1 FROM decisions_fts_stem_docsize ds WHERE ds.id = d.id)
				ORDER BY d.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO decisions_fts_stem(rowid, proposal_text, confirmation_text, repo) VALUES (?,?,?,?)`,
		},
		{
			corpus: "doc", stem: "docs_fts_stem", batch: stemDocBatch, ncols: 6,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM docs`,
			selectSQL: `SELECT d.id, COALESCE(d.title,''), mnemo_text(d.content, d.content_z), COALESCE(d.repo,''), COALESCE(d.kind,''), COALESCE(d.taxonomy,'')
				FROM docs d
				WHERE d.id < ?
				  AND NOT EXISTS (SELECT 1 FROM docs_fts_stem_docsize ds WHERE ds.id = d.id)
				ORDER BY d.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO docs_fts_stem(rowid, title, content, repo, kind, taxonomy) VALUES (?,?,?,?,?,?)`,
		},
		{
			corpus: "target", stem: "targets_fts_stem", batch: stemDefaultBatch, ncols: 5,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM targets`,
			selectSQL: `SELECT t.id, COALESCE(t.name,''), COALESCE(t.description,''), COALESCE(t.raw_text,''), COALESCE(t.repo,'')
				FROM targets t
				WHERE t.id < ?
				  AND NOT EXISTS (SELECT 1 FROM targets_fts_stem_docsize ds WHERE ds.id = t.id)
				ORDER BY t.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO targets_fts_stem(rowid, name, description, raw_text, repo) VALUES (?,?,?,?,?)`,
		},
		{
			corpus: "commit", stem: "git_commits_fts_stem", batch: stemDefaultBatch, ncols: 5,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM git_commits`,
			selectSQL: `SELECT g.id, COALESCE(g.subject,''), COALESCE(g.body,''), COALESCE(g.repo,''), COALESCE(g.author_name,'')
				FROM git_commits g
				WHERE g.id < ?
				  AND NOT EXISTS (SELECT 1 FROM git_commits_fts_stem_docsize ds WHERE ds.id = g.id)
				ORDER BY g.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO git_commits_fts_stem(rowid, subject, body, repo, author_name) VALUES (?,?,?,?,?)`,
		},
		{
			corpus: "pr", stem: "github_prs_fts_stem", batch: stemDefaultBatch, ncols: 5,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM github_prs`,
			selectSQL: `SELECT g.id, COALESCE(g.title,''), COALESCE(g.body,''), COALESCE(g.repo,''), COALESCE(g.author,'')
				FROM github_prs g
				WHERE g.id < ?
				  AND NOT EXISTS (SELECT 1 FROM github_prs_fts_stem_docsize ds WHERE ds.id = g.id)
				ORDER BY g.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO github_prs_fts_stem(rowid, title, body, repo, author) VALUES (?,?,?,?,?)`,
		},
		{
			corpus: "memory", stem: "memories_fts_stem", batch: stemDefaultBatch, ncols: 5,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM memories`,
			selectSQL: `SELECT m.id, COALESCE(m.name,''), COALESCE(m.description,''), COALESCE(m.content,''), COALESCE(m.project,'')
				FROM memories m
				WHERE m.id < ?
				  AND NOT EXISTS (SELECT 1 FROM memories_fts_stem_docsize ds WHERE ds.id = m.id)
				ORDER BY m.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO memories_fts_stem(rowid, name, description, content, project) VALUES (?,?,?,?,?)`,
		},
		{
			corpus: "plan", stem: "plans_fts_stem", batch: stemDefaultBatch, ncols: 4,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM plans`,
			selectSQL: `SELECT p.id, COALESCE(p.content,''), COALESCE(p.repo,''), COALESCE(p.phase,'')
				FROM plans p
				WHERE p.id < ?
				  AND NOT EXISTS (SELECT 1 FROM plans_fts_stem_docsize ds WHERE ds.id = p.id)
				ORDER BY p.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO plans_fts_stem(rowid, content, repo, phase) VALUES (?,?,?,?)`,
		},
		{
			corpus: "config", stem: "claude_configs_fts_stem", batch: stemDefaultBatch, ncols: 3,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM claude_configs`,
			selectSQL: `SELECT c.id, COALESCE(c.content,''), COALESCE(c.repo,'')
				FROM claude_configs c
				WHERE c.id < ?
				  AND NOT EXISTS (SELECT 1 FROM claude_configs_fts_stem_docsize ds WHERE ds.id = c.id)
				ORDER BY c.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO claude_configs_fts_stem(rowid, content, repo) VALUES (?,?,?)`,
		},
		{
			corpus: "skill", stem: "skills_fts_stem", batch: stemDefaultBatch, ncols: 4,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM skills`,
			selectSQL: `SELECT s.id, COALESCE(s.name,''), COALESCE(s.description,''), COALESCE(s.content,'')
				FROM skills s
				WHERE s.id < ?
				  AND NOT EXISTS (SELECT 1 FROM skills_fts_stem_docsize ds WHERE ds.id = s.id)
				ORDER BY s.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO skills_fts_stem(rowid, name, description, content) VALUES (?,?,?,?)`,
		},
		{
			corpus: "audit", stem: "audit_entries_fts_stem", batch: stemDefaultBatch, ncols: 4,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM audit_entries`,
			selectSQL: `SELECT a.id, COALESCE(a.summary,''), COALESCE(a.raw_text,''), COALESCE(a.repo,'')
				FROM audit_entries a
				WHERE a.id < ?
				  AND NOT EXISTS (SELECT 1 FROM audit_entries_fts_stem_docsize ds WHERE ds.id = a.id)
				ORDER BY a.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO audit_entries_fts_stem(rowid, summary, raw_text, repo) VALUES (?,?,?,?)`,
		},
		{
			corpus: "compaction", stem: "compactions_fts_stem", batch: stemDefaultBatch, ncols: 3,
			maxSQL: `SELECT COALESCE(MAX(id), 0) FROM compactions`,
			selectSQL: `SELECT c.id, COALESCE(c.summary,''), COALESCE(c.session_id,'')
				FROM compactions c
				WHERE c.id < ?
				  AND NOT EXISTS (SELECT 1 FROM compactions_fts_stem_docsize ds WHERE ds.id = c.id)
				ORDER BY c.id DESC LIMIT ?`,
			insertSQL: `INSERT INTO compactions_fts_stem(rowid, summary, session_id) VALUES (?,?,?)`,
		},
	}
}

func stemFTSName(kind string) string {
	for _, j := range stemJobs() {
		if j.corpus == kind {
			return j.stem
		}
	}
	return ""
}

// stemBoundary reports which index answers a corpus. A missing progress
// table or row means the upgrade has not started the pass yet: search
// stays on the legacy index, which already holds every committed row.
func (s *Store) stemBoundary(corpus string) stemBound {
	var status string
	var marker int64
	err := s.readDB.QueryRow(
		`SELECT status, marker_rowid FROM fts_stem_progress WHERE corpus = ?`, corpus,
	).Scan(&status, &marker)
	if err != nil {
		return stemBound{mode: stemLegacy}
	}
	if status == "complete" {
		return stemBound{mode: stemDone}
	}
	return stemBound{mode: stemSplit, marker: marker}
}

// StemStatus returns the last snapshot the worker published.
func (s *Store) StemStatus() StemSnapshot {
	s.stemSnapMu.Lock()
	defer s.stemSnapMu.Unlock()
	out := s.stemSnap
	out.Corpora = append([]StemCorpusStatus(nil), s.stemSnap.Corpora...)
	return out
}

func (s *Store) publishStem() {
	rows, err := s.readDB.Query(`SELECT corpus, status, marker_rowid, initial_marker, rows_done FROM fts_stem_progress ORDER BY corpus`)
	if err != nil {
		return
	}
	defer rows.Close()
	var corpora []StemCorpusStatus
	phase := "complete"
	any := false
	for rows.Next() {
		var c StemCorpusStatus
		if err := rows.Scan(&c.Corpus, &c.Status, &c.Marker, &c.Initial, &c.RowsDone); err != nil {
			continue
		}
		any = true
		if c.Status != "complete" {
			phase = "running"
		}
		corpora = append(corpora, c)
	}
	if !any {
		phase = "pending"
	}
	s.stemSnapMu.Lock()
	s.stemSnap = StemSnapshot{Phase: phase, Corpora: corpora, Updated: time.Now()}
	s.stemSnapMu.Unlock()
}

// stemBackfill runs one resumable pass over every stemmed corpus.
// It is safe to call again: a complete corpus is skipped, and a
// running one continues from its marker.
func (s *Store) stemBackfill(ctx context.Context) error {
	s.stemMu.Lock()
	if s.stemRunning {
		s.stemMu.Unlock()
		return nil
	}
	s.stemRunning = true
	s.stemMu.Unlock()
	defer func() {
		s.stemMu.Lock()
		s.stemRunning = false
		s.stemMu.Unlock()
	}()

	var sinceCk int64
	for _, job := range stemJobs() {
		if err := s.stemOne(ctx, job, &sinceCk); err != nil {
			s.publishStem()
			return err
		}
	}
	s.publishStem()
	return nil
}

func (s *Store) stemOne(ctx context.Context, job stemJob, sinceCk *int64) error {
	marker, initial, status, err := s.ensureStemProgress(ctx, job)
	if err != nil {
		return err
	}
	if status == "complete" {
		return nil
	}
	batch := job.batch
	if stemBatchOverride > 0 {
		batch = stemBatchOverride
	}
	bucket := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := time.Now()
		recs, err := s.readStemBatch(ctx, job, marker, batch)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			if !stemSkipCalibrate {
				if err := s.calibrateStem(ctx, job.corpus, job.stem, job.corpus, true); err != nil {
					// A small corpus cannot build a distribution. The
					// index is still complete; the reconciler fills the
					// calibration later. Staying in split mode with
					// marker 0 would score every hit at the neutral prior.
					slog.Info("stem completion calibrate skipped", "corpus", job.corpus, "err", err)
				}
			}
			if err := s.markStemComplete(ctx, job.corpus); err != nil {
				return err
			}
			s.publishStem()
			return nil
		}
		minID, _, nbytes, err := s.writeStemBatch(ctx, job, recs)
		if err != nil {
			return err
		}
		marker = minID
		*sinceCk += nbytes
		if *sinceCk >= stemCheckpointBytes {
			s.checkpointPassive("stem-backfill")
			*sinceCk = 0
		}
		s.publishStem()
		if !stemSkipCalibrate && initial > 0 {
			pct := int((initial - marker) * 10 / initial)
			if pct > bucket {
				bucket = pct
				if err := s.calibrateStem(ctx, job.corpus, job.stem, job.corpus+"#stem", true); err != nil {
					slog.Debug("stem side calibrate", "corpus", job.corpus, "err", err)
				}
			}
		}
		if stemAfterBatch != nil {
			var rows int64
			_ = s.readDB.QueryRow(`SELECT rows_done FROM fts_stem_progress WHERE corpus = ?`, job.corpus).Scan(&rows)
			if err := stemAfterBatch(job.corpus, marker, rows); err != nil {
				return err
			}
		}
		if stemNoYield {
			continue
		}
		// 50% duty: pause about as long as the batch took, so ingest
		// and search get the write lock back between batches.
		pause := time.Since(start)
		if pause < stemMinPause {
			pause = stemMinPause
		}
		if pause > stemMaxPause {
			pause = stemMaxPause
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
	}
}

func (s *Store) ensureStemProgress(ctx context.Context, job stemJob) (marker, initial int64, status string, err error) {
	err = s.readDB.QueryRowContext(ctx,
		`SELECT marker_rowid, initial_marker, status FROM fts_stem_progress WHERE corpus = ?`, job.corpus,
	).Scan(&marker, &initial, &status)
	if err == nil {
		return marker, initial, status, nil
	}
	if err != sql.ErrNoRows {
		return 0, 0, "", err
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, "", err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	err = tx.QueryRowContext(ctx,
		`SELECT marker_rowid, initial_marker, status FROM fts_stem_progress WHERE corpus = ?`, job.corpus,
	).Scan(&marker, &initial, &status)
	if err == nil {
		err = tx.Commit()
		return marker, initial, status, err
	}
	if err != sql.ErrNoRows {
		return 0, 0, "", err
	}
	var maxID int64
	if err = tx.QueryRowContext(ctx, job.maxSQL).Scan(&maxID); err != nil {
		return 0, 0, "", err
	}
	status = "running"
	marker = maxID + 1
	initial = marker
	if maxID == 0 {
		// Nothing to backfill. Triggers cover every later insert.
		status = "complete"
		marker = 0
		initial = 0
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO fts_stem_progress (corpus, marker_rowid, initial_marker, status, rows_done, updated_at)
		 VALUES (?, ?, ?, ?, 0, ?)`,
		job.corpus, marker, initial, status, now); err != nil {
		return 0, 0, "", err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, "", err
	}
	return marker, initial, status, nil
}

func (s *Store) readStemBatch(ctx context.Context, job stemJob, marker int64, batch int) ([]stemRec, error) {
	rows, err := s.readDB.QueryContext(ctx, job.selectSQL, marker, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stemRec
	for rows.Next() {
		rec, err := scanStemRec(rows, job.ncols)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanStemRec(rows *sql.Rows, n int) (stemRec, error) {
	var id int64
	cols := make([]string, n-1)
	dest := make([]any, n)
	dest[0] = &id
	for i := range cols {
		dest[i+1] = &cols[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return stemRec{}, err
	}
	var nbytes int64
	for _, c := range cols {
		nbytes += int64(len(c))
	}
	return stemRec{id: id, cols: cols, bytes: nbytes}, nil
}

func (s *Store) writeStemBatch(ctx context.Context, job stemJob, recs []stemRec) (minID, inserted, nbytes int64, err error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	minID = recs[0].id
	// External-content FTS answers a bare rowid lookup from the content
	// table, which still holds the row. Membership lives in the docsize
	// shadow table: one row per indexed document.
	existSQL := `SELECT id FROM ` + job.stem + `_docsize WHERE id = ?`
	for _, rec := range recs {
		if rec.id < minID {
			minID = rec.id
		}
		var found int64
		qerr := tx.QueryRowContext(ctx, existSQL, rec.id).Scan(&found)
		if qerr == nil {
			continue
		}
		if qerr != sql.ErrNoRows {
			err = qerr
			return 0, 0, 0, err
		}
		args := make([]any, 1+len(rec.cols))
		args[0] = rec.id
		for i, c := range rec.cols {
			args[i+1] = c
		}
		if _, err = tx.ExecContext(ctx, job.insertSQL, args...); err != nil {
			return 0, 0, 0, fmt.Errorf("stem insert %s row %d: %w", job.corpus, rec.id, err)
		}
		inserted++
		nbytes += rec.bytes
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx,
		`UPDATE fts_stem_progress SET marker_rowid = ?, rows_done = rows_done + ?, updated_at = ? WHERE corpus = ?`,
		minID, inserted, now, job.corpus); err != nil {
		return 0, 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	return minID, inserted, nbytes, nil
}

func (s *Store) markStemComplete(ctx context.Context, corpus string) error {
	_, err := s.writeDB.ExecContext(ctx,
		`UPDATE fts_stem_progress SET marker_rowid = 0, status = 'complete', updated_at = ? WHERE corpus = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), corpus)
	return err
}

// calibrateStem writes a distribution probed against the stem index.
// key is the corpus kind at completion (so the normal calibration
// follows the index search will use) and kind+"#stem" while the
// migration is split.
func (s *Store) calibrateStem(ctx context.Context, corpus, fts, key string, stemmed bool) error {
	spec, ok := corpusByKind(corpus)
	if !ok {
		return nil
	}
	_, err := s.calibrateCorpusInto(ctx, spec, fts, stemmed, key, time.Now())
	return err
}
