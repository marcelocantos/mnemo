// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build system_test

package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// goldenEmbedCase pairs a fixture with a conceptual text query that
// should rank it top among the fixture set. Queries intentionally avoid
// verbatim substrings from the fixture text — this exercises CLIP's
// vocabulary generalisation rather than lexical overlap (which FTS5
// already handles).
type goldenEmbedCase struct {
	name         string
	png          string
	semanticText string
}

var goldenEmbedCases = []goldenEmbedCase{
	{
		name:         "code_snippet",
		png:          "testdata/images/code_snippet.png",
		semanticText: "source code listing with function definition",
	},
	{
		name:         "error_log",
		png:          "testdata/images/error_log.png",
		semanticText: "stack trace with connection failure message",
	},
	{
		name:         "architecture_diagram",
		png:          "testdata/images/architecture_diagram.png",
		semanticText: "flowchart showing system components and arrows",
	},
	{
		name:         "data_table",
		png:          "testdata/images/data_table.png",
		semanticText: "spreadsheet with rows of numerical financial data",
	},
}

// TestEmbeddingSemanticRanking verifies that embedding each fixture and
// querying with a conceptual (non-verbatim) description ranks the
// correct fixture first.
//
// Run with:
//
//	go test -tags "sqlite_fts5 system_test" -run TestEmbeddingSemanticRanking -v -timeout 5m ./internal/store/
func TestEmbeddingSemanticRanking(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH — embedding test skipped")
	}
	if !embedBackendAvailable() {
		t.Skip("embed backend not available")
	}

	// Embed every fixture up-front.
	type embedding struct {
		name   string
		vector []float32
	}
	embeddings := make([]embedding, 0, len(goldenEmbedCases))
	for _, fx := range goldenEmbedCases {
		path, err := filepath.Abs(fx.png)
		if err != nil {
			t.Fatalf("abs %s: %v", fx.png, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		_, _, vec, err := runEmbedImage(data, "image/png")
		if err != nil {
			t.Fatalf("embed %s: %v", fx.name, err)
		}
		if len(vec) == 0 {
			t.Fatalf("empty vector for %s", fx.name)
		}
		embeddings = append(embeddings, embedding{name: fx.name, vector: vec})
	}

	// For each fixture, embed the semantic query and assert its top hit.
	for _, fx := range goldenEmbedCases {
		t.Run(fx.name, func(t *testing.T) {
			_, _, queryVec, err := runEmbedText(fx.semanticText)
			if err != nil {
				t.Fatalf("embed query text: %v", err)
			}

			type scored struct {
				name  string
				score float32
			}
			var scores []scored
			for _, e := range embeddings {
				scores = append(scores, scored{
					name:  e.name,
					score: cosineSimilarity(queryVec, e.vector),
				})
			}

			// Find the best.
			best := scores[0]
			for _, s := range scores[1:] {
				if s.score > best.score {
					best = s
				}
			}

			if best.name != fx.name {
				lines := "rankings:\n"
				for _, s := range scores {
					lines += fmt.Sprintf("  %-24s  %.4f\n", s.name, s.score)
				}
				t.Errorf("query %q expected %q top, got %q\n%s",
					fx.semanticText, fx.name, best.name, lines)
				return
			}
			t.Logf("%-24s top=%.4f (%s: %q)", fx.name, best.score, fx.name, fx.semanticText)
		})
	}
}

// TestEmbeddingVisualSimilaritySelf verifies that each fixture embedded
// twice produces identical (or near-identical) vectors — a sanity check
// on determinism of the inference pipeline.
//
// Run with:
//
//	go test -tags "sqlite_fts5 system_test" -run TestEmbeddingVisualSimilaritySelf -v -timeout 5m ./internal/store/
func TestEmbeddingVisualSimilaritySelf(t *testing.T) {
	if !embedBackendAvailable() {
		t.Skip("embed backend not available")
	}
	for _, fx := range goldenEmbedCases {
		t.Run(fx.name, func(t *testing.T) {
			path, err := filepath.Abs(fx.png)
			if err != nil {
				t.Fatalf("abs: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			_, _, a, err := runEmbedImage(data, "image/png")
			if err != nil {
				t.Fatalf("embed 1: %v", err)
			}
			_, _, b, err := runEmbedImage(data, "image/png")
			if err != nil {
				t.Fatalf("embed 2: %v", err)
			}
			sim := cosineSimilarity(a, b)
			if sim < 0.999 {
				t.Errorf("self-similarity for %s should be ~1.0, got %.6f", fx.name, sim)
			} else {
				t.Logf("%s self-sim = %.6f", fx.name, sim)
			}
		})
	}
}
