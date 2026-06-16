// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// hashTree walks dir and returns a stable composite SHA-256 over
// (sorted relative path, file bytes). Used to assert byte-identical
// output from the generator across two runs with the same seed.
func hashTree(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	var paths []string
	if err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		paths = append(paths, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// filepath.Walk already returns lexical order, but be explicit.
	for _, p := range paths {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newSmallGen(seed int64) *Generator {
	return &Generator{
		Seed:        seed,
		Sessions:    8,
		Repos:       []string{"acme/alpha", "acme/beta"},
		MsgsDist:    Distribution{Min: 4, Max: 12, Mean: 7},
		TokensDist:  Distribution{Min: 50, Max: 400, Mean: 200},
		ToolUseRate: 0.3,
	}
}

// TestGeneratorDeterministic asserts bit-identical output across
// two runs with the same Seed and config — the cornerstone property
// every downstream test relies on for reproducibility.
func TestGeneratorDeterministic(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	if err := newSmallGen(42).Write(dirA); err != nil {
		t.Fatal(err)
	}
	if err := newSmallGen(42).Write(dirB); err != nil {
		t.Fatal(err)
	}

	if got, want := hashTree(t, dirA), hashTree(t, dirB); got != want {
		t.Fatalf("same-seed runs diverged:\n A: %s\n B: %s", got, want)
	}

	// Different seed → different output, sanity check.
	dirC := t.TempDir()
	if err := newSmallGen(99).Write(dirC); err != nil {
		t.Fatal(err)
	}
	if hashTree(t, dirA) == hashTree(t, dirC) {
		t.Fatal("different seeds produced identical output")
	}
}

// TestGeneratorIngests confirms the generated tree is consumable by
// the real ingest path, not just a JSONL look-alike. If the shape
// drifts from what mnemo expects, this test catches it before any
// downstream consumer wires the generator into its own fixtures.
func TestGeneratorIngests(t *testing.T) {
	projectDir := t.TempDir()
	gen := &Generator{
		Seed:        7,
		Sessions:    100,
		Repos:       []string{"acme/alpha", "acme/beta", "acme/gamma"},
		MsgsDist:    Distribution{Min: 6, Max: 20, Mean: 12},
		TokensDist:  Distribution{Min: 100, Max: 500, Mean: 250},
		ToolUseRate: 0.4,
	}
	if err := gen.Write(projectDir); err != nil {
		t.Fatal(err)
	}

	s := NewStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Generated session count flows through to ListSessions when
	// minMessages is set low enough to include all synthetic
	// sessions (MsgsDist.Min=6, so a min of 1 always passes).
	sessions, err := s.ListSessions("all", 1, 1000, "", "", "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) < gen.Sessions {
		t.Fatalf("expected ≥%d sessions ingested, got %d", gen.Sessions, len(sessions))
	}

	// Repos must be threaded through cwd → session_meta.repo. With
	// 3 repos × 100 sessions all under acme/*, ListRepos should
	// report all three.
	repos, err := s.ListRepos("acme/")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) < len(gen.Repos) {
		t.Fatalf("expected ≥%d repos, got %d: %+v", len(gen.Repos), len(repos), repos)
	}
}

// TestGeneratorTokenDistribution asserts the generated assistant
// messages land near the requested mean output_tokens. The check
// is loose (±20%) because the triangular sampler is only an
// approximation and small samples are noisy; tighter bounds would
// be flaky.
func TestGeneratorTokenDistribution(t *testing.T) {
	projectDir := t.TempDir()
	wantMean := 300
	gen := &Generator{
		Seed:        13,
		Sessions:    50,
		Repos:       []string{"acme/alpha"},
		MsgsDist:    Distribution{Min: 10, Max: 30, Mean: 20},
		TokensDist:  Distribution{Min: 100, Max: 600, Mean: wantMean},
		ToolUseRate: 0.2,
	}
	if err := gen.Write(projectDir); err != nil {
		t.Fatal(err)
	}

	s := NewStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Use a wide explicit window so the synthetic 2026-01-01 epoch
	// timestamps fall inside it; Usage()'s default Days=30 window
	// is anchored on time.Now() and would exclude older fixtures.
	usage, err := s.Usage(store.UsageParams{
		Since:   "2025-01-01T00:00:00Z",
		Until:   "2030-01-01T00:00:00Z",
		GroupBy: "day",
	})
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.Total.Messages == 0 {
		t.Fatal("Usage reported zero assistant messages — ingest didn't see usage rows")
	}
	gotMean := float64(usage.Total.OutputTokens) / float64(usage.Total.Messages)
	lo, hi := float64(wantMean)*0.8, float64(wantMean)*1.2
	if gotMean < lo || gotMean > hi {
		t.Fatalf("output_tokens mean=%.1f outside ±20%% of target %d (range %.1f..%.1f)",
			gotMean, wantMean, lo, hi)
	}
}
