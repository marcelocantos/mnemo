// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
)

const ocrBatchSize = 50

// ocrOnce ensures the "no OCR backend" warning is logged only once.
var ocrOnce sync.Once

// ocrBackend returns the backend to use: "apple_vision", "tesseract", or "".
// apple_vision is in-process via CGO on macOS; tesseract shells out; empty
// means neither is available.
func ocrBackend() string {
	if appleVisionAvailable {
		return "apple_vision"
	}
	if _, err := exec.LookPath("tesseract"); err == nil {
		return "tesseract"
	}
	return ""
}

// runOCR extracts text from imageBytes using the given backend.
// Returns (text, confidence pointer, error).
func runOCR(imageBytes []byte, backend string) (string, *float64, error) {
	switch backend {
	case "apple_vision":
		return runAppleVisionOCRNative(imageBytes)
	case "tesseract":
		return runTesseractOCR(imageBytes)
	default:
		return "", nil, fmt.Errorf("unknown OCR backend: %s", backend)
	}
}

// runTesseractOCR writes image data to a temp file and runs tesseract.
// Tesseract reads from a file path and writes recognized text to stdout.
func runTesseractOCR(imageBytes []byte) (string, *float64, error) {
	tmp, err := os.CreateTemp("", "mnemo-ocr-*.png")
	if err != nil {
		return "", nil, fmt.Errorf("tesseract: create temp: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(imageBytes); err != nil {
		tmp.Close()
		return "", nil, fmt.Errorf("tesseract: write temp: %w", err)
	}
	tmp.Close()

	cmd := exec.Command("tesseract", tmp.Name(), "stdout", "--psm", "3", "-l", "eng")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return "", nil, fmt.Errorf("tesseract: %s", msg)
	}
	// Tesseract CLI does not expose per-observation confidence; leave NULL.
	return stdout.String(), nil, nil
}

// StartImageOCR runs one backfill pass over all images without OCR
// entries. Returns when the queue is empty. Fresh images that arrive
// after startup are handled per-image via ocrOneImage, triggered by
// ingestImagesForEntry / ingestImageFromPath.
func (s *Store) StartImageOCR() {
	backend := ocrBackend()
	if backend == "" {
		ocrOnce.Do(func() {
			slog.Warn("no OCR backend available (Apple Vision is macOS-only, tesseract not installed) — image OCR will be skipped")
		})
		return
	}
	slog.Info("starting OCR backfill", "backend", backend)
	go processPendingOCR(s.db, backend)
}

// ocrOneImage runs OCR on a single newly-ingested image. Idempotent via
// the image_ocr table's PK on image_id (INSERT OR IGNORE semantics here
// by checking existence first).
func ocrOneImage(db *sql.DB, imageID int64, data []byte) {
	backend := ocrBackend()
	if backend == "" {
		return
	}
	// Skip if already processed.
	var exists int
	if err := db.QueryRow(`SELECT 1 FROM image_ocr WHERE image_id = ? LIMIT 1`, imageID).Scan(&exists); err == nil {
		return
	}
	ocrAndStore(db, imageID, data, backend)
}

// processPendingOCR runs OCR on images that have no image_ocr row yet.
func processPendingOCR(db *sql.DB, backend string) {
	rows, err := db.Query(`
		SELECT img.id, img.bytes, img.mime_type
		FROM images img
		WHERE NOT EXISTS (
			SELECT 1 FROM image_ocr o WHERE o.image_id = img.id
		)
		ORDER BY img.created_at DESC
		LIMIT ?`, ocrBatchSize)
	if err != nil {
		slog.Warn("image OCR query failed", "err", err)
		return
	}

	type pendingImage struct {
		id       int64
		data     []byte
		mimeType string
	}
	var pending []pendingImage
	for rows.Next() {
		var pi pendingImage
		if rows.Scan(&pi.id, &pi.data, &pi.mimeType) == nil {
			pending = append(pending, pi)
		}
	}
	rows.Close()

	for _, pi := range pending {
		ocrAndStore(db, pi.id, pi.data, backend)
	}
}

// ocrAndStore runs OCR on image bytes and persists the result.
func ocrAndStore(db *sql.DB, imageID int64, data []byte, backend string) {
	text, confidence, err := runOCR(data, backend)

	var errMsg string
	if err != nil {
		errMsg = err.Error()
		slog.Warn("image OCR failed", "image_id", imageID, "backend", backend, "err", err)
	} else {
		slog.Debug("image OCR complete", "image_id", imageID, "backend", backend, "chars", len(text))
	}

	storeOCR(db, imageID, text, backend, confidence, errMsg)
}

// storeOCR inserts (or replaces) an image_ocr row.
func storeOCR(db *sql.DB, imageID int64, text, backend string, confidence *float64, errMsg string) {
	var errArg any
	if errMsg != "" {
		errArg = errMsg
	}
	_, err := db.Exec(`
		INSERT OR REPLACE INTO image_ocr (image_id, text, backend, confidence, error)
		VALUES (?, ?, ?, ?, ?)`,
		imageID, text, backend, confidence, errArg)
	if err != nil {
		slog.Warn("store image OCR failed", "image_id", imageID, "err", err)
	}
}
