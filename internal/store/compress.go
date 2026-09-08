// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Per-row text compression for the large text columns (🎯T151).
//
// messages.text (5.3 GB) and docs.content (1.3 GB) are the bulk of what
// is not already JSONB in mnemo.db. Both are stored zstd-compressed in a
// sibling nullable column (text_z / content_z) with the legacy column
// holding "" as a sentinel; a registered SQL function, mnemo_text(plain,
// z), gives every reader the plaintext regardless of which shape a row
// has. Dictionaries matter: short rows compress to ~0.39 on their own but
// to ~0.26 with a 110 KB dictionary trained on a few thousand rows, and
// the zstd frame header carries the dictionary id, so a blob is
// self-describing and a retrain never invalidates older rows.
//
// Schema policy (append-only, sqlift AllowNone) shapes the design: the
// legacy columns are never dropped, only emptied by CompressBackfill,
// and the new shape lives entirely in added columns, a new table and
// new views (messages_v, docs_v).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// Compression families: one dictionary lineage per compressed column.
const (
	FamilyMessagesText = "messages.text"
	FamilyDocsContent  = "docs.content"
	// FamilyEntriesRaw compresses the JSON line behind entries.raw
	// (🎯T152). Its sentinel is NULL, not '': the sixteen generated
	// columns evaluate raw, and '' raises "malformed JSON".
	FamilyEntriesRaw = "entries.raw"
)

// allFamilies is the display/backfill order.
var allFamilies = []string{FamilyMessagesText, FamilyDocsContent, FamilyEntriesRaw}

// textSQLFunc is the SQL function text readers go through; rawSQLFunc is
// its entries.raw twin, which passes a JSONB blob through untouched.
const (
	textSQLFunc = "mnemo_text"
	rawSQLFunc  = "mnemo_raw"
)

const (
	// compressMinBytes: rows shorter than this stay plain — a zstd frame
	// costs ~12 bytes of header and CRC, and a dictionary match cannot
	// recover that on a few-word row.
	compressMinBytes = 64

	// Dictionary training parameters. 110 KB is the zstd default target;
	// measured on the live corpus, content sampled as whole-row
	// concatenation reached 0.267 against 0.254 for `zstd --train`, so
	// no COVER pass is needed. Rows are capped when used as entropy
	// samples because BuildDict cannot take a sample over one block.
	dictTargetBytes     = 110 * 1024
	dictSampleRows      = 3000
	dictSampleRowMax    = 64 * 1024
	dictAutoTrainMinRow = 2000

	// backfillBatchRows bounds one GC transaction.
	backfillBatchRows = 2000

	// backfillCheckpointBytes is how much compressed data the auto
	// backfill writes before spending a PASSIVE WAL checkpoint (🎯T168).
	// The periodic TRUNCATE worker cannot help here: it waits for writes
	// to go quiet, and a pack over millions of rows never does.
	backfillCheckpointBytes = 64 << 20 // 64 MiB

	// backfillDutyCycle is the fraction of wall time the auto backfill
	// is allowed to spend working. The remainder is handed back to
	// foreground work (🎯T168). At 0.5 the packer takes about twice as
	// long and leaves the daemon responsive throughout; a background
	// task that finishes in an hour instead of half an hour, without
	// taking /health down, is the better trade.
	backfillDutyCycle = 0.5

	// backfillMaxPause caps a single inter-batch pause so an unusually
	// slow batch — a lock wait, a stalled disk — cannot park the packer
	// for minutes.
	backfillMaxPause = 5 * time.Second

	// decoderMaxMemory bounds a single frame's declared size; the largest
	// tool result mnemo has ingested is well under this.
	decoderMaxMemory = 512 << 20
)

// writerDriverName is the stock sqlite3 driver plus mnemo_text; the read
// pool's driver (rodriver.go) registers the same function. Exported as
// SQLiteDriverName for tools and tests that open mnemo.db themselves:
// the messages/docs triggers call mnemo_text, so a connection without it
// cannot insert into either table.
const writerDriverName = "sqlite3_mnemo_rw"

// SQLiteDriverName is the database/sql driver to open mnemo.db with.
const SQLiteDriverName = writerDriverName

// lenOrNil is the stored z_len for a frame: NULL when the row stayed plain.
func lenOrNil(z []byte) any {
	if z == nil {
		return nil
	}
	return len(z)
}

func init() {
	sql.Register(writerDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: registerTextFunc,
	})
}

// registerTextFunc installs mnemo_text on a connection. Pure: SQLite may
// cache and reorder calls.
func registerTextFunc(conn *sqlite3.SQLiteConn) error {
	if err := conn.RegisterFunc(textSQLFunc, mnemoText, true); err != nil {
		return err
	}
	return conn.RegisterFunc(rawSQLFunc, mnemoRaw, true)
}

// mnemoText is the SQL-visible decoder. A NULL/empty z means the row is
// stored plain. Arguments are untyped because go-sqlite3 rejects a NULL
// bound to a typed []byte parameter, and z is NULL on every plain row.
func mnemoText(plain any, z any) (any, error) {
	var frame []byte
	switch v := z.(type) {
	case nil:
	case []byte:
		frame = v
	case string:
		frame = []byte(v)
	default:
		return nil, fmt.Errorf("%s: z must be a BLOB, got %T", textSQLFunc, z)
	}
	if len(frame) == 0 {
		return plain, nil
	}
	out, err := dictRegistry.decode(frame)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", textSQLFunc, err)
	}
	return string(out), nil
}

// mnemoRaw decodes entries.raw: the stored JSONB blob when the row is
// plain, else the compressed JSON text. Both are valid input to every
// json_* function, which is all readers do with it.
func mnemoRaw(plain any, z any) (any, error) {
	return mnemoText(plain, z)
}

// dictRegistry is process-wide because mnemo_text is registered per
// connection with no handle back to a Store, and tests open several
// stores in one process. Dictionary ids are random uint32s, so lineages
// from different databases cannot collide in practice.
var dictRegistry = newDictRegistry()

type dictRegistryT struct {
	mu    sync.RWMutex
	dicts map[uint32][]byte
	dec   *zstd.Decoder
}

func newDictRegistry() *dictRegistryT {
	r := &dictRegistryT{dicts: map[uint32][]byte{}}
	r.rebuildLocked()
	return r
}

func (r *dictRegistryT) rebuildLocked() {
	opts := []zstd.DOption{zstd.WithDecoderMaxMemory(decoderMaxMemory)}
	for _, d := range r.dicts {
		opts = append(opts, zstd.WithDecoderDicts(d))
	}
	dec, err := zstd.NewReader(nil, opts...)
	if err != nil {
		// Only reachable with a corrupt dictionary blob; the previous
		// decoder stays in service so existing rows keep decoding.
		slog.Error("zstd decoder rebuild failed", "err", err)
		return
	}
	if r.dec != nil {
		r.dec.Close()
	}
	r.dec = dec
}

func (r *dictRegistryT) add(id uint32, dict []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.dicts[id]; ok {
		return
	}
	r.dicts[id] = dict
	r.rebuildLocked()
}

func (r *dictRegistryT) has(id uint32) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.dicts[id]
	return ok
}

func (r *dictRegistryT) decode(z []byte) ([]byte, error) {
	r.mu.RLock()
	dec := r.dec
	r.mu.RUnlock()
	out, err := dec.DecodeAll(z, nil)
	if err != nil && errors.Is(err, zstd.ErrUnknownDictionary) {
		return nil, fmt.Errorf("dictionary %d not loaded: %w", frameDictID(z), err)
	}
	return out, err
}

// frameDictID reads the Dictionary_ID from a zstd frame header, for
// diagnostics only (the decoder does its own lookup). Returns 0 when the
// frame has none or is too short.
func frameDictID(z []byte) uint32 {
	const magicLen = 4
	if len(z) < magicLen+1 {
		return 0
	}
	fhd := z[magicLen]
	singleSegment := fhd&(1<<5) != 0
	dictIDFlag := fhd & 3
	off := magicLen + 1
	if !singleSegment {
		off++ // Window_Descriptor
	}
	switch dictIDFlag {
	case 1:
		if len(z) < off+1 {
			return 0
		}
		return uint32(z[off])
	case 2:
		if len(z) < off+2 {
			return 0
		}
		return uint32(binary.LittleEndian.Uint16(z[off:]))
	case 3:
		if len(z) < off+4 {
			return 0
		}
		return binary.LittleEndian.Uint32(z[off:])
	}
	return 0
}

// textCodec is a Store's write-side state: whether the schema is ready
// for the compressed shape, and the active encoder per family.
type textCodec struct {
	ready atomic.Bool
	// entriesPackable latches once the boot-time entries materialisation
	// has completed (🎯T152); until then entries.raw is written plain.
	entriesPackable atomic.Bool

	mu     sync.RWMutex
	enc    map[string]*zstd.Encoder // family → encoder with the active dictionary
	active map[string]uint32        // family → active dictionary id (0 = none)
	plain  *zstd.Encoder            // dictionary-less encoder, shared
}

func newTextCodec() *textCodec {
	plain, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		panic("zstd encoder: " + err.Error()) // static options; cannot fail
	}
	return &textCodec{
		enc:    map[string]*zstd.Encoder{},
		active: map[string]uint32{},
		plain:  plain,
	}
}

// CompressionReady reports whether new rows are written compressed. False
// until the schema carries text_z/content_z, which on an upgrade boot is
// after the deferred migration lands (🎯T114.1).
func (s *Store) CompressionReady() bool { return s.codec.ready.Load() }

// loadDicts reads compression_dicts, registers every dictionary for
// decoding and makes the newest per family the active encoder.
func (s *Store) loadDicts() error {
	rows, err := s.readDB.Query(`
		SELECT dict_id, family, dict
		FROM compression_dicts
		ORDER BY id`)
	if err != nil {
		return missingTableIsEmpty(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var family string
		var dict []byte
		if err := rows.Scan(&id, &family, &dict); err != nil {
			return err
		}
		if err := s.codec.activate(family, uint32(id), dict); err != nil {
			return err
		}
	}
	return missingTableIsEmpty(rows.Err())
}

// missingTableIsEmpty maps "no such table" to success. A database
// predating 🎯T151 has no compression_dicts — and therefore no
// compressed rows to decode, so nothing to load is the correct outcome.
// Reporting it as a failure would leave codec.decode unavailable forever
// on the very boot that migrates the table in. database/sql surfaces
// this either from Query or from Rows.Err depending on when the pooled
// connection prepares, so both paths route through here.
func missingTableIsEmpty(err error) error {
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil
	}
	return err
}

func (c *textCodec) activate(family string, id uint32, dict []byte) error {
	dictRegistry.add(id, dict)
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderDict(dict))
	if err != nil {
		return fmt.Errorf("encoder for dictionary %d: %w", id, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.enc[family]; old != nil {
		old.Close()
	}
	c.enc[family] = enc
	c.active[family] = id
	return nil
}

func (c *textCodec) encoder(family string) *zstd.Encoder {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if enc := c.enc[family]; enc != nil {
		return enc
	}
	return c.plain
}

// ActiveDict returns the dictionary id new rows of family are written
// with (0 when dictionary-less).
func (c *textCodec) ActiveDict(family string) uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active[family]
}

// pack returns the (plain, z) pair to store for text. Exactly one of the
// two carries the content: (text, nil) when the codec is not ready, the
// row is short, or compression does not pay; ("", frame) otherwise.
func (c *textCodec) pack(family, text string) (string, []byte) {
	if !c.ready.Load() || len(text) < compressMinBytes {
		return text, nil
	}
	z := c.encoder(family).EncodeAll([]byte(text), nil)
	if len(z) >= len(text) {
		return text, nil
	}
	return "", z
}

// DictInfo describes one row of compression_dicts.
type DictInfo struct {
	DictID     uint32
	Family     string
	CreatedAt  string
	SampleRows int
	Bytes      int
	Active     bool
}

// CompressionStatus is the operator view: dictionaries, and per family
// how many rows are compressed vs plain and what the backfill has done.
type CompressionStatus struct {
	Ready    bool
	Dicts    []DictInfo
	Families []FamilyStatus
}

// FamilyStatus is one compressed column's row accounting.
//
// Byte totals come from stored plain_len / z_len, never from live
// SUM(length(blob)). Outstanding is leftover membership (z IS NULL)
// filtered by the compress threshold on the stored length.
type FamilyStatus struct {
	Family       string
	Rows         int64
	Compressed   int64
	PlainBytes   int64 // leftover plain_len (z-col NULL)
	PackedBytes  int64 // stored z_len of packed rows (0 until lengths are filled)
	Outstanding  int64 // leftover plain_len where length ≥ compressMinBytes
	LeftoverRows int64 // z IS NULL and plain_len ≥ compressMinBytes
	// LengthsPending is rows whose stored length is not yet measured
	// (🎯T173). Non-zero only after an upgrade. While it is non-zero
	// PlainBytes/PackedBytes understate, and the status output says so
	// rather than presenting a provisional number as a settled one.
	LengthsPending int64
	BackfillDone   bool
	BackfillNext   int64 // next row id the backfill will visit
	BackfillSaved  int64 // bytes saved so far by the backfill
	BackfillAt     string
	Running        bool
	LastError      string
}

// Compress worker phases (🎯T162). Reported by the health check and
// CompressWorkerStatus so an unfinished migration cannot hide.
const (
	CompressPhaseDisabled  = "disabled"
	CompressPhaseThrottled = "throttled"
	CompressPhaseRunning   = "running"
	CompressPhaseComplete  = "complete"
	CompressPhaseIdle      = "idle"
)

// CompressWorkerSnapshot is the auto-backfill worker's last decision.
type CompressWorkerSnapshot struct {
	Phase  string
	Reason string
}

// familySpec maps a family to its table and columns. Identifiers are
// constants baked into SQL text below, never caller-supplied.
type familySpec struct {
	table, plainCol, zCol string
	// readExpr yields the bytes to compress from a plain row; sentinel is
	// what the plain column holds once compressed; decodeFn reads either
	// shape back; extraSet is appended to the compressing UPDATE
	// (entries materialises its generated columns in the same statement).
	readExpr, sentinel, decodeFn, extraSet string
}

// entriesMaterialiseSet copies each generated column into its
// materialised twin. RHS values are the pre-update row, so it composes
// with raw = NULL in the same UPDATE. COALESCE keeps a value already
// written at ingest.
const entriesMaterialiseSet = `uuid_m = COALESCE(uuid_m, uuid),
	model_m = COALESCE(model_m, model),
	stop_reason_m = COALESCE(stop_reason_m, stop_reason),
	input_tokens_m = COALESCE(input_tokens_m, input_tokens),
	output_tokens_m = COALESCE(output_tokens_m, output_tokens),
	cache_read_tokens_m = COALESCE(cache_read_tokens_m, cache_read_tokens),
	cache_creation_tokens_m = COALESCE(cache_creation_tokens_m, cache_creation_tokens),
	agent_id_m = COALESCE(agent_id_m, agent_id),
	version_m = COALESCE(version_m, version),
	slug_m = COALESCE(slug_m, slug),
	is_sidechain_m = COALESCE(is_sidechain_m, is_sidechain),
	data_type_m = COALESCE(data_type_m, data_type),
	data_command_m = COALESCE(data_command_m, data_command),
	data_hook_event_m = COALESCE(data_hook_event_m, data_hook_event),
	top_tool_use_id_m = COALESCE(top_tool_use_id_m, top_tool_use_id),
	parent_tool_use_id_m = COALESCE(parent_tool_use_id_m, parent_tool_use_id),
	message_id_m = COALESCE(message_id_m, json_extract(raw, '$.message.id')),
	request_id_m = COALESCE(request_id_m, raw->>'$.requestId'),
	cache_write_5m_m = COALESCE(cache_write_5m_m, json_extract(raw, '$.message.usage.cache_creation.ephemeral_5m_input_tokens')),
	cache_write_1h_m = COALESCE(cache_write_1h_m, json_extract(raw, '$.message.usage.cache_creation.ephemeral_1h_input_tokens')),
	source_tool_assistant_uuid_m = COALESCE(source_tool_assistant_uuid_m, raw->>'$.sourceToolAssistantUUID'),
	attribution_skill_m = COALESCE(attribution_skill_m, raw->>'$.attributionSkill'),
	attribution_agent_m = COALESCE(attribution_agent_m, raw->>'$.attributionAgent'),
	spawn_agent_id_m = COALESCE(spawn_agent_id_m, raw->>'$.toolUseResult.agentId'),
	spawn_tool_use_id_m = COALESCE(spawn_tool_use_id_m, raw->>'$.message.content[0].tool_use_id')`

var familySpecs = map[string]familySpec{
	FamilyMessagesText: {table: "messages", plainCol: "text", zCol: "text_z",
		readExpr: "text", sentinel: "''", decodeFn: textSQLFunc},
	FamilyDocsContent: {table: "docs", plainCol: "content", zCol: "content_z",
		readExpr: "content", sentinel: "''", decodeFn: textSQLFunc},
	FamilyEntriesRaw: {table: "entries", plainCol: "raw", zCol: "raw_z",
		readExpr: "json(raw)", sentinel: "NULL", decodeFn: rawSQLFunc, extraSet: entriesMaterialiseSet},
}

func familyOf(name string) (familySpec, error) {
	fs, ok := familySpecs[name]
	if !ok {
		return familySpec{}, fmt.Errorf("unknown compression family %q (want %s, %s or %s)",
			name, FamilyMessagesText, FamilyDocsContent, FamilyEntriesRaw)
	}
	return fs, nil
}

// CompressionStatus reports dictionaries and per-family accounting.
func (s *Store) CompressionStatus() (CompressionStatus, error) {
	st := CompressionStatus{Ready: s.CompressionReady()}
	rows, err := s.readDB.Query(`
		SELECT dict_id, family, created_at, sample_rows, length(dict)
		FROM compression_dicts
		ORDER BY id`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var d DictInfo
		var id int64
		if err := rows.Scan(&id, &d.Family, &d.CreatedAt, &d.SampleRows, &d.Bytes); err != nil {
			rows.Close()
			return st, err
		}
		d.DictID = uint32(id)
		d.Active = s.codec.ActiveDict(d.Family) == d.DictID
		st.Dicts = append(st.Dicts, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	for _, family := range allFamilies {
		fs := familySpecs[family]
		var f FamilyStatus
		f.Family = family
		// Membership + stored lengths. No length(blob): that is the
		// overflow-page scan that made compress.backfill ~22s. And no
		// measurement either — filling lengths is the background
		// worker's job, never a reader's (🎯T173).
		q := fmt.Sprintf(`
			SELECT
				(SELECT COUNT(*) FROM %[1]s),
				(SELECT COUNT(*) FROM %[1]s WHERE %[2]s IS NOT NULL),
				(SELECT COALESCE(SUM(plain_len), 0) FROM %[1]s WHERE %[2]s IS NULL),
				(SELECT COALESCE(SUM(z_len), 0) FROM %[1]s WHERE z_len IS NOT NULL)`,
			fs.table, fs.zCol)
		if err := s.readDB.QueryRow(q).Scan(&f.Rows, &f.Compressed, &f.PlainBytes, &f.PackedBytes); err != nil {
			return st, err
		}
		var done int
		err := s.readDB.QueryRow(`
			SELECT done, next_id, saved_bytes, updated_at, COALESCE(last_error, '')
			FROM compression_gc
			WHERE family = ?`, family).Scan(&done, &f.BackfillNext, &f.BackfillSaved, &f.BackfillAt, &f.LastError)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return st, err
		}
		f.BackfillDone = done == 1
		f.Running = s.backfill.running(family)
		rows, bytes, err := s.familyLeftover(fs)
		if err != nil {
			return st, err
		}
		f.LeftoverRows = rows
		f.Outstanding = bytes
		if f.LengthsPending, err = s.familyLengthsPending(fs); err != nil {
			return st, err
		}
		st.Families = append(st.Families, f)
	}
	return st, nil
}

// familyLeftover is ACTIONABLE leftover: z IS NULL and the stored length
// is known to meet the compress threshold. Byte total is SUM(plain_len),
// never length(blob).
//
// Rows whose length is not yet measured are deliberately excluded (🎯T173).
// Counting them as leftover would make the auto-backfill reopen a finished
// family on every cycle, walk it, compress nothing, and mark it done again
// — which is precisely the ~22s-every-2-minutes loop this work exists to
// remove. They are reported separately by familyLengthsPending, so the
// backlog is disclosed rather than hidden or acted upon.
func (s *Store) familyLeftover(fs familySpec) (rows, bytes int64, err error) {
	q := fmt.Sprintf(`
		SELECT COUNT(*), COALESCE(SUM(plain_len), 0)
		FROM %[1]s
		WHERE %[2]s IS NULL AND plain_len >= ?`,
		fs.table, fs.zCol)
	err = s.readDB.QueryRow(q, compressMinBytes).Scan(&rows, &bytes)
	return rows, bytes, err
}

// familyOutstanding is leftover plain bytes (length ≥ compressMinBytes).
// Kept as a bytes figure because doctor/compress_status format it as IEC.
func (s *Store) familyOutstanding(fs familySpec) (int64, error) {
	_, bytes, err := s.familyLeftover(fs)
	return bytes, err
}

// familyLeftoverCount is the membership probe the auto-backfill uses
// instead of SUM(length(blob)). Empty (or only-short) leftover is an
// index-only walk of idx_*_z_null.
func (s *Store) familyLeftoverCount(fs familySpec) (int64, error) {
	rows, _, err := s.familyLeftover(fs)
	return rows, err
}

// familyLengthsPending counts rows whose stored length is still unknown:
// unpacked rows with no plain_len, and packed rows with no z_len. Both
// arise only on an upgrade — every insert and every pack writes its own
// lengths — and both are filled in bounded batches by the background
// worker (fillLengths).
//
// It is a disclosure, not an alarm. While it is non-zero the family's
// PlainBytes/PackedBytes are understated, and saying so is the difference
// between a provisional number and a wrong one.
func (s *Store) familyLengthsPending(fs familySpec) (int64, error) {
	q := fmt.Sprintf(`
		SELECT (SELECT COUNT(*) FROM %[1]s WHERE %[2]s IS NULL AND plain_len IS NULL)
		     + (SELECT COUNT(*) FROM %[1]s WHERE %[2]s IS NOT NULL AND z_len IS NULL)`,
		fs.table, fs.zCol)
	var n int64
	err := s.readDB.QueryRow(q).Scan(&n)
	return n, err
}

// lengthFillBatch bounds one fillLengths pass per family per cycle. Each
// row read pulls the payload (and its overflow pages) once, so this is
// the one place that still pays that cost — deliberately, in the
// background, at a bounded rate, and exactly once per row for the life of
// the database.
var lengthFillBatch int64 = 5000

// lengthFillBatchForTest sets the batch size and returns the previous
// value, so a test can prove the pass is bounded. Tests only.
func lengthFillBatchForTest(n int64) int64 {
	prev := lengthFillBatch
	lengthFillBatch = n
	return prev
}

// fillLengths measures a bounded batch of rows whose stored lengths are
// unknown (🎯T173).
//
// This used to run as one unbounded UPDATE on the /health path, from
// CompressionStatus, which is a Fast-tier diag check: on an upgraded
// store that rewrote every unpacked row in a single transaction while
// every other writer queued behind SQLite's single writer. That is the
// same starvation the compression backfill was taught to avoid in 🎯T168,
// reintroduced in the health handler — so measurement now belongs to the
// background worker, and never to a reader.
//
// plain_len and z_len are BYTES OF THE PAYLOAD THE PACKER COMPRESSES —
// fs.readExpr — for every writer: the ingest INSERT, the pack UPDATE, and
// this pass. Getting that wrong is silent: SQLite's length() counts
// CHARACTERS on TEXT and BYTES on BLOB, and entries.raw is stored as
// jsonb, whose byte length is a third quantity again. CAST(... AS BLOB)
// pins all three to the same unit as Go's len().
func (s *Store) fillLengths(ctx context.Context, fs familySpec) (int64, error) {
	var filled int64

	plainQ := fmt.Sprintf(`
		UPDATE %[1]s SET plain_len = length(CAST(%[2]s AS BLOB))
		WHERE id IN (
			SELECT id FROM %[1]s WHERE %[3]s IS NULL AND plain_len IS NULL LIMIT ?)`,
		fs.table, fs.readExpr, fs.zCol)
	r, err := s.writeDB.ExecContext(ctx, plainQ, lengthFillBatch)
	if err != nil {
		return filled, fmt.Errorf("%s: fill plain_len: %w", fs.table, err)
	}
	n, _ := r.RowsAffected()
	filled += n

	zQ := fmt.Sprintf(`
		UPDATE %[1]s SET z_len = length(%[2]s)
		WHERE id IN (
			SELECT id FROM %[1]s WHERE %[2]s IS NOT NULL AND z_len IS NULL LIMIT ?)`,
		fs.table, fs.zCol)
	if r, err = s.writeDB.ExecContext(ctx, zQ, lengthFillBatch); err != nil {
		return filled, fmt.Errorf("%s: fill z_len: %w", fs.table, err)
	}
	n, _ = r.RowsAffected()
	filled += n
	return filled, nil
}

// reopenBackfill clears the completion marker so a newly-detected
// backlog is walked from the start. saved_bytes is kept — it is a
// cumulative counter, not a claim that the file shrank.
func (s *Store) reopenBackfill(family string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.writeDB.Exec(`
		INSERT INTO compression_gc (family, next_id, done, saved_bytes, updated_at, last_error)
		VALUES (?, 0, 0, 0, ?, '')
		ON CONFLICT(family) DO UPDATE SET
			next_id = 0,
			done = 0,
			updated_at = excluded.updated_at,
			last_error = ''`, family, now)
	return err
}

// TrainDictionary samples rows of family, builds a zstd dictionary,
// persists it as a new compression_dicts row and makes it the active
// encoder. Earlier dictionaries stay registered, so rows written under
// them keep decoding. Returns the new dictionary's id.
func (s *Store) TrainDictionary(ctx context.Context, family string) (uint32, error) {
	fs, err := familyOf(family)
	if err != nil {
		return 0, err
	}
	if !s.CompressionReady() {
		return 0, errors.New("compression schema not ready (deferred upgrade still running?)")
	}
	samples, err := s.sampleRows(ctx, fs, dictSampleRows)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, fmt.Errorf("%s: no rows to train on", family)
	}

	// History is the dictionary content: whole rows concatenated up to
	// the target size. Contents are the entropy samples, capped per row.
	var history []byte
	for _, r := range samples {
		if len(history) >= dictTargetBytes {
			break
		}
		history = append(history, r...)
	}
	if len(history) > dictTargetBytes {
		history = history[:dictTargetBytes]
	}
	contents := make([][]byte, len(samples))
	for i, r := range samples {
		if len(r) > dictSampleRowMax {
			r = r[:dictSampleRowMax]
		}
		contents[i] = r
	}
	id, err := freshDictID(s.readDB)
	if err != nil {
		return 0, err
	}
	dict, err := zstd.BuildDict(zstd.BuildDictOptions{
		ID:       id,
		Contents: contents,
		History:  history,
		Level:    zstd.SpeedDefault,
	})
	if err != nil {
		return 0, fmt.Errorf("build dictionary: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.writeDB.ExecContext(ctx, `
		INSERT INTO compression_dicts (dict_id, family, created_at, sample_rows, dict)
		VALUES (?, ?, ?, ?, ?)`, int64(id), family, now, len(samples), dict); err != nil {
		return 0, err
	}
	if err := s.codec.activate(family, id, dict); err != nil {
		return 0, err
	}
	slog.Info("compression dictionary trained",
		"family", family, "dict_id", id, "rows", len(samples), "bytes", len(dict))
	return id, nil
}

// freshDictID draws a random non-zero id not already in compression_dicts.
func freshDictID(db *sql.DB) (uint32, error) {
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		id := binary.LittleEndian.Uint32(b[:])
		if id == 0 || dictRegistry.has(id) {
			continue
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM compression_dicts WHERE dict_id = ?`, int64(id)).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return id, nil
		}
	}
}

// sampleRows picks up to n rows of the family's plaintext by random id.
// Rows are read through mnemo_text so already-compressed rows count as
// samples too. Ids are drawn uniformly over [1, max]; misses (deleted
// ids, short rows) are skipped, so the sample is whatever landed.
func (s *Store) sampleRows(ctx context.Context, fs familySpec, n int) ([][]byte, error) {
	var maxID int64
	q := fmt.Sprintf(`SELECT COALESCE(MAX(id), 0) FROM %s`, fs.table)
	if err := s.readDB.QueryRowContext(ctx, q).Scan(&maxID); err != nil {
		return nil, err
	}
	if maxID == 0 {
		return nil, nil
	}
	// json() normalises entries.raw to text whether the row holds JSONB
	// or a decoded frame, so the dictionary is trained on what is stored.
	sel := fmt.Sprintf(`SELECT %s(%s, %s) FROM %s WHERE id = ?`,
		fs.decodeFn, fs.plainCol, fs.zCol, fs.table)
	if family := fs; family.table == "entries" {
		sel = fmt.Sprintf(`SELECT json(%s(%s, %s)) FROM %s WHERE id = ?`,
			fs.decodeFn, fs.plainCol, fs.zCol, fs.table)
	}
	stmt, err := s.readDB.PrepareContext(ctx, sel)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	var out [][]byte
	attempts := n * 4
	for i := 0; i < attempts && len(out) < n; i++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		id := int64(binary.LittleEndian.Uint64(b[:])%uint64(maxID)) + 1
		var text string
		err := stmt.QueryRowContext(ctx, id).Scan(&text)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(text) < compressMinBytes {
			continue
		}
		out = append(out, []byte(text))
	}
	return out, nil
}

// backfillState tracks in-flight CompressBackfill runs per family so the
// op can be started from MCP and observed while it runs. The auto
// worker (🎯T162) also parks its last phase here for the health check.
type backfillState struct {
	mu            sync.Mutex
	active        map[string]bool
	lastErr       map[string]string
	yield         time.Duration
	phase         string
	reason        string
	started       bool
	forceDisabled bool
}

func (b *backfillState) setPhase(phase, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.phase = phase
	b.reason = reason
}

func (b *backfillState) snapshot() CompressWorkerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	phase := b.phase
	if phase == "" {
		phase = CompressPhaseIdle
	}
	return CompressWorkerSnapshot{Phase: phase, Reason: b.reason}
}

func (b *backfillState) setYield(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.yield = d
}

func (b *backfillState) getYield() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.yield
}

func (b *backfillState) running(family string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.active[family]
}

func (b *backfillState) start(family string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == nil {
		b.active = map[string]bool{}
		b.lastErr = map[string]string{}
	}
	if b.active[family] {
		return false
	}
	b.active[family] = true
	return true
}

func (b *backfillState) finish(family string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active[family] = false
	if err != nil {
		b.lastErr[family] = err.Error()
	} else {
		b.lastErr[family] = ""
	}
}

// ErrBackfillRunning is returned when CompressBackfill is already in
// flight for that family — the auto worker and the MCP op share a lock.
var ErrBackfillRunning = errors.New("backfill already running")

// BackfillResult summarises one CompressBackfill run.
type BackfillResult struct {
	Family     string
	Rows       int64 // rows visited this run
	Compressed int64 // rows rewritten compressed this run
	Saved      int64 // bytes saved this run
	Skipped    int64 // rows left plain because the row itself is unwritable (🎯T169)
	Done       bool  // cursor reached the end of the table

	// skipErr is the most recent per-row write failure, recorded against
	// the cursor once the pass ends. It cannot be written as it happens:
	// every batch's saveBackfillCursor clears last_error, so a note
	// written mid-pass is erased by the next batch.
	skipErr error
}

// CompressBackfill is the phase-3 GC: it walks family's table in id
// order from the persisted cursor, compresses each still-plain row that
// pays, verifies the frame decodes back to the original bytes, and only
// then empties the legacy column. Idempotent and resumable — the cursor
// in compression_gc advances per batch, so a killed daemon resumes where
// it stopped. Cancelling ctx stops between batches.
func (s *Store) CompressBackfill(ctx context.Context, family string) (BackfillResult, error) {
	fs, err := familyOf(family)
	if err != nil {
		return BackfillResult{}, err
	}
	if !s.CompressionReady() {
		return BackfillResult{}, errors.New("compression schema not ready (deferred upgrade still running?)")
	}
	if family == FamilyEntriesRaw {
		ok, err := s.EntriesMaterialised()
		if err != nil {
			return BackfillResult{}, err
		}
		if !ok {
			return BackfillResult{}, errors.New("entries.raw: the boot-time field materialisation has not finished; retry later (op=compress_status shows entries.fields)")
		}
	}
	if !s.backfill.start(family) {
		return BackfillResult{}, fmt.Errorf("%s: %w", family, ErrBackfillRunning)
	}
	res := BackfillResult{Family: family}
	err = s.compressBackfill(ctx, fs, family, &res)
	s.backfill.finish(family, err)
	if res.Skipped > 0 {
		// Durable, so op=compress_status can say why a family that
		// reports done still carries a plain residue. Written after the
		// pass because each batch's cursor save clears last_error
		// (🎯T169).
		s.noteBackfillSkip(family, res.Skipped, res.skipErr)
	}
	return res, err
}

func (s *Store) compressBackfill(ctx context.Context, fs familySpec, family string, res *BackfillResult) error {
	var next int64
	err := s.readDB.QueryRow(`SELECT next_id FROM compression_gc WHERE family = ?`, family).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Identifiers come from familySpecs; the values are bound.
	selectSQL := fmt.Sprintf(`
		SELECT id, COALESCE(%[2]s, '')
		FROM %[1]s
		WHERE id >= ? AND %[3]s IS NULL
		ORDER BY id
		LIMIT ?`, fs.table, fs.readExpr, fs.zCol)
	extra := ""
	if fs.extraSet != "" {
		extra = ", " + fs.extraSet
	}
	updateSQL := fmt.Sprintf(`UPDATE %[1]s SET %[2]s = %[4]s, %[3]s = ?, plain_len = ?, z_len = ?%[5]s WHERE id = ?`,
		fs.table, fs.plainCol, fs.zCol, fs.sentinel, extra)
	maxSQL := fmt.Sprintf(`SELECT COALESCE(MAX(id), 0) FROM %s`, fs.table)
	enc := s.codec.encoder(family)
	var walDirty int64 // compressed bytes written since the last checkpoint

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		type row struct {
			id   int64
			text string
		}
		var batch []row
		rows, err := s.readDB.QueryContext(ctx, selectSQL, next, backfillBatchRows)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.text); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if len(batch) == 0 {
			// Nothing plain at or past the cursor: done. Record max id + 1
			// so a resume after new (already-compressed) inserts is a no-op.
			var maxID int64
			if err := s.readDB.QueryRowContext(ctx, maxSQL).Scan(&maxID); err != nil {
				return err
			}
			res.Done = true
			return s.saveBackfillCursor(family, maxID+1, 0, true)
		}

		// Encode and verify OUTSIDE the write transaction (🎯T168).
		// zstd on 2,000 rows — entries.raw rows especially — is the bulk
		// of a batch's wall time, and doing it between BeginTx and Commit
		// held SQLite's single writer for all of it. Every foreground
		// write (ingest, compaction, stream reconcile) queued behind that
		// lock, which is how a background packer took /health down with
		// it. The transaction below now contains only the UPDATEs.
		type packed struct {
			id       int64
			z        []byte
			saved    int64
			plainLen int
		}
		var (
			toWrite   []packed
			saved     int64
			zBytes    int64
			encodeDur time.Duration
		)
		encodeStart := time.Now()
		for _, r := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(r.text) < compressMinBytes {
				continue
			}
			z := enc.EncodeAll([]byte(r.text), nil)
			if len(z) >= len(r.text) {
				continue
			}
			back, err := dictRegistry.decode(z)
			if err != nil || string(back) != r.text {
				return fmt.Errorf("%s id %d: round-trip mismatch (%v); backfill halted", fs.table, r.id, err)
			}
			d := int64(len(r.text) - len(z))
			toWrite = append(toWrite, packed{id: r.id, z: z, saved: d, plainLen: len(r.text)})
			saved += d
			zBytes += int64(len(z))
		}
		encodeDur = time.Since(encodeStart)

		writeStart := time.Now()
		var skipped int64
		var lastSkipErr error
		if len(toWrite) > 0 {
			tx, err := s.writeDB.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			stmt, err := tx.PrepareContext(ctx, updateSQL)
			if err != nil {
				tx.Rollback()
				return err
			}
			for _, w := range toWrite {
				if _, err := stmt.ExecContext(ctx, w.z, w.plainLen, len(w.z), w.id); err != nil {
					// One unwritable row must not strand the family
					// (🎯T169). entries.raw carries materialised twins in
					// extraSet, and writing them can collide with the
					// unique index when a duplicate row was ingested while
					// the twin was still NULL. Aborting the pass on that
					// row meant the same poison batch was retried every
					// two minutes forever, so the remaining millions of
					// rows were never packed and the backfill never
					// reported done — which is what gates VACUUM.
					//
					// SQLite's default conflict action is ABORT, which
					// rolls back the failing statement and leaves the
					// transaction usable, so the rest of the batch still
					// commits. The row stays plain and is reported.
					if isRowLevelWriteError(err) {
						slog.Warn("compress backfill skipped an unwritable row",
							"table", fs.table, "id", w.id, "err", err)
						skipped++
						lastSkipErr = err
						continue
					}
					stmt.Close()
					tx.Rollback()
					return err
				}
			}
			stmt.Close()
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		writeDur := time.Since(writeStart)

		res.Rows += int64(len(batch))
		res.Compressed += int64(len(toWrite)) - skipped
		res.Saved += saved
		res.Skipped += skipped
		if lastSkipErr != nil {
			res.skipErr = lastSkipErr
		}
		next = batch[len(batch)-1].id + 1
		if err := s.saveBackfillCursor(family, next, saved, false); err != nil {
			return err
		}

		// Keep the WAL bounded during the run (🎯T168). The maintenance
		// worker defers while writes are active, and a multi-hour pack is
		// never quiet, so on 0.94.0 the WAL climbed from 64 MiB to 1,215
		// MiB before anything reclaimed it. A PASSIVE checkpoint blocks
		// neither readers nor writers: it copies what it can and returns.
		if s.backfill.getYield() > 0 {
			walDirty += zBytes
			if walDirty >= backfillCheckpointBytes {
				walDirty = 0
				s.checkpointPassive("compress-backfill")
			}
		}

		if err := s.pauseAfterBatch(ctx, encodeDur+writeDur); err != nil {
			return err
		}
	}
}

// saveBackfillCursor upserts compression_gc. savedDelta is the bytes
// saved by the batch just committed, added to the persisted total.
func (s *Store) saveBackfillCursor(family string, next int64, savedDelta int64, done bool) error {
	now := time.Now().UTC().Format(time.RFC3339)
	d := 0
	if done {
		d = 1
	}
	_, err := s.writeDB.Exec(`
		INSERT INTO compression_gc (family, next_id, done, saved_bytes, updated_at, last_error)
		VALUES (?, ?, ?, ?, ?, '')
		ON CONFLICT(family) DO UPDATE SET
			next_id = excluded.next_id,
			done = excluded.done,
			saved_bytes = compression_gc.saved_bytes + ?,
			updated_at = excluded.updated_at,
			last_error = ''`,
		family, next, d, savedDelta, now, savedDelta)
	return err
}

// autoTrainDictionaries trains a first dictionary for any family that has
// none and enough rows to learn from. Runs once compression is ready;
// a fresh install writes dictionary-less frames until it crosses the
// threshold, then retrains nothing — later retrains are an explicit op.
func (s *Store) autoTrainDictionaries(ctx context.Context) {
	for _, family := range allFamilies {
		if ctx.Err() != nil {
			return
		}
		if s.codec.ActiveDict(family) != 0 {
			continue
		}
		fs := familySpecs[family]
		var n int64
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, fs.table)
		if err := s.readDB.QueryRowContext(ctx, q).Scan(&n); err != nil {
			if ctx.Err() == nil {
				slog.Warn("compression auto-train count failed", "family", family, "err", err)
			}
			return
		}
		if n < dictAutoTrainMinRow {
			continue
		}
		if _, err := s.TrainDictionary(ctx, family); err != nil {
			slog.Warn("compression auto-train failed", "family", family, "err", err)
		}
	}
}

// entriesFieldsFamily is the compression_gc cursor for the boot-time
// pass that copies entries' generated columns into their *_m twins
// (🎯T152). It is not a compression family: nothing is encoded, and it
// runs automatically because readers (entries_v) source these columns
// exclusively — until it completes, rows it has not reached read NULL
// there. Compressing entries.raw is refused until it reports done, so
// INSERT OR IGNORE keeps its unique (session_id, uuid) key on every row
// throughout.
const entriesFieldsFamily = "entries.fields"

// entriesRawPackable is the cached answer to EntriesMaterialised for the
// hot write path; it flips once and never back.
func (s *Store) entriesRawPackable() bool {
	if s.codec.entriesPackable.Load() {
		return true
	}
	ok, err := s.EntriesMaterialised()
	if err == nil && ok {
		s.codec.entriesPackable.Store(true)
	}
	return ok
}

// EntriesMaterialised reports whether every pre-🎯T152 entries row has
// its *_m columns populated.
func (s *Store) EntriesMaterialised() (bool, error) {
	var done int
	err := s.readDB.QueryRow(`SELECT done FROM compression_gc WHERE family = ?`, entriesFieldsFamily).Scan(&done)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return done == 1, err
}

// MaterialiseEntries runs the boot-time pass in id order from the
// persisted cursor, batch by batch. Idempotent and resumable.
func (s *Store) MaterialiseEntries(ctx context.Context) (BackfillResult, error) {
	res := BackfillResult{Family: entriesFieldsFamily}
	if !s.CompressionReady() {
		return res, errors.New("compression schema not ready (deferred upgrade still running?)")
	}
	if !s.backfill.start(entriesFieldsFamily) {
		return res, fmt.Errorf("%s: already running", entriesFieldsFamily)
	}
	err := s.materialiseEntries(ctx, &res)
	s.backfill.finish(entriesFieldsFamily, err)
	return res, err
}

func (s *Store) materialiseEntries(ctx context.Context, res *BackfillResult) error {
	var next int64
	var done int
	err := s.readDB.QueryRow(`SELECT next_id, done FROM compression_gc WHERE family = ?`, entriesFieldsFamily).Scan(&next, &done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == 1 {
		res.Done = true
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var lo, hi sql.NullInt64
		var count int64
		err := s.readDB.QueryRowContext(ctx, `
			SELECT MIN(id), MAX(id), COUNT(*) FROM (
				SELECT id FROM entries WHERE id >= ? ORDER BY id LIMIT ?)`,
			next, backfillBatchRows).Scan(&lo, &hi, &count)
		if err != nil {
			return err
		}
		if !hi.Valid {
			res.Done = true
			return s.saveBackfillCursor(entriesFieldsFamily, next, 0, true)
		}
		// Only rows still carrying a plain raw have anything to copy; a
		// compressed row (raw NULL) was materialised when it was compressed.
		r, err := s.writeDB.ExecContext(ctx, `
			UPDATE entries SET `+entriesMaterialiseSet+`
			WHERE id BETWEEN ? AND ? AND raw IS NOT NULL AND uuid_m IS NULL`, lo.Int64, hi.Int64)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		res.Rows += count
		res.Compressed += n
		next = hi.Int64 + 1
		if err := s.saveBackfillCursor(entriesFieldsFamily, next, 0, false); err != nil {
			return err
		}
	}
}

// entriesUsageFieldsFamily is the boot-time pass that fills the Usage /
// AgentTrees *_m columns on historical rows so token aggregates never
// decode raw. New ingest writes these at INSERT; this walk covers rows
// ingested before the columns existed (including already-packed ones).
const entriesUsageFieldsFamily = "entries.usage_fields"

// MaterialiseUsageFields copies message_id / request_id / cache-tier /
// agent-tree fields out of raw (or raw_z) into their *_m twins.
func (s *Store) MaterialiseUsageFields(ctx context.Context) (BackfillResult, error) {
	res := BackfillResult{Family: entriesUsageFieldsFamily}
	if !s.CompressionReady() {
		return res, errors.New("compression schema not ready (deferred upgrade still running?)")
	}
	if !s.backfill.start(entriesUsageFieldsFamily) {
		return res, fmt.Errorf("%s: already running", entriesUsageFieldsFamily)
	}
	err := s.materialiseUsageFields(ctx, &res)
	s.backfill.finish(entriesUsageFieldsFamily, err)
	return res, err
}

func (s *Store) materialiseUsageFields(ctx context.Context, res *BackfillResult) error {
	var next int64
	var done int
	err := s.readDB.QueryRow(`SELECT next_id, done FROM compression_gc WHERE family = ?`, entriesUsageFieldsFamily).Scan(&next, &done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == 1 {
		res.Done = true
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var lo, hi sql.NullInt64
		var count int64
		err := s.readDB.QueryRowContext(ctx, `
			SELECT MIN(id), MAX(id), COUNT(*) FROM (
				SELECT id FROM entries WHERE id >= ? ORDER BY id LIMIT ?)`,
			next, backfillBatchRows).Scan(&lo, &hi, &count)
		if err != nil {
			return err
		}
		if !hi.Valid {
			res.Done = true
			return s.saveBackfillCursor(entriesUsageFieldsFamily, next, 0, true)
		}
		r, err := s.writeDB.ExecContext(ctx, `
			UPDATE entries SET
				message_id_m = COALESCE(entries.message_id_m, json_extract(src.j, '$.message.id')),
				request_id_m = COALESCE(entries.request_id_m, src.j->>'$.requestId'),
				cache_write_5m_m = COALESCE(entries.cache_write_5m_m, json_extract(src.j, '$.message.usage.cache_creation.ephemeral_5m_input_tokens')),
				cache_write_1h_m = COALESCE(entries.cache_write_1h_m, json_extract(src.j, '$.message.usage.cache_creation.ephemeral_1h_input_tokens')),
				source_tool_assistant_uuid_m = COALESCE(entries.source_tool_assistant_uuid_m, src.j->>'$.sourceToolAssistantUUID'),
				attribution_skill_m = COALESCE(entries.attribution_skill_m, src.j->>'$.attributionSkill'),
				attribution_agent_m = COALESCE(entries.attribution_agent_m, src.j->>'$.attributionAgent'),
				spawn_agent_id_m = COALESCE(entries.spawn_agent_id_m, src.j->>'$.toolUseResult.agentId'),
				spawn_tool_use_id_m = COALESCE(entries.spawn_tool_use_id_m, src.j->>'$.message.content[0].tool_use_id')
			FROM (
				SELECT id, mnemo_raw(raw, raw_z) AS j FROM entries
				WHERE id BETWEEN ? AND ?
			) src
			WHERE entries.id = src.id`, lo.Int64, hi.Int64)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		res.Rows += count
		res.Compressed += n
		next = hi.Int64 + 1
		if err := s.saveBackfillCursor(entriesUsageFieldsFamily, next, 0, false); err != nil {
			return err
		}
	}
}

// pauseAfterBatch hands wall time back to foreground work between
// backfill batches (🎯T168).
//
// The original fixed 10ms pause was duty-cycle blind: a batch that took
// two seconds of writer time was followed by ten milliseconds of relief,
// so the packer still held the writer 99.5% of the time and everything
// else — /health, stream reconcilers, the compactor — queued behind it.
// Pausing in proportion to the batch's own cost is what actually bounds
// the packer's share, whatever the row sizes turn out to be.
//
// A zero yield means the caller is an explicit, user-triggered
// compress_gc: that one is meant to go as fast as the disk allows, and
// the user is waiting on it.
func (s *Store) pauseAfterBatch(ctx context.Context, busy time.Duration) error {
	yield := s.backfill.getYield()
	if yield <= 0 {
		return nil
	}
	pause := time.Duration(float64(busy) * (1/backfillDutyCycle - 1))
	if pause < yield {
		pause = yield
	}
	if pause > backfillMaxPause {
		pause = backfillMaxPause
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pause):
		return nil
	}
}

// isRowLevelWriteError reports whether err is a fault of the individual
// row rather than of the database or the pass (🎯T169). Constraint
// violations qualify: the row cannot be written as-is, and no amount of
// retrying the batch changes that. Anything else — a disk error, a
// corrupt page, a cancelled context — is a reason to stop.
func isRowLevelWriteError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "constraint failed") ||
		strings.Contains(msg, "constraint violation")
}

// noteBackfillSkip records the most recent skip against the family's
// cursor row so it survives a restart and shows up in op=compress_status.
func (s *Store) noteBackfillSkip(family string, skipped int64, cause error) {
	detail := fmt.Sprintf("skipped %d unwritable row(s): %v", skipped, cause)
	if _, err := s.writeDB.Exec(
		`UPDATE compression_gc SET last_error = ? WHERE family = ?`, detail, family); err != nil {
		slog.Debug("compress backfill: could not record skip", "family", family, "err", err)
	}
}
