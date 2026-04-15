// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && system_test

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenImageCase couples a fixture PNG with the strings we expect OCR to
// recover from it. Fixtures are generated deterministically from the
// sibling .md files via scripts/gen-golden-images.sh (Markdown -> PDF via
// vellum -> PNG via pdftoppm).
type goldenImageCase struct {
	name     string
	png      string
	mustHave []string
}

var goldenImageCases = []goldenImageCase{
	{
		name: "code_snippet",
		png:  "testdata/images/code_snippet.png",
		mustHave: []string{
			"calculateFoobarIndex",
			"events",
			"Weight",
			"42",
		},
	},
	{
		name: "error_log",
		png:  "testdata/images/error_log.png",
		mustHave: []string{
			"ECONNREFUSED",
			"5432",
			"abc12345",
			"db/pool.go",
		},
	},
	{
		name: "architecture_diagram",
		png:  "testdata/images/architecture_diagram.png",
		mustHave: []string{
			"JSONL transcripts",
			"OCR worker",
			"CLIP embedder",
			"FTS5 index",
		},
	},
	{
		name: "data_table",
		png:  "testdata/images/data_table.png",
		mustHave: []string{
			"North America",
			"48,200",
			"Asia Pacific",
			"Q4 2026",
		},
	},
}

// TestOCRGoldenImages runs Apple Vision OCR against each generated fixture
// and asserts the distinctive tokens from the source Markdown survive the
// render + OCR round-trip.
//
// Run with: go test -tags "sqlite_fts5 system_test" -run TestOCRGoldenImages -v ./internal/store/
func TestOCRGoldenImages(t *testing.T) {
	for _, tc := range goldenImageCases {
		t.Run(tc.name, func(t *testing.T) {
			path, err := filepath.Abs(tc.png)
			if err != nil {
				t.Fatalf("resolve path: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture %s: %v (regenerate via scripts/gen-golden-images.sh)", path, err)
			}

			text, conf, err := runAppleVisionOCRNative(data)
			if err != nil {
				t.Fatalf("OCR failed: %v", err)
			}
			if conf == nil {
				t.Fatal("expected non-nil confidence")
			}
			t.Logf("confidence: %.3f  chars: %d", *conf, len(text))

			var missing []string
			for _, want := range tc.mustHave {
				if !strings.Contains(text, want) {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				t.Errorf("OCR output missing expected tokens: %v\n--- OCR text ---\n%s", missing, text)
			}
		})
	}
}
