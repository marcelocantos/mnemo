// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
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
			if size == off {
				return jsonlResumeDecision{caughtUp: true}
			}
			if boundary, err := legacyJSONLBoundary(path, off); err == nil && boundary < off {
				return jsonlResumeDecision{offset: boundary}
			}
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

// A pre-prefix cursor could end inside an incomplete final line. Only
// re-read that line, and only when more bytes have arrived. A complete
// old EOF record was already indexed even if it had no newline.
func legacyJSONLBoundary(path string, offset int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	const blockSize = 4096
	var block [blockSize]byte
	for end := offset; end > 0; {
		start := max(0, end-blockSize)
		buf := block[:end-start]
		if _, err := f.ReadAt(buf, start); err != nil {
			return offset, err
		}
		boundary := int64(bytes.LastIndexByte(buf, '\n')) + 1
		if boundary > 0 || start == 0 {
			start += boundary
			if start == offset {
				return offset, nil
			}
			line, err := io.ReadAll(io.NewSectionReader(f, start, offset-start))
			if err != nil || completeJSONLObject(line) {
				return offset, err
			}
			return start, nil
		}
		end = start
	}
	return offset, nil
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
// line. An unterminated chunk is counted only if it is already a complete
// UTF-8 JSON object. A later newline will be consumed as an empty line.
//
// After the scan, a file that is no longer the one that was opened
// (different inode, a shorter size, or a changed prefix of the sampled
// length) returns errTranscriptReplaced. The caller must not commit.
func scanJSONL(path string, offset int64, handle func(line []byte, start, end int64, snapshot *jsonlSnapshot) error) (int64, *jsonlSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, nil, err
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return offset, nil, err
	}
	sampleN := prefixLen(opened.Size())
	var openedPrefix []byte
	if sampleN > 0 {
		openedPrefix = make([]byte, sampleN)
		if _, err := io.ReadFull(f, openedPrefix); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return offset, nil, errTranscriptReplaced
			}
			return offset, nil, err
		}
	}
	if opened.Size() < offset {
		return offset, nil, errTranscriptReplaced
	}
	snapshot := &jsonlSnapshot{info: opened, prefix: openedPrefix, start: offset}
	// Leave appends after the open for a later pass. This bounds the read
	// and ensures the sampled bytes cover every offset in this pass.
	reader := bufio.NewReader(io.NewSectionReader(f, offset, opened.Size()-offset))
	consumed := offset
	lines := 0
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return consumed, nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		if len(raw) == 0 {
			break
		}
		if readErr == io.EOF && !completeJSONLObject(raw) {
			break // Leave partial records at the previous boundary.
		}
		start := consumed
		consumed += int64(len(raw))
		if line := trimLineEnding(raw); len(line) > 0 {
			if err := handle(line, start, consumed, snapshot); err != nil {
				return consumed, nil, err
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
	if err := snapshot.check(path); err != nil {
		return consumed, nil, err
	}
	return consumed, snapshot, nil
}

func completeJSONLObject(raw []byte) bool {
	object := bytes.TrimLeft(raw, " \t\r")
	return len(object) > 0 && object[0] == '{' && utf8.Valid(object) && json.Valid(object)
}

type jsonlSnapshot struct {
	info   os.FileInfo
	prefix []byte
	start  int64
}

// A path can be replaced between choosing an offset and opening it. Check
// the open file against the committed generation before treating it as an
// append. A legacy cursor may deliberately start before its stored offset.
func (s *Store) checkJSONLResume(path string, snapshot *jsonlSnapshot) error {
	if snapshot.start == 0 {
		return nil
	}
	s.mu.Lock()
	off, gen := s.offsets[path], s.gens[path]
	s.mu.Unlock()
	id := fileIdentFromInfo(snapshot.info)
	if id.ino != 0 && gen.ino != 0 && (id.dev != gen.dev || id.ino != gen.ino) {
		return errTranscriptReplaced
	}
	n := prefixLen(off)
	if gen.prefix != "" && n <= len(snapshot.prefix) && gen.prefix != prefixHash(snapshot.prefix[:n]) {
		return errTranscriptReplaced
	}
	return nil
}

func (snapshot *jsonlSnapshot) check(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errTranscriptReplaced
		}
		return err
	}
	defer f.Close()
	now, err := f.Stat()
	if err != nil {
		return err
	}
	opened := snapshot.info
	if !os.SameFile(opened, now) || now.Size() < opened.Size() ||
		(now.Size() == opened.Size() && !now.ModTime().Equal(opened.ModTime())) {
		return errTranscriptReplaced
	}
	if len(snapshot.prefix) == 0 {
		return nil
	}
	cur := make([]byte, len(snapshot.prefix))
	_, err = io.ReadFull(f, cur)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return errTranscriptReplaced
		}
		return err
	}
	if !bytes.Equal(cur, snapshot.prefix) {
		return errTranscriptReplaced
	}
	return nil
}

// putIngestCursor records the resume cursor. absolute replaces the stored
// offset (a restarted read of a replacement file); otherwise the offset
// only moves forward, so a stale writer cannot walk it backwards.
// A database that has not yet grown prefix_sha256 is written without it.
func (ws *writerState) putIngestCursor(path string, offset int64, absolute bool, snapshot *jsonlSnapshot) error {
	recSize, recMtime := statFingerprint(path)
	sum := ""
	id := fileIdentOf(path)
	if snapshot != nil {
		if err := snapshot.check(path); err != nil {
			return err
		}
		sum = prefixHash(snapshot.prefix[:prefixLen(offset)])
		id = fileIdentFromInfo(snapshot.info)
		recSize = snapshot.info.Size()
		recMtime = snapshot.info.ModTime().UTC().Format(time.RFC3339Nano)
	} else if isCursorStore(path) {
		// store.db reuses this column for its size/mtime/change-counter
		// fingerprint. A JSONL prefix hash would not change on an
		// in-page UPDATE past the sampled window, and the header
		// counter alone misses an uncheckpointed WAL.
		sum = cursorStoreFingerprint(path)
	} else if n := prefixLen(offset); n > 0 {
		if b, err := prefixSample(path, n); err == nil {
			sum = prefixHash(b)
		}
	}
	abs := 0
	if absolute {
		abs = 1
	}
	_, err := ws.tx.Exec(`
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
				WHEN ? = 1 THEN excluded.prefix_sha256
				WHEN excluded.prefix_sha256 IS NULL THEN ingest_state.prefix_sha256
				WHEN excluded.offset >= ingest_state.offset THEN excluded.prefix_sha256
				ELSE ingest_state.prefix_sha256 END`,
		path, offset, recSize, recMtime, nullIfEmpty(sum), abs, abs, abs, abs)
	if err != nil && strings.Contains(err.Error(), "prefix_sha256") {
		_, err = ws.tx.Exec(`
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
	ws.cursors = append(ws.cursors, pendingIngestCursor{path, offset, absolute, ingestGen{sum, id.dev, id.ino}})
	return nil
}

type pendingIngestCursor struct {
	path     string
	offset   int64
	absolute bool
	gen      ingestGen
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
	return s.commitIngest(ws)
}
