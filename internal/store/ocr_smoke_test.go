// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && smoke

package store

import (
	"os"
	"testing"
)

//	Run with: go test -tags "sqlite_fts5 smoke" -run TestOCRSmoke -v ./internal/store/ \
//	  -- -ocr-image /tmp/ocr-test/shot.png
//
// Or set MNEMO_OCR_SMOKE_IMAGE.
func TestOCRSmoke(t *testing.T) {
	path := os.Getenv("MNEMO_OCR_SMOKE_IMAGE")
	if path == "" {
		t.Skip("set MNEMO_OCR_SMOKE_IMAGE to a PNG path to run this smoke test")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text, conf, err := runAppleVisionOCRNative(data)
	if err != nil {
		t.Fatalf("OCR failed: %v", err)
	}
	if conf == nil {
		t.Fatal("expected non-nil confidence")
	}
	t.Logf("confidence: %.3f", *conf)
	t.Logf("chars: %d", len(text))
	if len(text) > 400 {
		t.Logf("text (first 400): %s", text[:400])
	} else {
		t.Logf("text: %s", text)
	}
	if len(text) == 0 {
		t.Fatal("expected non-empty recognized text")
	}
}
