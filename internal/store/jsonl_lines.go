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
	"hash"
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
// Rows already indexed stay. 🎯T68.6: the index only grows.
var errTranscriptReplaced = errors.New("transcript replaced while reading")
var errTranscriptTruncated = fmt.Errorf("%w: file shrank", errTranscriptReplaced)

func (s *Store) jsonlRestartClass(path string, cause error) string {
	if errors.Is(cause, errTranscriptTruncated) {
		return "truncated_at"
	}
	s.mu.Lock()
	prev := s.offsets[path]
	s.mu.Unlock()
	if info, err := os.Stat(path); err == nil && info.Size() < prev {
		return "truncated_at"
	}
	return "rewritten_at"
}

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
	// statusClass is the 🎯T68.6 tag class when the file is not the one
	// the cursor was taken from: "truncated_at" (shorter than the cursor)
	// or "rewritten_at" (rotated, or rewritten at the same size or larger).
	statusClass string
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
// A different inode, a prefix that does not match the bytes the offset
// was taken from, or a file shorter than the cursor is read from the
// start. Rows already indexed are kept (🎯T68.6); the read exists so new
// lines are not missed, and statusClass says how the source changed.
// An empty stored prefix is stamped in memory only, so a rewrite that
// lands before the first stamp is missed once — an upgrade does not
// re-read every transcript.
func (s *Store) jsonlResume(path string, size int64) jsonlResumeDecision {
	s.mu.Lock()
	off := s.offsets[path]
	gen := s.gens[path]
	s.mu.Unlock()

	shorter := size < off
	class := "rewritten_at"
	if shorter {
		class = "truncated_at"
	}
	id := fileIdentOf(path)
	if id.ino != 0 && gen.ino != 0 && (id.dev != gen.dev || id.ino != gen.ino) {
		return jsonlResumeDecision{restarted: true, statusClass: class}
	}
	if shorter {
		return jsonlResumeDecision{restarted: true, statusClass: "truncated_at"}
	}

	n := prefixLen(off)
	if n > 0 {
		sample, err := prefixSample(path, n)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return jsonlResumeDecision{restarted: true, statusClass: "truncated_at"}
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
			return jsonlResumeDecision{restarted: true, statusClass: "rewritten_at"}
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
				return offset, nil, errTranscriptTruncated
			}
			return offset, nil, err
		}
	}
	if opened.Size() < offset {
		return offset, nil, errTranscriptTruncated
	}
	snapshot := &jsonlSnapshot{info: opened, prefix: openedPrefix, start: offset, end: offset, digest: sha256.New()}
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
		snapshot.end = consumed
		_, _ = snapshot.digest.Write(raw)
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
	end    int64
	digest hash.Hash
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
	if !os.SameFile(opened, now) {
		return errTranscriptReplaced
	}
	if now.Size() < opened.Size() {
		return errTranscriptTruncated
	}
	if now.Size() == opened.Size() && !now.ModTime().Equal(opened.ModTime()) {
		return errTranscriptReplaced
	}
	if len(snapshot.prefix) == 0 {
		return nil
	}
	cur := make([]byte, len(snapshot.prefix))
	_, err = io.ReadFull(f, cur)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return errTranscriptTruncated
		}
		return err
	}
	if !bytes.Equal(cur, snapshot.prefix) {
		return errTranscriptReplaced
	}
	// Growth can be a concurrent append or a rewrite followed by an
	// append. Verify the range this scan actually consumed, including
	// bytes buffered beyond the stored prefix sample.
	if now.Size() > opened.Size() && snapshot.end > snapshot.start {
		digest := sha256.New()
		length := snapshot.end - snapshot.start
		n, err := io.Copy(digest, io.NewSectionReader(f, snapshot.start, length))
		if err != nil {
			return err
		}
		if n != length || !bytes.Equal(digest.Sum(nil), snapshot.digest.Sum(nil)) {
			return errTranscriptReplaced
		}
	}
	return nil
}

// putIngestCursor records the resume cursor. absolute replaces the stored
// offset (a restarted read of a replacement file); otherwise the offset
// only moves forward, so a stale writer cannot walk it backwards.
// A database that has not yet grown prefix_sha256 is written without it.
func (ws *writerState) putIngestCursor(path string, offset int64, absolute bool, snapshot *jsonlSnapshot, cursorStoreVersion string) error {
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
		// Store the version observed before reading the SQLite snapshot.
		sum = cursorStoreVersion
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

// ingestParsed reads one JSONL transcript from its cursor, or from the
// start when the file is no longer the one that cursor was taken from.
// A replacement observed mid-read is discarded and tried once more.
// Indexed rows are not removed.
func (s *Store) ingestParsed(path string, parse func(string, int64) (parsedFile, error)) error {
	err := s.ingestParsedOnce(path, parse, "")
	if errors.Is(err, errTranscriptReplaced) {
		return s.ingestParsedOnce(path, parse, s.jsonlRestartClass(path, err))
	}
	return err
}

func (s *Store) ingestParsedOnce(path string, parse func(string, int64) (parsedFile, error), statusClass string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var offset int64
	restarted := statusClass != ""
	if !restarted {
		d := s.jsonlResume(path, info.Size())
		if d.caughtUp {
			return nil
		}
		restarted = d.restarted
		statusClass = d.statusClass
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
		pf.sourceClass = statusClass
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

// A replay consumes occurrences from the durable index as it reads the new
// file. Matching full records keeps unrelated messages with the same text,
// and consuming each match once keeps genuinely repeated anonymous records.
type transcriptReplay struct {
	source string
	counts map[[32]byte]int
	uuids  map[string]struct{}
}

func loadTranscriptReplay(tx *sql.Tx, sessionID, source string) (*transcriptReplay, error) {
	rows, err := tx.Query(`
		SELECT json(raw), uuid FROM entries_v WHERE session_id = ? AND raw IS NOT NULL`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	r := &transcriptReplay{source, make(map[[32]byte]int), make(map[string]struct{})}
	for rows.Next() {
		var raw string
		var uuid sql.NullString
		if err := rows.Scan(&raw, &uuid); err != nil {
			return nil, err
		}
		key, _, _, err := replayRecord(raw, source)
		if err != nil {
			return nil, err
		}
		r.counts[key]++
		if uuid.Valid {
			r.uuids[uuid.String] = struct{}{}
		}
	}
	return r, rows.Err()
}

const replayUUIDPrefix = "mnemo-replay:"

// replayRecord ignores synthesized offsets. Native Claude IDs (including
// a UUID displaced by a collision) and Omp's original id remain part of
// the record. JSON numbers are decoded without a float64 round trip.
func replayRecord(raw, source string) (key [32]byte, uuid, originalUUID string, err error) {
	var record map[string]any
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&record); err != nil {
		return
	}
	originalUUID, _ = record["uuid"].(string)
	uuid = originalUUID
	if uuid == "" {
		uuid, _ = record["messageId"].(string)
	}
	switch source {
	case "cursor", "grok", "codex", "omp":
		delete(record, "uuid")
	default:
		if strings.HasPrefix(originalUUID, replayUUIDPrefix) {
			_, orig, ok := strings.Cut(strings.TrimPrefix(originalUUID, replayUUIDPrefix), ":")
			if ok {
				if orig == "" {
					delete(record, "uuid")
				} else {
					record["uuid"] = orig
				}
			}
		}
	}
	// Grok and Cursor copy model metadata from a sidecar, so a reread
	// of the same transcript can have a different stamped model.
	if source == "grok" || source == "cursor" || source == "codex" {
		dropStampedModel(record)
	}
	if source == "codex" {
		payload, _ := record["payload"].(map[string]any)
		dropStampedModel(payload)
	}
	canonical, err := json.Marshal(record)
	return sha256.Sum256(canonical), uuid, originalUUID, err
}

func dropStampedModel(record map[string]any) {
	if message, ok := record["message"].(map[string]any); ok {
		delete(message, "model")
		if len(message) == 0 {
			delete(record, "message")
		}
	}
}

// next returns whether to insert raw and, if its UUID is already in use,
// the same record under a fresh UUID. The archive retains the old row.
func (r *transcriptReplay) next(raw string) (string, bool, error) {
	key, uuid, originalUUID, err := replayRecord(raw, r.source)
	if err != nil {
		return "", false, err
	}
	if r.counts[key] > 0 {
		r.counts[key]--
		return "", false, nil
	}
	raw, err = r.allocate(raw, key, uuid, originalUUID)
	return raw, true, err
}

func (r *transcriptReplay) allocate(raw string, key [32]byte, uuid, originalUUID string) (string, error) {
	if _, occupied := r.uuids[uuid]; !occupied || uuid == "" {
		if uuid != "" {
			r.uuids[uuid] = struct{}{}
		}
		return raw, nil
	}
	for i := 1; ; i++ {
		uuid = fmt.Sprintf("%s%x-%d:%s", replayUUIDPrefix, key, i, originalUUID)
		if _, occupied := r.uuids[uuid]; !occupied {
			break
		}
	}
	r.uuids[uuid] = struct{}{}
	var record map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return "", err
	}
	id, err := json.Marshal(uuid)
	if err != nil {
		return "", err
	}
	record["uuid"] = id
	rewritten, err := json.Marshal(record)
	return string(rewritten), err
}

// An incremental read after drift can reach a byte offset used by the
// previous file. The transaction's cursor identifies an actual unread
// range so stale readers and legacy tail backtracking keep normal UUID
// deduplication. A native Omp ID is independent of its byte offset.
func appendOffsetCollision(tx *sql.Tx, pf parsedFile, raw string) (bool, error) {
	if pf.snapshot == nil || pf.source == "" || pf.source == "claude" {
		return false, nil
	}
	if pf.source == "omp" {
		var record struct{ ID string }
		if err := json.Unmarshal([]byte(raw), &record); err != nil || record.ID != "" {
			return false, err
		}
	}
	var class string
	err := tx.QueryRow(`
		SELECT sm.source_status
		FROM ingest_state i
		JOIN session_meta sm ON sm.session_id = ?
		WHERE i.path = ? AND i.offset = ?`,
		pf.sessionID, pf.path, pf.snapshot.start).Scan(&class)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	class = sourceStatusClass(class)
	return class == "truncated_at" || class == "rewritten_at", err
}
