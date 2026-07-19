// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestSignalFileMtimeFreshAndStale(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "heartbeat")
	if err := os.WriteFile(f, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewSignalEvaluator("", []store.SignalSource{{
		Name: "hb", Kind: store.SignalKindFileMtime, Path: f,
		Cadence: "1h", GraceMultiple: 2,
	}})
	fixed := time.Now()
	e.now = func() time.Time { return fixed }

	res := e.DiagChecks()[0].Run(context.Background())
	if res.Severity.String() != "ok" {
		t.Fatalf("fresh: %+v", res)
	}

	// Age the file beyond threshold.
	old := fixed.Add(-3 * time.Hour)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}
	res = e.DiagChecks()[0].Run(context.Background())
	if res.Severity.String() != "fail" {
		t.Fatalf("stale: %+v", res)
	}
	if res.Remediation == "" {
		t.Fatal("expected remediation")
	}
}

func TestSignalNewestArtifact(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(a, []byte("1"), 0o644)
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(b, []byte("2"), 0o644)
	e := NewSignalEvaluator("", []store.SignalSource{{
		Name: "art", Kind: store.SignalKindNewestArtifact, Path: dir,
		Cadence: "1h",
	}})
	res := e.DiagChecks()[0].Run(context.Background())
	if res.Severity.String() != "ok" {
		t.Fatalf("%+v", res)
	}
}
