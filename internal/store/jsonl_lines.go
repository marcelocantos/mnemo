// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// jsonlPrefixBytes is how much of a transcript is hashed into
// ingest_state.prefix_sha256. A rewrite past this window that keeps the
// prefix is not detected; the offset is only meaningful for the bytes it
// was taken from.
const jsonlPrefixBytes = 64 << 10

// errTranscriptReplaced means the file changed identity while it was being
// read (rotated, rewritten, or truncated under the scan). The caller rolls
// its transaction back and reads the file that exists now, from byte 0.
var errTranscriptReplaced = errors.New("transcript replaced while reading")

// ingestGen is the in-memory identity of an ingested transcript. prefix
// is also persisted; the inode is not, so a restart falls back to the hash.
type ingestGen struct {
	prefix string
	dev    uint64
	ino    uint64
}

type jsonlResumeDecision struct {
	offset    int64
	caughtUp  bool
	restarted bool
}

func prefixLen(n int64) int {
	if n <= 0 {
		return 0
	}
	if n > jsonlPrefixBytes {
		return jsonlPrefixBytes
	}
	return int(n)
}

func prefixSample(path string, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func prefixHash(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// jsonlResume decides how to read path, whose current size is size.
//
// A shorter file with the same inode is the durable tier (🎯T68.6): rows
// already indexed are kept and the file is not parsed again. A different
// inode, or a prefix that does not match the bytes the offset was taken
// from, is a different transcript and is read from the start. An empty
// stored prefix is stamped in memory only, so a rewrite that lands before
// the first stamp is missed once.
func (s *Store) jsonlResume(path string, size int64) jsonlResumeDecision {
	s.mu.Lock()
	off := s.offsets[path]
	gen := s.gens[path]
	s.mu.Unlock()

	id := fileIdentOf(path)
	if id.ino != 0 && gen.ino != 0 && (id.dev != gen.dev || id.ino != gen.ino) {
		return jsonlResumeDecision{restarted: true}
	}
	if size < off {
		return jsonlResumeDecision{caughtUp: true}
	}

	n := prefixLen(off)
	if n > 0 {
		sample, err := prefixSample(path, n)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return jsonlResumeDecision{restarted: true}
			}
			return jsonlResumeDecision{offset: off}
		}
		sum := prefixHash(sample)
		if gen.prefix == "" {
			s.stampGen(path, sum, id)
		} else if gen.prefix != sum {
			return jsonlResumeDecision{restarted: true}
		} else if gen.ino == 0 && id.ino != 0 {
			s.stampGen(path, sum, id)
		}
	}
	if off >= size {
		return jsonlResumeDecision{caughtUp: true}
	}
	return jsonlResumeDecision{offset: off}
}

func (s *Store) stampGen(path, prefix string, id fileIdent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gens[path]
	if prefix != "" && g.prefix == "" {
		g.prefix = prefix
	}
	if id.ino != 0 && g.ino == 0 {
		g.dev, g.ino = id.dev, id.ino
	}
	s.gens[path] = g
}

// scanJSONL reads path from offset and calls handle for each complete
// line. A chunk that does not end in '\n' is neither parsed nor counted,
// so a line still being written is read again once its newline arrives.
// handle's end argument is the byte just past that newline.
//
// After the scan, a file that is no longer the one that was opened
// (different inode, a shorter size, or a changed prefix of the sampled
// length) returns errTranscriptReplaced. The caller must not commit.
func scanJSONL(path string, offset int64, handle func(line []byte, start, end int64) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return offset, err
	}
	sampleN := prefixLen(opened.Size())
	var openedPrefix []byte
	if sampleN > 0 {
		openedPrefix = make([]byte, sampleN)
		if _, err := io.ReadFull(f, openedPrefix); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return offset, errTranscriptReplaced
			}
			return offset, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return offset, fmt.Errorf("seek %s: %w", path, err)
		}
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset, fmt.Errorf("seek %s: %w", path, err)
		}
	}

	reader := bufio.NewReader(f)
	consumed := offset
	lines := 0
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return consumed, fmt.Errorf("read %s: %w", path, readErr)
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			// EOF, or a partial tail. Leave it for the next open.
			break
		}
		start := consumed
		consumed += int64(len(raw))
		if line := trimLineEnding(raw); len(line) > 0 {
			if err := handle(line, start, consumed); err != nil {
				return consumed, err
			}
		}
		lines++
		if testAfterJSONLLine != nil {
			testAfterJSONLLine(path, lines)
		}
		if readErr == io.EOF {
			break
		}
	}
	if err := jsonlStillTheFile(path, opened, openedPrefix, consumed); err != nil {
		return consumed, err
	}
	return consumed, nil
}

func jsonlStillTheFile(path string, opened os.FileInfo, openedPrefix []byte, consumed int64) error {
	now, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errTranscriptReplaced
		}
		return err
	}
	if !os.SameFile(opened, now) || now.Size() < consumed {
		return errTranscriptReplaced
	}
	if len(openedPrefix) == 0 {
		return nil
	}
	cur, err := prefixSample(path, len(openedPrefix))
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return errTranscriptReplaced
		}
		return err
	}
	if !bytes.Equal(cur, openedPrefix) {
		return errTranscriptReplaced
	}
	return nil
}

// putIngestCursor records the resume cursor. absolute replaces the stored
// offset (a restarted read of a replacement file); otherwise the offset
// only moves forward, so a stale writer cannot walk it backwards.
// A database that has not yet grown prefix_sha256 is written without it.
func (s *Store) putIngestCursor(tx *sql.Tx, path string, offset int64, absolute bool) error {
	recSize, recMtime := statFingerprint(path)
	sum := ""
	if n := prefixLen(offset); n > 0 {
		if b, err := prefixSample(path, n); err == nil {
			sum = prefixHash(b)
		}
	}
	abs := 0
	if absolute {
		abs = 1
	}
	_, err := tx.Exec(`
		INSERT INTO ingest_state (path, offset, recorded_size, recorded_mtime, prefix_sha256)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			offset = CASE WHEN ? = 1 THEN excluded.offset
			              ELSE MAX(ingest_state.offset, excluded.offset) END,
			recorded_size = CASE
				WHEN ? = 1 OR excluded.offset >= ingest_state.offset THEN excluded.recorded_size
				ELSE ingest_state.recorded_size END,
			recorded_mtime = CASE
				WHEN ? = 1 OR excluded.offset >= ingest_state.offset THEN excluded.recorded_mtime
				ELSE ingest_state.recorded_mtime END,
			prefix_sha256 = CASE
				WHEN excluded.prefix_sha256 IS NULL THEN ingest_state.prefix_sha256
				WHEN ? = 1 OR excluded.offset >= ingest_state.offset THEN excluded.prefix_sha256
				ELSE ingest_state.prefix_sha256 END`,
		path, offset, recSize, recMtime, nullIfEmpty(sum), abs, abs, abs, abs)
	if err != nil && strings.Contains(err.Error(), "prefix_sha256") {
		_, err = tx.Exec(`
			INSERT INTO ingest_state (path, offset, recorded_size, recorded_mtime)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
				offset = CASE WHEN ? = 1 THEN excluded.offset
				              ELSE MAX(ingest_state.offset, excluded.offset) END,
				recorded_size = CASE
					WHEN ? = 1 OR excluded.offset >= ingest_state.offset THEN excluded.recorded_size
					ELSE ingest_state.recorded_size END,
				recorded_mtime = CASE
					WHEN ? = 1 OR excluded.offset >= ingest_state.offset THEN excluded.recorded_mtime
					ELSE ingest_state.recorded_mtime END`,
			path, offset, recSize, recMtime, abs, abs, abs)
	}
	if err != nil {
		return err
	}
	id := fileIdentOf(path)
	s.mu.Lock()
	if absolute || offset >= s.offsets[path] {
		s.offsets[path] = offset
		g := s.gens[path]
		if sum != "" {
			g.prefix = sum
		}
		if id.ino != 0 {
			g.dev, g.ino = id.dev, id.ino
		}
		s.gens[path] = g
	}
	s.mu.Unlock()
	return nil
}

// deleteSessionTranscript removes one session's indexed transcript so a
// replaced file can be inserted in its place. compactions and hours_*
// stay. messages_fts is external-content and has no delete trigger, so
// the delete command is issued first, matching dedupeDeleteBatch.
func (s *Store) deleteSessionTranscript(tx *sql.Tx, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	stmts := []string{
		`INSERT INTO messages_fts(messages_fts, rowid, text, role, project, session_id)
		 SELECT 'delete', m.id, m.text, m.role, m.project, m.session_id
		 FROM messages_v m WHERE m.session_id = ? AND m.is_noise = 0`,
		`INSERT INTO messages_fts_stem(messages_fts_stem, rowid, text, role, project, session_id)
		 SELECT 'delete', m.id, m.text, m.role, m.project, m.session_id
		 FROM messages_v m WHERE m.session_id = ? AND m.is_noise = 0
		   AND EXISTS (SELECT 1 FROM messages_fts_stem_docsize ds WHERE ds.id = m.id)`,
		`DELETE FROM tool_outputs WHERE session_id = ?`,
		`DELETE FROM decisions WHERE session_id = ?`,
		`DELETE FROM decision_scan_state WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM entries WHERE session_id = ?`,
		`DELETE FROM topic_segments WHERE session_id = ?`,
		`DELETE FROM segment_scan_state WHERE session_id = ?`,
		`DELETE FROM session_summary WHERE session_id = ?`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q, sessionID); err != nil {
			return fmt.Errorf("replace session %s: %w", sessionID, err)
		}
	}
	return nil
}

// ingestParsed reads one JSONL transcript from its cursor, or from the
// start when the file is no longer the one that cursor was taken from.
// A replacement observed mid-read is discarded and tried once more.
func (s *Store) ingestParsed(path string, parse func(string, int64) (parsedFile, error)) error {
	err := s.ingestParsedOnce(path, parse, false)
	if errors.Is(err, errTranscriptReplaced) {
		return s.ingestParsedOnce(path, parse, true)
	}
	return err
}

func (s *Store) ingestParsedOnce(path string, parse func(string, int64) (parsedFile, error), force bool) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var offset int64
	restarted := force
	if !force {
		d := s.jsonlResume(path, info.Size())
		if d.caughtUp {
			return nil
		}
		restarted = d.restarted
		if !restarted {
			offset = d.offset
		}
	}
	pf, err := parse(path, offset)
	if err != nil {
		return err
	}
	if restarted {
		pf.restarted = true
	}
	ws, err := s.newWriterState()
	if err != nil {
		return err
	}
	defer func() { _ = ws.tx.Rollback() }()
	defer ws.Close()
	if err := s.writeParsedFile(ws, pf); err != nil {
		return err
	}
	ws.Close()
	return ws.tx.Commit()
}
