// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

const embedScriptPath = "tools/embed-clip/embed.py"

// embedderOnce ensures the "uv not available" warning is logged only once.
var embedderOnce sync.Once

// embedBackendAvailable returns true if uv is available and the embed script exists.
func embedBackendAvailable() bool {
	if _, err := exec.LookPath("uv"); err != nil {
		return false
	}
	// Try to locate the script relative to the executable.
	scriptPath := resolveEmbedScript()
	if scriptPath == "" {
		return false
	}
	_, err := os.Stat(scriptPath)
	return err == nil
}

// resolveEmbedScript finds the embed.py script relative to the running binary.
// Falls back to a path relative to the Go module root for development.
func resolveEmbedScript() string {
	// Relative to executable.
	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "..", embedScriptPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		// Also try alongside the binary (flat install).
		candidate2 := filepath.Join(filepath.Dir(exe), "embed.py")
		if _, err := os.Stat(candidate2); err == nil {
			return candidate2
		}
	}

	// Development: walk up from cwd to find go.mod.
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			candidate := filepath.Join(dir, embedScriptPath)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// embedRequest is the JSON struct sent to embed.py on stdin.
type embedRequest struct {
	Mode string `json:"mode"`
	Path string `json:"path,omitempty"`
	Text string `json:"text,omitempty"`
}

// embedResponse is the JSON struct received from embed.py on stdout.
type embedResponse struct {
	Model  string    `json:"model"`
	Dim    int       `json:"dim"`
	Vector []float32 `json:"vector"`
	Error  string    `json:"error,omitempty"`
}

// runEmbed shells out to the Python helper and returns an embedding.
func runEmbed(req embedRequest) (model string, dim int, vector []float32, err error) {
	scriptPath := resolveEmbedScript()
	if scriptPath == "" {
		return "", 0, nil, fmt.Errorf("embed script not found")
	}

	reqJSON, err := json.Marshal(req)
	if err != nil {
		return "", 0, nil, fmt.Errorf("marshal embed request: %w", err)
	}

	cmd := exec.Command("uv", "run", "--script", scriptPath)
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if ok := fmt.Errorf("%w", err); ok != nil {
			if ee, ok2 := err.(*exec.ExitError); ok2 {
				exitErr = ee
				_ = exitErr
				return "", 0, nil, fmt.Errorf("embed script error: %s", string(ee.Stderr))
			}
		}
		return "", 0, nil, fmt.Errorf("run embed script: %w", err)
	}

	var resp embedResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", 0, nil, fmt.Errorf("parse embed response: %w", err)
	}
	if resp.Error != "" {
		return "", 0, nil, fmt.Errorf("embed backend error: %s", resp.Error)
	}
	return resp.Model, resp.Dim, resp.Vector, nil
}

// runEmbedImage writes image bytes to a temp file and calls the embed helper.
func runEmbedImage(imageBytes []byte, mimeType string) (model string, dim int, vector []float32, err error) {
	ext := ".png"
	switch mimeType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	}

	tmp, err := os.CreateTemp("", "mnemo-embed-*"+ext)
	if err != nil {
		return "", 0, nil, fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(imageBytes); err != nil {
		tmp.Close()
		return "", 0, nil, fmt.Errorf("write temp file: %w", err)
	}
	tmp.Close()

	return runEmbed(embedRequest{Mode: "image", Path: tmp.Name()})
}

// runEmbedText embeds a text query for semantic search.
func runEmbedText(text string) (model string, dim int, vector []float32, err error) {
	return runEmbed(embedRequest{Mode: "text", Text: text})
}

// encodeVector encodes a float32 slice as a little-endian BLOB.
func encodeVector(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVector decodes a little-endian BLOB into a float32 slice.
func decodeVector(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// cosineSimilarity computes cosine similarity between two equal-length vectors.
// Returns 0 if either vector has zero magnitude.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

// StartImageEmbedder runs one backfill pass over all images without
// embeddings. Returns when the queue is empty. Fresh images arriving
// after startup are handled per-image via embedOneImage, triggered by
// ingestImagesForEntry / ingestImageFromPath.
func (s *Store) StartImageEmbedder() {
	if !embedBackendAvailable() {
		embedderOnce.Do(func() {
			slog.Warn("embed backend unavailable (uv or embed script not found) — image embeddings will be skipped")
		})
		return
	}
	slog.Info("starting embedder backfill")
	go processUnembeddedImages(s)
}

// embedOneImage generates and stores an embedding for a single image.
// Idempotent: skips if an embedding already exists.
func embedOneImage(db *sql.DB, imageID int64, data []byte, mimeType string) {
	if !embedBackendAvailable() {
		return
	}
	var exists int
	if err := db.QueryRow(`SELECT 1 FROM image_embeddings WHERE image_id = ? LIMIT 1`, imageID).Scan(&exists); err == nil {
		return
	}
	model, dim, vector, err := runEmbedImage(data, mimeType)
	if err != nil {
		storeEmbedding(db, imageID, "", 0, nil, err.Error())
		return
	}
	storeEmbedding(db, imageID, model, dim, vector, "")
}

// processUnembeddedImages generates embeddings for all images without one.
func processUnembeddedImages(s *Store) {
	s.rwmu.RLock()
	rows, err := s.db.Query(`
		SELECT img.id, img.bytes, img.mime_type
		FROM images img
		WHERE NOT EXISTS (
			SELECT 1 FROM image_embeddings e WHERE e.image_id = img.id
		)
		ORDER BY img.created_at DESC
		LIMIT 50`)
	if err != nil {
		s.rwmu.RUnlock()
		slog.Warn("image embedder query failed", "err", err)
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
	s.rwmu.RUnlock()

	for _, pi := range pending {
		embedAndStore(s, pi.id, pi.data, pi.mimeType)
	}
}

// embedAndStore generates an embedding for an image and stores it.
func embedAndStore(s *Store, imageID int64, data []byte, mimeType string) {
	model, dim, vector, err := runEmbedImage(data, mimeType)
	if err != nil {
		slog.Warn("image embedding failed", "image_id", imageID, "err", err)
		storeEmbedding(s.db, imageID, "", 0, nil, err.Error())
		return
	}
	storeEmbedding(s.db, imageID, model, dim, vector, "")
	slog.Debug("image embedded", "image_id", imageID, "model", model, "dim", dim)
}

// storeEmbedding inserts or replaces an image_embeddings row.
func storeEmbedding(db *sql.DB, imageID int64, model string, dim int, vector []float32, errMsg string) {
	var errArg any
	if errMsg != "" {
		errArg = errMsg
	}
	var blobArg any
	if vector != nil {
		blobArg = encodeVector(vector)
	}
	_, err := db.Exec(`
		INSERT OR REPLACE INTO image_embeddings
			(image_id, model, dim, vector, error)
		VALUES (?, ?, ?, ?, ?)`,
		imageID, model, dim, blobArg, errArg)
	if err != nil {
		slog.Warn("store image embedding failed", "image_id", imageID, "err", err)
	}
}

// candidateEmbedding holds a loaded image embedding for k-NN ranking.
type candidateEmbedding struct {
	imageID int64
	vector  []float32
}

// loadCandidateEmbeddings loads all image embeddings for a given model
// matching the provided image IDs (nil means all).
func loadCandidateEmbeddings(db *sql.DB, model string, imageIDs []int64) ([]candidateEmbedding, error) {
	var q string
	var args []any

	if len(imageIDs) == 0 {
		q = `SELECT image_id, vector FROM image_embeddings WHERE error IS NULL AND model = ?`
		args = []any{model}
	} else {
		// Build IN clause.
		placeholders := make([]byte, 0, len(imageIDs)*2)
		for i, id := range imageIDs {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, id)
		}
		q = fmt.Sprintf(`SELECT image_id, vector FROM image_embeddings WHERE error IS NULL AND model = ? AND image_id IN (%s)`, string(placeholders))
		args = append([]any{model}, args...)
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("load candidate embeddings: %w", err)
	}
	defer rows.Close()

	var result []candidateEmbedding
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			continue
		}
		result = append(result, candidateEmbedding{imageID: id, vector: decodeVector(blob)})
	}
	return result, nil
}

// knnSearch ranks candidates by cosine similarity and returns the top-k image IDs.
func knnSearch(query []float32, candidates []candidateEmbedding, k int) []int64 {
	type scored struct {
		id    int64
		score float32
	}

	numWorkers := runtime.NumCPU()
	if numWorkers > len(candidates) {
		numWorkers = len(candidates)
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	scored_ch := make(chan scored, len(candidates))
	chunkSize := (len(candidates) + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		start := w * chunkSize
		end := start + chunkSize
		if end > len(candidates) {
			end = len(candidates)
		}
		if start >= end {
			break
		}
		wg.Add(1)
		go func(slice []candidateEmbedding) {
			defer wg.Done()
			for _, c := range slice {
				scored_ch <- scored{id: c.imageID, score: cosineSimilarity(query, c.vector)}
			}
		}(candidates[start:end])
	}
	go func() {
		wg.Wait()
		close(scored_ch)
	}()

	var scores []scored
	for s := range scored_ch {
		scores = append(scores, s)
	}

	// Partial sort: find top-k.
	// Simple approach: insertion-style bounded heap is fine at this scale.
	top := make([]scored, 0, k+1)
	for _, s := range scores {
		top = append(top, s)
		// Bubble the new element up if needed.
		for i := len(top) - 1; i > 0 && top[i].score > top[i-1].score; i-- {
			top[i], top[i-1] = top[i-1], top[i]
		}
		if len(top) > k {
			top = top[:k]
		}
	}

	ids := make([]int64, len(top))
	for i, s := range top {
		ids[i] = s.id
	}
	return ids
}
