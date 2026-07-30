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

// patternsTestExporter seeds a store whose sessions read transcript
// JSONL directly across two repos — four occurrences over two sessions,
// which clears the emission gate — mines them, and syncs a vault at the
// given layout. Returns the vault root.
func patternsTestExporter(t *testing.T, layout string) (*Exporter, string) {
	t.Helper()
	projDir := t.TempDir()
	const (
		cwdA = "/Users/alice/work/github.com/acme/alpha"
		cwdB = "/Users/alice/work/github.com/acme/beta"
	)
	ts := func(hoursAgo int) string {
		return time.Now().UTC().Add(-time.Duration(hoursAgo) * time.Hour).Format(time.RFC3339)
	}
	bashRead := func(session, uuid, cwd, at, cmd string) map[string]any {
		return map[string]any{
			"type":      "assistant",
			"uuid":      uuid,
			"sessionId": session,
			"timestamp": at,
			"cwd":       cwd,
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_" + uuid, "name": "Bash",
						"input": map[string]any{"command": cmd}},
				},
			},
		}
	}
	storetest.WriteJSONL(t, projDir, "projA", "sess-pat-a", []map[string]any{
		bashRead("sess-pat-a", "pa1", cwdA, ts(50), "cat ~/.claude/projects/x/one.jsonl"),
		bashRead("sess-pat-a", "pa2", cwdA, ts(49), "head ~/.claude/projects/x/two.jsonl"),
		bashRead("sess-pat-a", "pa3", cwdA, ts(48), "tail ~/.claude/projects/x/three.jsonl"),
	})
	storetest.WriteJSONL(t, projDir, "projB", "sess-pat-b", []map[string]any{
		bashRead("sess-pat-b", "pb1", cwdB, ts(20), "wc -l ~/.claude/projects/y/four.jsonl"),
	})

	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatalf("RefreshPatterns: %v", err)
	}

	vaultDir := t.TempDir()
	exp, err := New(s, vaultDir, Options{
		Layout:    layout,
		StatePath: filepath.Join(t.TempDir(), "state.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return exp, vaultDir
}

// readPatternPages returns the non-index pattern pages in the wing.
func readPatternPages(t *testing.T, vaultDir string) map[string]string {
	t.Helper()
	dir := filepath.Join(vaultDir, "_mnemo", "patterns")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "_index.md" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(raw)
	}
	return out
}

// TestPatternPagesRenderedUnderV2 is the renderer half of 🎯T64.7: a
// pattern past the emission gate gets a page in the wing, tagged into
// the mnemo/pattern namespace, with the "Occurrences" provenance
// heading the design's entity table specifies.
func TestPatternPagesRenderedUnderV2(t *testing.T) {
	_, vaultDir := patternsTestExporter(t, store.VaultLayoutV2)

	pages := readPatternPages(t, vaultDir)
	if len(pages) != 1 {
		t.Fatalf("wrote %d pattern pages, want 1: %v", len(pages), keysOf(pages))
	}
	var name, body string
	for name, body = range pages {
	}
	if !strings.HasPrefix(name, "direct-jsonl-read-") {
		t.Errorf("page name %q does not lead with the pattern type", name)
	}
	for _, want := range []string{
		"type: pattern",
		"- mnemo\n",
		"- mnemo/pattern\n",
		"- pattern-direct-jsonl-read\n",
		"occurrence_count: 4",
		"session_count: 2",
		"## Occurrences",
		"## Suggestion",
		"acme/alpha",
		"acme/beta",
		generatedFence,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pattern page missing %q\n---\n%s", want, body)
		}
	}
}

// TestPatternsIndexListsPages checks the collection hub exists and
// links every page. Without it the wing's pattern notes are orphans in
// the user's graph view, which is the shape the design explicitly
// rejects.
func TestPatternsIndexListsPages(t *testing.T) {
	_, vaultDir := patternsTestExporter(t, store.VaultLayoutV2)

	raw, err := os.ReadFile(filepath.Join(vaultDir, "_mnemo", "patterns", "_index.md"))
	if err != nil {
		t.Fatalf("_index.md missing: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "patterns: 1") {
		t.Errorf("index does not report the collection size:\n%s", body)
	}
	for name := range readPatternPages(t, vaultDir) {
		stem := strings.TrimSuffix(name, ".md")
		if !strings.Contains(body, "[["+"_mnemo/patterns/"+stem) {
			t.Errorf("index does not link %s:\n%s", stem, body)
		}
	}
}

// TestPatternsSuppressedUnderV1 pins the wing-only rule: v1 never had a
// patterns/ directory, so there is nothing to be backwards-compatible
// with and writing one would put mnemo content back at the vault root
// that 🎯T64.4 moved out of it.
func TestPatternsSuppressedUnderV1(t *testing.T) {
	_, vaultDir := patternsTestExporter(t, store.VaultLayoutV1)

	if _, err := os.Stat(filepath.Join(vaultDir, "_mnemo", "patterns")); !os.IsNotExist(err) {
		t.Errorf("v1 layout wrote _mnemo/patterns (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "patterns")); !os.IsNotExist(err) {
		t.Errorf("v1 layout wrote a root-level patterns/ dir (stat err = %v)", err)
	}
}

// TestPatternPageAnnotationSurvivesResync exercises the fence contract
// on the new collection: a user note below the fence must outlive the
// next sync, exactly as it does for decisions and memories.
func TestPatternPageAnnotationSurvivesResync(t *testing.T) {
	exp, vaultDir := patternsTestExporter(t, store.VaultLayoutV2)

	pages := readPatternPages(t, vaultDir)
	var name string
	for name = range pages {
	}
	pagePath := filepath.Join(vaultDir, "_mnemo", "patterns", name)

	const annotation = "This one is deliberate — the daemon isn't running in CI."
	raw, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pagePath, []byte(string(raw)+"\n"+annotation+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Force a rewrite: needsUpdate skips a page whose recorded entity
	// timestamp is current, and the point here is the merge, not the skip.
	if err := os.Remove(filepath.Join(vaultDir, "_mnemo", "patterns", "_index.md")); err != nil {
		t.Fatal(err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), annotation) {
		t.Errorf("annotation lost across re-sync:\n%s", after)
	}
}

// TestPatternPageBelowGateNotWritten checks a sub-threshold pattern
// produces no page — the vault should not assert a pattern exists on
// the strength of one session.
func TestPatternPageBelowGateNotWritten(t *testing.T) {
	projDir := t.TempDir()
	const cwd = "/Users/alice/work/github.com/acme/alpha"
	at := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	entry := func(uuid, cmd string) map[string]any {
		return map[string]any{
			"type": "assistant", "uuid": uuid, "sessionId": "sess-thin",
			"timestamp": at, "cwd": cwd,
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "tool_use", "id": "toolu_" + uuid, "name": "Bash",
						"input": map[string]any{"command": cmd}},
				},
			},
		}
	}
	storetest.WriteJSONL(t, projDir, "projA", "sess-thin", []map[string]any{
		entry("t1", "cat ~/.claude/projects/x/one.jsonl"),
		entry("t2", "cat ~/.claude/projects/x/two.jsonl"),
		entry("t3", "cat ~/.claude/projects/x/three.jsonl"),
	})
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}

	vaultDir := t.TempDir()
	exp, err := New(s, vaultDir, Options{
		Layout:    store.VaultLayoutV2,
		StatePath: filepath.Join(t.TempDir(), "state.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	if pages := readPatternPages(t, vaultDir); len(pages) != 0 {
		t.Errorf("wrote %d pages for a single-session pattern: %v", len(pages), keysOf(pages))
	}
	raw, err := os.ReadFile(filepath.Join(vaultDir, "_mnemo", "patterns", "_index.md"))
	if err != nil {
		t.Fatalf("_index.md should still exist to say the collection is empty: %v", err)
	}
	if !strings.Contains(string(raw), "No pattern currently clears that bar") {
		t.Errorf("empty index does not say so:\n%s", raw)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
