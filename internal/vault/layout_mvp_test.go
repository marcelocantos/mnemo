// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// seedLayoutMVPStore builds a store with one session and one memory so
// Sync exercises memory dual-write and raw-signal layout policy for real.
func seedLayoutMVPStore(t *testing.T) *store.Store {
	t.Helper()
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-layout-mvp-01", []map[string]any{
		storetest.MetaMsg("user",
			"I propose we move decisions under _mnemo/decisions for the vault wing.",
			"2026-05-10T10:00:00Z", "/Users/alice/dev/myapp", "main"),
		storetest.Msg("assistant",
			"Agreed — I'll put decisions under _mnemo/decisions.",
			"2026-05-10T10:00:05Z"),
		storetest.Msg("user",
			"Yes, go ahead with that approach.",
			"2026-05-10T10:00:10Z"),
	})
	memDir := filepath.Join(projDir, "-Users-alice-dev-myapp", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	memBody := "---\nname: wing-layout\ndescription: vault wing layout note\n---\n\nKeep raw signal out of the wing.\n"
	if err := os.WriteFile(filepath.Join(memDir, "wing-layout.md"), []byte(memBody), 0o644); err != nil {
		t.Fatal(err)
	}
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := s.IngestMemories(); err != nil {
		t.Fatalf("IngestMemories: %v", err)
	}
	return s
}

func syncWithLayout(t *testing.T, layout string) string {
	t.Helper()
	s := seedLayoutMVPStore(t)
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")
	exp, err := New(s, vaultDir, Options{
		Layout:    layout,
		StatePath: statePath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return vaultDir
}

func mustFindUnder(t *testing.T, root, prefix string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		sl := filepath.ToSlash(rel)
		if strings.HasPrefix(sl, prefix) && strings.HasSuffix(sl, ".md") {
			hits = append(hits, sl)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestDecisionPathsForLayout(t *testing.T) {
	d := store.DecisionInfo{SessionID: "abcdef012345", Repo: "myapp", Timestamp: "2026-05-10T10:00:00Z"}
	v1 := filepath.ToSlash(decisionPath(d))
	v2 := filepath.ToSlash(decisionPathV2(d))
	if !strings.HasPrefix(v1, "decisions/") {
		t.Fatalf("v1 path: %s", v1)
	}
	if !strings.HasPrefix(v2, "_mnemo/decisions/") {
		t.Fatalf("v2 path: %s", v2)
	}
	cases := []struct {
		layout string
		want   []string
	}{
		{store.VaultLayoutV1, []string{v1}},
		{store.VaultLayoutBoth, []string{v1, v2}},
		{store.VaultLayoutV2, []string{v2}},
		{"", []string{v2}},
	}
	for _, tc := range cases {
		got := decisionPathsForLayout(tc.layout, d)
		if len(got) != len(tc.want) {
			t.Fatalf("layout %q: got %v want %v", tc.layout, got, tc.want)
		}
		for i := range got {
			if filepath.ToSlash(got[i]) != tc.want[i] {
				t.Errorf("layout %q [%d]: got %q want %q", tc.layout, i, got[i], tc.want[i])
			}
		}
	}
}

func TestMemoryPathsForLayout(t *testing.T) {
	m := store.MemoryInfo{Project: "myapp", Name: "wing-layout"}
	got := memoryPathsForLayout(store.VaultLayoutBoth, m)
	if len(got) != 2 {
		t.Fatalf("both: got %v", got)
	}
	if !strings.HasPrefix(filepath.ToSlash(got[0]), "memories/") {
		t.Errorf("v1 path: %s", got[0])
	}
	if !strings.HasPrefix(filepath.ToSlash(got[1]), "_mnemo/memories/") {
		t.Errorf("v2 path: %s", got[1])
	}
}

func TestSyncV2WritesWingMemoriesSuppressesRawSignal(t *testing.T) {
	vaultDir := syncWithLayout(t, store.VaultLayoutV2)

	if _, err := os.Stat(filepath.Join(vaultDir, "_mnemo", "index.md")); err != nil {
		t.Fatalf("expected _mnemo/index.md: %v", err)
	}

	for _, prefix := range []string{"sessions/", "ci/", "prs/", "repos/"} {
		if hits := mustFindUnder(t, vaultDir, prefix); len(hits) > 0 {
			t.Errorf("v2 must not write %s: %v", prefix, hits)
		}
	}
	// v1 roots must stay empty.
	if hits := mustFindUnder(t, vaultDir, "decisions/"); len(hits) > 0 {
		t.Errorf("v2 leaked v1 decisions/: %v", hits)
	}
	if hits := mustFindUnder(t, vaultDir, "memories/"); len(hits) > 0 {
		t.Errorf("v2 leaked v1 memories/: %v", hits)
	}

	memHits := mustFindUnder(t, vaultDir, "_mnemo/memories/")
	if len(memHits) == 0 {
		t.Fatal("expected at least one _mnemo/memories note")
	}
	body, err := os.ReadFile(filepath.Join(vaultDir, memHits[0]))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, needle := range []string{"type: memory", "mnemo/memory"} {
		if !strings.Contains(text, needle) {
			t.Errorf("memory frontmatter missing %q in %s:\n%s", needle, memHits[0], text[:min(400, len(text))])
		}
	}
}

func TestSyncBothDualWritesMemoriesAndKeepsSessions(t *testing.T) {
	vaultDir := syncWithLayout(t, store.VaultLayoutBoth)

	if hits := mustFindUnder(t, vaultDir, "sessions/"); len(hits) == 0 {
		t.Error("both: expected sessions/")
	}
	if hits := mustFindUnder(t, vaultDir, "memories/"); len(hits) == 0 {
		t.Error("both: expected v1 memories/")
	}
	if hits := mustFindUnder(t, vaultDir, "_mnemo/memories/"); len(hits) == 0 {
		t.Error("both: expected _mnemo/memories/")
	}
}

// TestSyncWritesDecisionPaths exercises writeNote + path selection for a
// concrete decision under both layouts via the same code path Sync uses
// (renderDecision → writeNote at decisionPathsForLayout targets).
func TestSyncWritesDecisionPaths(t *testing.T) {
	d := store.DecisionInfo{
		SessionID:        "decpath01deadbeef",
		Repo:             "myapp",
		Timestamp:        "2026-05-10T11:00:00Z",
		ProposalText:     "Use _mnemo/decisions for wing pages.",
		ConfirmationText: "Confirmed.",
	}
	for _, layout := range []string{store.VaultLayoutV2, store.VaultLayoutBoth, store.VaultLayoutV1} {
		t.Run(layout, func(t *testing.T) {
			dir := t.TempDir()
			content := renderDecision(d, "")
			for _, rel := range decisionPathsForLayout(layout, d) {
				abs := filepath.Join(dir, rel)
				if err := writeNote(abs, content, d.Timestamp); err != nil {
					t.Fatalf("writeNote %s: %v", rel, err)
				}
				raw, err := os.ReadFile(abs)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(raw), "type: decision") {
					t.Errorf("%s missing type frontmatter", rel)
				}
			}
			// Pure v2 must not create v1 decision root.
			if layout == store.VaultLayoutV2 {
				if _, err := os.Stat(filepath.Join(dir, "decisions")); !os.IsNotExist(err) {
					t.Errorf("v2 should not create v1 decisions/ dir")
				}
			}
		})
	}
}

func TestSyncV1KeepsLegacyPathsOnly(t *testing.T) {
	vaultDir := syncWithLayout(t, store.VaultLayoutV1)

	if hits := mustFindUnder(t, vaultDir, "_mnemo/"); len(hits) > 0 {
		t.Errorf("v1 must not write _mnemo/: %v", hits)
	}
	if hits := mustFindUnder(t, vaultDir, "sessions/"); len(hits) == 0 {
		t.Error("v1: expected sessions/")
	}
	if hits := mustFindUnder(t, vaultDir, "memories/"); len(hits) == 0 {
		t.Error("v1: expected memories/")
	}
}

func TestAtomicWriteFileReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "note.md")
	if err := os.WriteFile(dst, []byte("old content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(dst, []byte("new content\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content\n" {
		t.Fatalf("got %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mnemo-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteNoteUsesAtomicPath(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "sub", "note.md")
	if err := writeNote(dst, "# Hello\n\nbody\n", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), generatedFence) {
		t.Fatalf("missing fence: %s", raw)
	}
	if !strings.Contains(string(raw), "# Hello") {
		t.Fatalf("missing body: %s", raw)
	}
}

func TestRenderDecisionFrontmatterShape(t *testing.T) {
	d := store.DecisionInfo{
		SessionID:        "sess12345678",
		Repo:             "mnemo",
		Timestamp:        "2026-05-10T12:00:00Z",
		ProposalText:     "Ship the wing.",
		ConfirmationText: "Yes.",
	}
	out := renderDecision(d, "")
	for _, needle := range []string{
		"type: decision",
		"mnemo/decision",
		"first-seen:",
		"## Proposal",
		"## Outcome",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("missing %q in:\n%s", needle, out)
		}
	}
}
