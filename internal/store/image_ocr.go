// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	ocrWorkers   = 2
	ocrBatchSize = 50
	ocrPollEvery = 5 * time.Minute
)

// ocrOnce ensures the "no OCR backend" warning is logged only once.
var ocrOnce sync.Once

// ocrHelperPath is the cached path to the compiled macOS OCR helper.
var ocrHelperPath string
var ocrHelperOnce sync.Once

// ocrMacOSResult is the JSON output from tools/ocr-macos/main.swift.
type ocrMacOSResult struct {
	Text       string   `json:"text"`
	Confidence *float64 `json:"confidence"`
}

// buildMacOSOCRHelper compiles tools/ocr-macos/main.swift to ~/.mnemo/bin/mnemo-ocr
// if not already present. Returns the absolute path on success, empty string if
// swiftc is unavailable or we are not on darwin. Silently no-ops on non-darwin.
func buildMacOSOCRHelper() string {
	if runtime.GOOS != "darwin" {
		return ""
	}

	// Check swiftc availability.
	swiftc, err := exec.LookPath("swiftc")
	if err != nil {
		slog.Warn("swiftc not found — Apple Vision OCR unavailable")
		return ""
	}

	// Target path for the compiled binary.
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("could not determine home directory for OCR helper", "err", err)
		return ""
	}
	binDir := filepath.Join(home, ".mnemo", "bin")
	helperPath := filepath.Join(binDir, "mnemo-ocr")

	// Already compiled?
	if _, err := os.Stat(helperPath); err == nil {
		return helperPath
	}

	// Locate source relative to the executable.
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("could not determine executable path for OCR helper source lookup", "err", err)
		return ""
	}
	// Try <exe_dir>/../../tools/ocr-macos/main.swift (for dev tree).
	exeDir := filepath.Dir(exe)
	srcCandidates := []string{
		filepath.Join(exeDir, "..", "..", "tools", "ocr-macos", "main.swift"),
		filepath.Join(exeDir, "tools", "ocr-macos", "main.swift"),
	}
	var srcPath string
	for _, c := range srcCandidates {
		if _, err := os.Stat(c); err == nil {
			srcPath = c
			break
		}
	}
	if srcPath == "" {
		slog.Warn("mnemo-ocr Swift source not found — Apple Vision OCR unavailable")
		return ""
	}

	// Create output directory.
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		slog.Warn("could not create ~/.mnemo/bin", "err", err)
		return ""
	}

	slog.Info("compiling Apple Vision OCR helper", "src", srcPath, "dst", helperPath)
	cmd := exec.Command(swiftc, "-O",
		"-framework", "Vision",
		"-framework", "CoreImage",
		srcPath,
		"-o", helperPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Warn("swiftc failed — Apple Vision OCR unavailable", "err", err, "stderr", stderr.String())
		return ""
	}
	slog.Info("Apple Vision OCR helper compiled", "path", helperPath)
	return helperPath
}

// ocrHelperPathOnce returns (and caches) the path to the macOS OCR helper.
func getOCRHelperPath() string {
	ocrHelperOnce.Do(func() {
		ocrHelperPath = buildMacOSOCRHelper()
	})
	return ocrHelperPath
}

// ocrBackend returns the backend to use: "apple_vision", "tesseract", or "".
func ocrBackend() string {
	if runtime.GOOS == "darwin" && getOCRHelperPath() != "" {
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
		return runAppleVisionOCR(imageBytes)
	case "tesseract":
		return runTesseractOCR(imageBytes)
	default:
		return "", nil, fmt.Errorf("unknown OCR backend: %s", backend)
	}
}

// runAppleVisionOCR shells out to the compiled Swift helper.
func runAppleVisionOCR(imageBytes []byte) (string, *float64, error) {
	helperPath := getOCRHelperPath()
	if helperPath == "" {
		return "", nil, fmt.Errorf("Apple Vision helper not available")
	}

	cmd := exec.Command(helperPath)
	cmd.Stdin = bytes.NewReader(imageBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return "", nil, fmt.Errorf("apple_vision: %s", msg)
	}

	var result ocrMacOSResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", nil, fmt.Errorf("apple_vision: parse output: %w", err)
	}
	return result.Text, result.Confidence, nil
}

// runTesseractOCR writes image data to a temp file and runs tesseract.
func runTesseractOCR(imageBytes []byte) (string, *float64, error) {
	// Write to a temp file; tesseract reads from a file path.
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

	// tesseract <infile> stdout --psm 3 -l eng
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
	// Tesseract does not provide per-observation confidence via CLI; confidence is NULL.
	return stdout.String(), nil, nil
}

// StartImageOCR launches background workers that run OCR on images that
// don't yet have an image_ocr entry. Safe to call multiple times — workers
// exit when there are no more pending images and re-poll every 5 minutes.
func (s *Store) StartImageOCR() {
	backend := ocrBackend()
	if backend == "" {
		ocrOnce.Do(func() {
			slog.Warn("no OCR backend available (swiftc/Apple Vision not found on macOS, tesseract not installed) — image OCR will be skipped")
		})
		return
	}
	slog.Info("starting image OCR workers", "backend", backend, "workers", ocrWorkers)
	for range ocrWorkers {
		go imageOCRWorker(s.db, backend)
	}
}

// imageOCRWorker polls for images without OCR entries and processes them.
func imageOCRWorker(db *sql.DB, backend string) {
	ticker := time.NewTicker(ocrPollEvery)
	defer ticker.Stop()

	processPendingOCR(db, backend)
	for range ticker.C {
		processPendingOCR(db, backend)
	}
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
