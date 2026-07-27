// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const embedScriptPath = "tools/embed-clip/embed.py"

// embedBatchSize caps one backfill pass, mirroring ocrBatchSize.
const embedBatchSize = 50

// embedMaxAttempts bounds how many times a single image may be put
// through the embed helper before it is left alone (🎯T121). Before this
// existed, a failing image was retried on every daemon startup — each
// attempt a subprocess spawn plus, on a cold uv cache, a model-weight
// download. A small allowance still absorbs genuinely transient failures
// (an interrupted first-run download), which is why this is not 1.
const embedMaxAttempts = 3

// Terminal outcomes recorded in image_embedding_attempts.status.
const (
	embedStatusOK     = "ok"
	embedStatusFailed = "failed"
)

// Machine-readable reasons reported by EmbedderStatus.Reason, so callers
// can tell "ran" from "skipped, and why" without parsing prose (🎯T121).
const (
	embedReasonReady    = "ready"
	embedReasonDisabled = "disabled"
	embedReasonNoUV     = "no_uv"
	embedReasonNoScript = "no_script"
)

// embedderOnce ensures the "embeddings skipped" warning is logged only
// once; the live reason stays queryable via EmbedderStatus.
var embedderOnce sync.Once

// embedBackendState is the resolved availability of the embed helper:
// whether it can run, and if not, why.
type embedBackendState struct {
	ready      bool
	reason     string
	detail     string
	scriptPath string
}

// resolveEmbedBackend reports whether the embed helper can run, and the
// reason when it cannot. The opt-in check comes first so a disabled
// embedder is reported as a deliberate choice rather than as a missing
// dependency — and so no PyPI or HuggingFace fetch can be reached by a
// user who never asked for one (🎯T121).
//
// Config is read per call (cheap, hot-reloadable) so toggling
// image_embeddings.enabled takes effect without a daemon restart. An
// unreadable config leaves the embedder OFF: the default for this
// feature is "do not fetch", so a config we cannot parse must not be
// the thing that starts a download.
func resolveEmbedBackend() embedBackendState {
	cfg, err := LoadConfig()
	if err != nil || !cfg.ImageEmbeddings.IsEnabled() {
		return embedBackendState{
			reason: embedReasonDisabled,
			detail: `image embeddings are opt-in; set {"image_embeddings":{"enabled":true}} in ~/.mnemo/config.json`,
		}
	}
	if _, err := exec.LookPath("uv"); err != nil {
		return embedBackendState{
			reason: embedReasonNoUV,
			detail: "uv is not on the daemon's PATH",
		}
	}
	scriptPath := resolveEmbedScript()
	if scriptPath == "" {
		return embedBackendState{
			reason: embedReasonNoScript,
			detail: "embed helper " + embedScriptPath + " not found (it ships only in the source tree, not in release archives)",
		}
	}
	return embedBackendState{
		ready:      true,
		reason:     embedReasonReady,
		detail:     "embed helper available at " + scriptPath,
		scriptPath: scriptPath,
	}
}

// embedBackendAvailable returns true if the embed helper can run.
func embedBackendAvailable() bool { return resolveEmbedBackend().ready }

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

// embedCommand builds the subprocess that runs the embed helper, or
// returns an error when the helper cannot be located. Indirected through
// a package var so a test can inject a deterministic failure (🎯T121):
// the real command shells out to uv, which resolves PyPI dependencies
// and downloads model weights, neither of which belongs in a unit test.
var embedCommand = func() (*exec.Cmd, error) {
	scriptPath := resolveEmbedScript()
	if scriptPath == "" {
		return nil, fmt.Errorf("embed script not found")
	}
	return exec.Command("uv", "run", "--script", scriptPath), nil
}

// runEmbed shells out to the Python helper and returns an embedding.
func runEmbed(req embedRequest) (model string, dim int, vector []float32, err error) {
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return "", 0, nil, fmt.Errorf("marshal embed request: %w", err)
	}

	cmd, err := embedCommand()
	if err != nil {
		return "", 0, nil, err
	}
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", 0, nil, fmt.Errorf("embed script error: %s", strings.TrimSpace(string(exitErr.Stderr)))
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
	st := resolveEmbedBackend()
	if !st.ready {
		embedderOnce.Do(func() {
			slog.Warn("image embeddings will be skipped", "reason", st.reason, "detail", st.detail)
		})
		return
	}
	slog.Info("starting embedder backfill")
	go processUnembeddedImages(s)
}

// embedOneImage generates and stores an embedding for a single image.
// Idempotent: skips if an embedding already exists or if the image has
// exhausted its attempt budget.
func embedOneImage(db *sql.DB, imageID int64, data []byte, mimeType string) {
	if !embedBackendAvailable() || !embedWorthAttempting(db, imageID) {
		return
	}
	embedAndStore(db, imageID, data, mimeType)
}

// embedWorthAttempting reports whether imageID still warrants an embed
// attempt: it has no embedding yet and has not spent its attempt budget.
// The single-image counterpart of the anti-join in
// processUnembeddedImages. Errors resolve to true so a transient read
// failure never silently drops work.
func embedWorthAttempting(db *sql.DB, imageID int64) bool {
	var exists int
	if err := db.QueryRow(`SELECT 1 FROM image_embeddings WHERE image_id = ? LIMIT 1`, imageID).Scan(&exists); err == nil {
		return false
	}
	var attempts int
	if err := db.QueryRow(
		`SELECT attempts FROM image_embedding_attempts WHERE image_id = ?`, imageID,
	).Scan(&attempts); err != nil {
		return true
	}
	return attempts < embedMaxAttempts
}

// processUnembeddedImages generates embeddings for all images that have
// neither an embedding nor an exhausted attempt budget.
func processUnembeddedImages(s *Store) {
	rows, err := s.readDB.Query(`
		SELECT img.id, img.bytes, img.mime_type
		FROM images img
		LEFT JOIN image_embedding_attempts a ON a.image_id = img.id
		WHERE NOT EXISTS (
			SELECT 1 FROM image_embeddings e WHERE e.image_id = img.id
		)
		AND COALESCE(a.attempts, 0) < ?
		ORDER BY img.created_at DESC
		LIMIT ?`, embedMaxAttempts, embedBatchSize)
	if err != nil {
		slog.Warn("image embedder query failed", "err", err)
		return
	}

	type unembeddedImage struct {
		id       int64
		data     []byte
		mimeType string
	}
	var pending []unembeddedImage
	for rows.Next() {
		var pi unembeddedImage
		if rows.Scan(&pi.id, &pi.data, &pi.mimeType) == nil {
			pending = append(pending, pi)
		}
	}
	rows.Close()

	for _, pi := range pending {
		embedAndStore(s.writeDB, pi.id, pi.data, pi.mimeType)
	}
}

// embedAndStore generates an embedding for an image and records the
// outcome. Success writes the vector; failure writes only an attempt row
// (🎯T121). image_embeddings.vector is NOT NULL, so the old failure path
// — inserting a NULL vector — was rejected by SQLite, leaving the failure
// unrecorded and the image queued for retry on every subsequent pass.
func embedAndStore(db *sql.DB, imageID int64, data []byte, mimeType string) {
	model, dim, vector, err := runEmbedImage(data, mimeType)
	if err != nil {
		slog.Warn("image embedding failed", "image_id", imageID, "err", err)
		recordEmbedAttempt(db, imageID, embedStatusFailed, err.Error())
		return
	}
	if len(vector) == 0 {
		// A clean exit with an empty vector is still a failure; storing it
		// would violate the NOT NULL column just as an error would.
		slog.Warn("image embedding returned no vector", "image_id", imageID, "model", model)
		recordEmbedAttempt(db, imageID, embedStatusFailed, "embed helper returned an empty vector")
		return
	}
	storeEmbedding(db, imageID, model, dim, vector)
	recordEmbedAttempt(db, imageID, embedStatusOK, "")
	slog.Debug("image embedded", "image_id", imageID, "model", model, "dim", dim)
}

// storeEmbedding inserts or replaces an image_embeddings row. Callers
// must supply a non-empty vector; failures belong in
// image_embedding_attempts.
//
// The legacy `error` column is deliberately left out of the insert: it
// is never written any more (phase 1 of the CLAUDE.md deprecation
// strategy), and existing readers filter on `error IS NULL`, which the
// omission satisfies.
func storeEmbedding(db *sql.DB, imageID int64, model string, dim int, vector []float32) {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO image_embeddings
			(image_id, model, dim, vector)
		VALUES (?, ?, ?, ?)`,
		imageID, model, dim, encodeVector(vector))
	if err != nil {
		slog.Warn("store image embedding failed", "image_id", imageID, "err", err)
	}
}

// recordEmbedAttempt upserts the terminal outcome of one embed attempt,
// incrementing the per-image attempt counter. The counter is what stops
// a permanently-failing image from being retried forever, and the row is
// what makes "did the embedder run, and what happened" answerable from
// SQL rather than from the daemon log (🎯T121).
func recordEmbedAttempt(db *sql.DB, imageID int64, status, errMsg string) {
	_, err := db.Exec(`
		INSERT INTO image_embedding_attempts
			(image_id, status, attempts, error, last_attempt_at)
		VALUES (?, ?, 1, ?, datetime('now'))
		ON CONFLICT(image_id) DO UPDATE SET
			status = excluded.status,
			attempts = image_embedding_attempts.attempts + 1,
			error = excluded.error,
			last_attempt_at = excluded.last_attempt_at`,
		imageID, status, errMsg)
	if err != nil {
		slog.Warn("record image embedding attempt failed", "image_id", imageID, "err", err)
	}
}

// EmbedderStatus reports whether the image embedder can run — and if
// not, why — alongside the terminal outcomes it has recorded (🎯T121).
// This is the "without reading the log" surface: it backs the
// images.embedder diagnostic check, and hence mnemo_doctor and /health.
type EmbedderStatus struct {
	// Enabled is true when the helper can actually run.
	Enabled bool `json:"enabled"`
	// Reason is machine-readable: ready, disabled, no_uv, or no_script.
	Reason string `json:"reason"`
	// Detail is the human-facing expansion of Reason.
	Detail string `json:"detail"`
	// ScriptPath is the resolved helper path when Enabled.
	ScriptPath string `json:"script_path,omitempty"`
	// Embedded, Failed and Pending count images by outcome. Pending
	// excludes images that have exhausted their attempt budget — those
	// are counted as Failed and will not be retried.
	Embedded int `json:"embedded"`
	Failed   int `json:"failed"`
	Pending  int `json:"pending"`
	// LastError is the most recent recorded failure, if any. Weight
	// downloads that could not be fetched surface here.
	LastError string `json:"last_error,omitempty"`
}

// EmbedderStatus assembles the current embedder status. Count queries
// that fail leave their counters at zero rather than failing the whole
// report — a status surface must not be the thing that breaks.
func (s *Store) EmbedderStatus() EmbedderStatus {
	backend := resolveEmbedBackend()
	st := EmbedderStatus{
		Enabled:    backend.ready,
		Reason:     backend.reason,
		Detail:     backend.detail,
		ScriptPath: backend.scriptPath,
	}
	s.readDB.QueryRow(`SELECT COUNT(*) FROM image_embeddings`).Scan(&st.Embedded)       //nolint:errcheck
	s.readDB.QueryRow(`SELECT COUNT(*) FROM image_embedding_attempts WHERE status = ?`, //nolint:errcheck
		embedStatusFailed).Scan(&st.Failed)
	s.readDB.QueryRow(`
		SELECT COUNT(*)
		FROM images img
		LEFT JOIN image_embedding_attempts a ON a.image_id = img.id
		WHERE NOT EXISTS (
			SELECT 1 FROM image_embeddings e WHERE e.image_id = img.id
		)
		AND COALESCE(a.attempts, 0) < ?`, embedMaxAttempts).Scan(&st.Pending) //nolint:errcheck
	s.readDB.QueryRow(`
		SELECT error FROM image_embedding_attempts
		WHERE status = ? AND error <> ''
		ORDER BY last_attempt_at DESC LIMIT 1`,
		embedStatusFailed).Scan(&st.LastError) //nolint:errcheck
	return st
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
