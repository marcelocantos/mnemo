// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestEmbedHelperProcess is not a real test: it is the child process the
// fault-injection tests exec in place of `uv run --script embed.py`. It
// reproduces the observed failure shape — chatter about a model download
// on stderr, then a non-zero exit — with no network and no uv.
func TestEmbedHelperProcess(t *testing.T) {
	if os.Getenv("MNEMO_EMBED_HELPER") == "" {
		t.Skip("helper process for TestEmbedScriptFailure*; not a standalone test")
	}
	fmt.Fprintln(os.Stderr, "Downloading transformers (11.1MiB)...")
	os.Exit(1)
}

// injectFailingEmbedHelper points embedCommand at TestEmbedHelperProcess
// for the duration of the test.
func injectFailingEmbedHelper(t *testing.T) {
	t.Helper()
	prev := embedCommand
	t.Cleanup(func() { embedCommand = prev })
	embedCommand = func() (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=TestEmbedHelperProcess")
		cmd.Env = append(os.Environ(), "MNEMO_EMBED_HELPER=1")
		return cmd, nil
	}
}

// TestEmbedScriptFailureIsRecorded is the 🎯T121 regression test. A
// failing embed helper used to be written to image_embeddings with a
// NULL vector, which SQLite rejects (`NOT NULL constraint failed:
// image_embeddings.vector`) — so the failure went unrecorded and the
// image was retried on every startup. The failure must now land in
// image_embedding_attempts, and the image must drop out of the pending
// queue once its attempt budget is spent.
func TestEmbedScriptFailureIsRecorded(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	injectFailingEmbedHelper(t)

	imageID, err := storeImage(s.writeDB, []byte("fake-image-bytes"), "image/png", "")
	if err != nil {
		t.Fatalf("storeImage: %v", err)
	}

	embedAndStore(s.writeDB, imageID, []byte("fake-image-bytes"), "image/png")

	// No embedding row: the constraint-violating insert is gone, not
	// merely tolerated.
	var embeddings int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM image_embeddings WHERE image_id = ?`, imageID,
	).Scan(&embeddings); err != nil {
		t.Fatalf("count embeddings: %v", err)
	}
	if embeddings != 0 {
		t.Fatalf("failed embedding wrote %d image_embeddings rows, want 0", embeddings)
	}

	// The failure is recorded, with the helper's stderr preserved so the
	// cause (a model-weight download) is visible without the log.
	var status, errMsg string
	var attempts int
	if err := s.readDB.QueryRow(
		`SELECT status, attempts, error FROM image_embedding_attempts WHERE image_id = ?`, imageID,
	).Scan(&status, &attempts, &errMsg); err != nil {
		t.Fatalf("read attempt row: %v", err)
	}
	if status != embedStatusFailed {
		t.Fatalf("status = %q, want %q", status, embedStatusFailed)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d after one failure, want 1", attempts)
	}
	if !strings.Contains(errMsg, "Downloading transformers") {
		t.Fatalf("recorded error %q does not carry the helper's stderr", errMsg)
	}

	// Still pending: one failure is inside the budget, so a transient
	// fault is retried.
	if got := s.EmbedderStatus().Pending; got != 1 {
		t.Fatalf("pending = %d after one failure, want 1", got)
	}

	// Drain the budget; the image must then stop being offered for work.
	for range embedMaxAttempts {
		embedAndStore(s.writeDB, imageID, []byte("fake-image-bytes"), "image/png")
	}
	st := s.EmbedderStatus()
	if st.Pending != 0 {
		t.Fatalf("pending = %d after exhausting the attempt budget, want 0", st.Pending)
	}
	if st.Failed != 1 {
		t.Fatalf("failed = %d, want 1", st.Failed)
	}
	if !strings.Contains(st.LastError, "Downloading transformers") {
		t.Fatalf("status last_error = %q, want the helper's stderr", st.LastError)
	}
}

// TestEmbedWorthAttempting covers the per-image gate that keeps the
// ingest path (embedOneImage) from re-spawning the helper for an image
// that is already embedded or has spent its attempt budget.
func TestEmbedWorthAttempting(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	fresh, err := storeImage(s.writeDB, []byte("fresh-image"), "image/png", "")
	if err != nil {
		t.Fatalf("storeImage: %v", err)
	}
	if !embedWorthAttempting(s.writeDB, fresh) {
		t.Fatalf("a never-attempted image should be worth attempting")
	}

	exhausted, err := storeImage(s.writeDB, []byte("doomed-image"), "image/png", "")
	if err != nil {
		t.Fatalf("storeImage: %v", err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO image_embedding_attempts (image_id, status, attempts, error)
		 VALUES (?, ?, ?, 'boom')`, exhausted, embedStatusFailed, embedMaxAttempts,
	); err != nil {
		t.Fatalf("seed attempt row: %v", err)
	}
	if embedWorthAttempting(s.writeDB, exhausted) {
		t.Fatalf("a budget-exhausted image should not be attempted again")
	}

	embedded, err := storeImage(s.writeDB, []byte("done-image"), "image/png", "")
	if err != nil {
		t.Fatalf("storeImage: %v", err)
	}
	storeEmbedding(s.writeDB, embedded, "clip-ViT-B-32", 2, []float32{0.5, 0.5})
	if embedWorthAttempting(s.writeDB, embedded) {
		t.Fatalf("an already-embedded image should not be attempted again")
	}
}

// TestEmbedderStatusReportsWhySkipped covers the 🎯T121 acceptance that
// "skipped, and why" is retrievable without the log — here for the
// default state, which must win over dependency detection.
func TestEmbedderStatusReportsWhySkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv(MnemoHomeEnv, home)
	if err := WriteConfig(Config{}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	s := newTestStore(t, t.TempDir())
	st := s.EmbedderStatus()
	if st.Enabled {
		t.Fatalf("embedder reports enabled without an explicit opt-in")
	}
	if st.Reason != embedReasonDisabled {
		t.Fatalf("reason = %q, want %q", st.Reason, embedReasonDisabled)
	}
	if !strings.Contains(st.Detail, "image_embeddings") {
		t.Fatalf("detail %q does not name the config key", st.Detail)
	}
}

// TestImageEmbeddingsAreOptIn is the load-bearing guard for the gate
// (🎯T121): an absent config section must leave the embedder off, so no
// PyPI resolution or HuggingFace weight download can be reached by a
// user who never asked for one. Opting in flips it, subject to the
// dependencies actually being present.
func TestImageEmbeddingsAreOptIn(t *testing.T) {
	t.Setenv(MnemoHomeEnv, t.TempDir())

	// Default: section omitted entirely.
	if err := WriteConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	if got := resolveEmbedBackend(); got.ready || got.reason != embedReasonDisabled {
		t.Errorf("embedder must be off by default; got ready=%v reason=%q",
			got.ready, got.reason)
	}

	// Explicitly disabled reads the same.
	if err := WriteConfig(Config{ImageEmbeddings: ImageEmbeddingsConfig{Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if got := resolveEmbedBackend(); got.ready {
		t.Error("explicit enabled=false must keep the embedder off")
	}

	// Opted in: no longer gated on config. Whether it is *ready* then
	// depends on uv and the helper script, which this test does not
	// require — only that the refusal is no longer the config one.
	if err := WriteConfig(Config{ImageEmbeddings: ImageEmbeddingsConfig{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if got := resolveEmbedBackend(); got.reason == embedReasonDisabled {
		t.Error("opting in must clear the config gate")
	}
}
