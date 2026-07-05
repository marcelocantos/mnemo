// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// imageContentBlock is the JSON representation of an image content block
// in a JSONL transcript.
type imageContentBlock struct {
	Type   string `json:"type"`
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// rawMessage wraps the message field of a JSONL entry for image extraction.
type rawMessage struct {
	Content json.RawMessage `json:"content"`
}

// rawEntry wraps a JSONL line for image extraction.
type rawEntry struct {
	Message rawMessage `json:"message"`
	Type    string     `json:"type"`
}

// colorModelName returns a string for the color model of an image config.
func colorModelName(_ image.Config) string {
	return "rgb"
}

// storeImage inserts an image into the images table (deduped by content_hash)
// and returns the image ID.
func storeImage(db *sql.DB, data []byte, mimeType string, originalPath string) (int64, error) {
	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:])

	// Check if already exists.
	var id int64
	err := db.QueryRow(`SELECT id FROM images WHERE content_hash = ?`, hash).Scan(&id)
	if err == nil {
		return id, nil // already stored
	}

	// Decode image dimensions.
	cfg, _, decErr := image.DecodeConfig(strings.NewReader(string(data)))
	width, height := 0, 0
	pixelFormat := "unknown"
	if decErr == nil {
		width = cfg.Width
		height = cfg.Height
		pixelFormat = colorModelName(cfg)
	}

	var origPath any
	if originalPath != "" {
		origPath = originalPath
	}

	result, err := db.Exec(`
		INSERT OR IGNORE INTO images
			(content_hash, bytes, original_path, mime_type, width, height, pixel_format, byte_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hash, data, origPath, mimeType, width, height, pixelFormat, int64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("insert image: %w", err)
	}

	newID, err := result.LastInsertId()
	if err != nil || newID == 0 {
		// Race: another writer inserted it.
		err2 := db.QueryRow(`SELECT id FROM images WHERE content_hash = ?`, hash).Scan(&id)
		if err2 != nil {
			return 0, fmt.Errorf("find image after race: %w", err2)
		}
		return id, nil
	}
	return newID, nil
}

// recordOccurrence links an image to a session/entry (deduplicated).
func recordOccurrence(db *sql.DB, imageID int64, entryID int64, messageID int64, sessionID string, sourceType string, occurredAt string) {
	var entryArg, msgArg any
	if entryID > 0 {
		entryArg = entryID
	}
	if messageID > 0 {
		msgArg = messageID
	}
	db.Exec(`
		INSERT OR IGNORE INTO image_occurrences
			(image_id, entry_id, message_id, session_id, source_type, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		imageID, entryArg, msgArg, sessionID, sourceType, occurredAt)
}

// ingestImagesForEntry extracts inline base64 images from a raw JSONL entry
// and stores them in the images/image_occurrences tables.
// Called during ingest for user/assistant entries. Triggers per-image
// sidecar processing (OCR / description / embedding) for each new image.
func ingestImagesForEntry(s *Store, entryID int64, sessionID string, rawJSON []byte, occurredAt string) {
	var entry rawEntry
	if json.Unmarshal(rawJSON, &entry) != nil {
		return
	}

	if entry.Message.Content == nil {
		return
	}

	// Try array of content blocks.
	var blocks []imageContentBlock
	if json.Unmarshal(entry.Message.Content, &blocks) != nil {
		return
	}

	for _, b := range blocks {
		if b.Type != "image" {
			continue
		}
		if b.Source.Type != "base64" || b.Source.Data == "" {
			continue
		}

		imgData, err := base64.StdEncoding.DecodeString(b.Source.Data)
		if err != nil {
			slog.Debug("image base64 decode failed", "session", sessionID, "err", err)
			continue
		}

		mimeType := b.Source.MediaType
		if mimeType == "" {
			mimeType = "image/png"
		}

		imageID, err := storeImage(s.writeDB, imgData, mimeType, "")
		if err != nil {
			slog.Warn("store image failed", "session", sessionID, "err", err)
			continue
		}

		recordOccurrence(s.writeDB, imageID, entryID, 0, sessionID, "inline", occurredAt)
		s.triggerImageSidecars(imageID, imgData, mimeType)
	}
}

// ingestImageFromPath loads and stores an image file referenced by path,
// then triggers per-image sidecar processing.
func ingestImageFromPath(s *Store, path string, entryID int64, messageID int64, sessionID string, occurredAt string) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return // silently skip unreadable files
	}

	mimeType := extToMime(ext)
	imageID, err := storeImage(s.writeDB, data, mimeType, path)
	if err != nil {
		slog.Warn("store image from path failed", "path", path, "err", err)
		return
	}

	recordOccurrence(s.writeDB, imageID, entryID, messageID, sessionID, "path", occurredAt)
	s.triggerImageSidecars(imageID, data, mimeType)
}

// triggerImageSidecars runs the three sidecar pipelines (OCR,
// description, embedding) for a single image. runSidecar acquires a slot
// on the store-wide semaphore (sized at runtime.NumCPU()) BEFORE spawning
// each goroutine, so both the number of in-flight goroutines and the
// image bytes their closures pin are bounded no matter how large a
// backlog backfillImages feeds in (🎯T105); the caller back-pressures
// rather than launching thousands of parked goroutines. Each helper is
// idempotent and a cheap no-op if its backend is unavailable.
func (s *Store) triggerImageSidecars(imageID int64, data []byte, mimeType string) {
	if s == nil || s.writeDB == nil || s.imageSem == nil {
		return
	}
	s.runSidecar(func() { ocrOneImage(s.writeDB, imageID, data) })
	s.runSidecar(func() { describeOneImage(s.writeDB, imageID, data, mimeType) })
	s.runSidecar(func() { embedOneImage(s.writeDB, imageID, data, mimeType) })
}

// runSidecar acquires a slot on the shared image semaphore, then spawns a
// goroutine that runs fn and releases the slot. Acquiring BEFORE the `go`
// (rather than as the goroutine's first statement) is what bounds the
// spawn count and the memory fn's closure pins: the caller blocks here
// once NumCPU slots are taken instead of queueing unbounded goroutines
// (🎯T105).
func (s *Store) runSidecar(fn func()) {
	s.imageSem <- struct{}{}
	go func() {
		defer func() { <-s.imageSem }()
		fn()
	}()
}

// extToMime maps common image extensions to MIME types.
func extToMime(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

// backfillImages processes all existing entries and messages for images.
// Pass 1: entries.raw with inline base64 image blocks.
// Pass 2: messages with image-extension file paths.
// This is idempotent — image_occurrences has UNIQUE constraints.
func backfillImages(s *Store) {

	start := time.Now()

	// Pass 1: entries with inline image blocks.
	rows, err := s.readDB.Query(`
		SELECT e.id, e.session_id, e.raw, COALESCE(e.timestamp, datetime('now'))
		FROM entries e
		WHERE e.raw LIKE '%"type":"image"%'`)
	if err != nil {
		slog.Warn("backfill images pass1 query failed", "err", err)
		return
	}

	type entryRow struct {
		id        int64
		sessionID string
		raw       []byte
		ts        string
	}
	var entries []entryRow
	for rows.Next() {
		var r entryRow
		if rows.Scan(&r.id, &r.sessionID, &r.raw, &r.ts) == nil {
			entries = append(entries, r)
		}
	}
	rows.Close()

	inlineCount := 0
	for _, e := range entries {
		ingestImagesForEntry(s, e.id, e.sessionID, e.raw, e.ts)
		inlineCount++
	}

	// Pass 2: messages with image file paths.
	msgRows, err := s.readDB.Query(`
		SELECT m.id, m.entry_id, m.session_id, m.tool_file_path,
		       COALESCE(m.timestamp, datetime('now'))
		FROM messages m
		WHERE LOWER(m.tool_file_path) LIKE '%.png'
		   OR LOWER(m.tool_file_path) LIKE '%.jpg'
		   OR LOWER(m.tool_file_path) LIKE '%.jpeg'
		   OR LOWER(m.tool_file_path) LIKE '%.gif'
		   OR LOWER(m.tool_file_path) LIKE '%.webp'`)
	if err != nil {
		slog.Warn("backfill images pass2 query failed", "err", err)
		return
	}

	type msgRow struct {
		id        int64
		entryID   int64
		sessionID string
		filePath  string
		ts        string
	}
	var msgs []msgRow
	for msgRows.Next() {
		var r msgRow
		var entryID sql.NullInt64
		if msgRows.Scan(&r.id, &entryID, &r.sessionID, &r.filePath, &r.ts) == nil {
			if entryID.Valid {
				r.entryID = entryID.Int64
			}
			msgs = append(msgs, r)
		}
	}
	msgRows.Close()

	pathCount := 0
	for _, m := range msgs {
		ingestImageFromPath(s, m.filePath, m.entryID, m.id, m.sessionID, m.ts)
		pathCount++
	}

	// Report counts.
	var totalImages, totalOccurrences int
	s.readDB.QueryRow("SELECT COUNT(*) FROM images").Scan(&totalImages)                 //nolint:errcheck
	s.readDB.QueryRow("SELECT COUNT(*) FROM image_occurrences").Scan(&totalOccurrences) //nolint:errcheck

	if totalImages > 0 || inlineCount > 0 || pathCount > 0 {
		slog.Info("backfilled images",
			"entries_scanned", inlineCount,
			"messages_scanned", pathCount,
			"images_stored", totalImages,
			"occurrences", totalOccurrences,
			"elapsed", time.Since(start).Round(time.Millisecond))
	}
}
