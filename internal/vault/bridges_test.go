// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

func TestLocateBridgeBlock(t *testing.T) {
	sd := bridgeStartDelim("themes")
	ed := bridgeEndDelim("themes")

	t.Run("absent", func(t *testing.T) {
		_, _, count, err := locateBridgeBlock("no fence here", "themes")
		if err != nil || count != 0 {
			t.Fatalf("count=%d err=%v, want 0/nil", count, err)
		}
	})
	t.Run("single", func(t *testing.T) {
		content := "top\n" + sd + "\n- x\n" + ed + "\nbottom\n"
		start, end, count, err := locateBridgeBlock(content, "themes")
		if err != nil || count != 1 {
			t.Fatalf("count=%d err=%v, want 1/nil", count, err)
		}
		if content[start:end] != sd+"\n- x\n"+ed {
			t.Errorf("located block = %q", content[start:end])
		}
	})
	t.Run("duplicate start", func(t *testing.T) {
		content := sd + "\n" + ed + "\n" + sd + "\n" + ed + "\n"
		if _, _, _, err := locateBridgeBlock(content, "themes"); !errors.Is(err, errDuplicateFence) {
			t.Fatalf("err=%v, want errDuplicateFence", err)
		}
	})
}

func TestRenderBridgeBlock(t *testing.T) {
	got := renderBridgeBlock("decisions", []string{"- a", "- b"})
	want := bridgeStartDelim("decisions") + "\n- a\n- b\n" + bridgeEndDelim("decisions")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Empty body → just the two delimiters, no interior blank line.
	empty := renderBridgeBlock("themes", nil)
	if empty != bridgeStartDelim("themes")+"\n"+bridgeEndDelim("themes") {
		t.Errorf("empty body render = %q", empty)
	}
}

func TestUpsertBridgeBlockCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	anchor := filepath.Join(dir, "sub", "Themes MOC.md")
	if err := upsertBridgeBlock(anchor, "themes", []string{"- [[x]]"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	raw, err := os.ReadFile(anchor)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(raw)
	if !strings.HasPrefix(got, "# Themes MOC\n") {
		t.Errorf("missing derived header, got:\n%s", got)
	}
	if !strings.Contains(got, bridgeStartDelim("themes")) || !strings.Contains(got, "- [[x]]") {
		t.Errorf("missing block/body:\n%s", got)
	}
}

func TestUpsertBridgeBlockAppendsPreservingUserContent(t *testing.T) {
	dir := t.TempDir()
	anchor := filepath.Join(dir, "notes.md")
	user := "# My Notes\n\nSome important user text.\n"
	if err := os.WriteFile(anchor, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertBridgeBlock(anchor, "patterns", []string{"- [[p]]"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	raw, _ := os.ReadFile(anchor)
	got := string(raw)
	if !strings.HasPrefix(got, user) {
		t.Errorf("user content not preserved at top:\n%s", got)
	}
	if !strings.Contains(got, bridgeStartDelim("patterns")) {
		t.Errorf("block not appended:\n%s", got)
	}
}

func TestUpsertBridgeBlockReplacesInPlaceHonouringMovedBlock(t *testing.T) {
	dir := t.TempDir()
	anchor := filepath.Join(dir, "moc.md")
	// User has relocated the block into the middle of the file.
	initial := "# MOC\n\n" +
		bridgeStartDelim("decisions") + "\nold link\n" + bridgeEndDelim("decisions") + "\n\n" +
		"## User section below\ntext\n"
	if err := os.WriteFile(anchor, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertBridgeBlock(anchor, "decisions", []string{"- new link"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := string(mustRead(t, anchor))
	if strings.Contains(got, "old link") {
		t.Error("old block content survived")
	}
	if !strings.Contains(got, "- new link") {
		t.Error("new block content missing")
	}
	// User content on both sides is untouched, block stays in the middle.
	if !strings.HasPrefix(got, "# MOC\n") || !strings.Contains(got, "## User section below") {
		t.Errorf("surrounding user content disturbed:\n%s", got)
	}
	if strings.Index(got, bridgeStartDelim("decisions")) > strings.Index(got, "## User section below") {
		t.Errorf("block should remain above the user section:\n%s", got)
	}
	// Exactly one fence pair after replace.
	if strings.Count(got, bridgeStartDelim("decisions")) != 1 {
		t.Errorf("expected exactly one fence, got:\n%s", got)
	}
}

func TestUpsertBridgeBlockDuplicateFenceLeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	anchor := filepath.Join(dir, "dup.md")
	dupe := bridgeStartDelim("themes") + "\na\n" + bridgeEndDelim("themes") + "\n" +
		bridgeStartDelim("themes") + "\nb\n" + bridgeEndDelim("themes") + "\n"
	if err := os.WriteFile(anchor, []byte(dupe), 0o644); err != nil {
		t.Fatal(err)
	}
	err := upsertBridgeBlock(anchor, "themes", []string{"- x"})
	if !errors.Is(err, errDuplicateFence) {
		t.Fatalf("err=%v, want errDuplicateFence", err)
	}
	// File is byte-for-byte unchanged.
	if string(mustRead(t, anchor)) != dupe {
		t.Error("file was modified despite duplicate fence")
	}
}

func TestStripBridgeBlock(t *testing.T) {
	t.Run("removes block, keeps user content", func(t *testing.T) {
		dir := t.TempDir()
		anchor := filepath.Join(dir, "a.md")
		content := "# Head\n\nbefore\n\n" +
			bridgeStartDelim("memories") + "\n### proj\n- [[m]]\n" + bridgeEndDelim("memories") + "\n\nafter\n"
		os.WriteFile(anchor, []byte(content), 0o644)
		if err := stripBridgeBlock(anchor, "memories"); err != nil {
			t.Fatalf("strip: %v", err)
		}
		got := string(mustRead(t, anchor))
		if strings.Contains(got, "mnemo:bridge:memories") {
			t.Errorf("fence not stripped:\n%s", got)
		}
		if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
			t.Errorf("user content lost:\n%s", got)
		}
	})
	t.Run("no-op when fence absent", func(t *testing.T) {
		dir := t.TempDir()
		anchor := filepath.Join(dir, "b.md")
		os.WriteFile(anchor, []byte("just user content\n"), 0o644)
		if err := stripBridgeBlock(anchor, "themes"); err != nil {
			t.Fatalf("strip: %v", err)
		}
		if string(mustRead(t, anchor)) != "just user content\n" {
			t.Error("file changed on no-op strip")
		}
	})
	t.Run("missing file is nil", func(t *testing.T) {
		if err := stripBridgeBlock(filepath.Join(t.TempDir(), "nope.md"), "themes"); err != nil {
			t.Fatalf("strip missing: %v", err)
		}
	})
}

// ---- syncBridges integration (through Sync) --------------------------------

// newBridgeExporter builds an Exporter over an empty store with a
// tempdir state.json, ready to exercise bridge reconciliation.
func newBridgeExporter(t *testing.T, bridges map[string]string) (*Exporter, string, string) {
	t.Helper()
	s := storetest.NewStore(t, t.TempDir())
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")
	exp, err := New(s, vaultDir, Options{
		Layout:    store.VaultLayoutV2,
		StatePath: statePath,
		Bridges:   bridges,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exp, vaultDir, statePath
}

func TestSyncBridgesWritesBlockAndTracksState(t *testing.T) {
	exp, vaultDir, statePath := newBridgeExporter(t, map[string]string{
		"decisions": "MOCs/Decisions.md",
	})
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	anchor := filepath.Join(vaultDir, "MOCs", "Decisions.md")
	got := string(mustRead(t, anchor))
	if !strings.Contains(got, bridgeStartDelim("decisions")) {
		t.Errorf("decisions bridge block not written:\n%s", got)
	}
	st, err := store.LoadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if st.WrittenBridges["decisions"] != "MOCs/Decisions.md" {
		t.Errorf("WrittenBridges = %v, want decisions→MOCs/Decisions.md", st.WrittenBridges)
	}
}

func TestSyncBridgesUnknownCollectionRecordsError(t *testing.T) {
	exp, _, statePath := newBridgeExporter(t, map[string]string{
		"notacollection": "X.md",
	})
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st, _ := store.LoadState(statePath)
	if len(st.BridgeErrors) == 0 {
		t.Fatal("expected a bridge error for unknown collection")
	}
	if st.BridgeErrors[0].Name != "notacollection" {
		t.Errorf("error name = %q, want notacollection", st.BridgeErrors[0].Name)
	}
	if _, ok := st.WrittenBridges["notacollection"]; ok {
		t.Error("unknown collection should not be recorded as written")
	}
}

func TestSyncBridgesRemovedBridgeStripsBlock(t *testing.T) {
	// First sync: bridge present.
	exp, vaultDir, statePath := newBridgeExporter(t, map[string]string{
		"decisions": "MOCs/Decisions.md",
	})
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	anchor := filepath.Join(vaultDir, "MOCs", "Decisions.md")
	// Add user content below the block to prove it survives the strip.
	prev := string(mustRead(t, anchor))
	os.WriteFile(anchor, []byte(prev+"\nuser tail\n"), 0o644)

	// Second exporter over the SAME vault + state, bridge removed.
	s := storetest.NewStore(t, t.TempDir())
	s.IngestAll()
	exp2, err := New(s, vaultDir, Options{
		Layout:    store.VaultLayoutV2,
		StatePath: statePath,
		Bridges:   nil, // removed
	})
	if err != nil {
		t.Fatalf("New2: %v", err)
	}
	if err := exp2.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	got := string(mustRead(t, anchor))
	if strings.Contains(got, "mnemo:bridge:decisions") {
		t.Errorf("removed bridge block not stripped:\n%s", got)
	}
	if !strings.Contains(got, "user tail") {
		t.Errorf("user content lost during strip:\n%s", got)
	}
	st, _ := store.LoadState(statePath)
	if _, ok := st.WrittenBridges["decisions"]; ok {
		t.Error("removed bridge still tracked in WrittenBridges")
	}
}

// #1: a relocation whose old-anchor strip fails must not orphan the old
// block. The old-anchor record survives for a later retry, the new
// anchor is not written, and the failure is surfaced.
func TestSyncBridgesRelocationStripFailureDoesNotOrphan(t *testing.T) {
	exp, vaultDir, statePath := newBridgeExporter(t, map[string]string{
		"decisions": "A.md",
	})
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	aPath := filepath.Join(vaultDir, "A.md")
	// Duplicate the fence so the strip fails with errDuplicateFence.
	dup := string(mustRead(t, aPath)) + "\n" +
		bridgeStartDelim("decisions") + "\n" + bridgeEndDelim("decisions") + "\n"
	if err := os.WriteFile(aPath, []byte(dup), 0o644); err != nil {
		t.Fatal(err)
	}

	// Relocate the bridge to B.md over the same vault + state.
	s := storetest.NewStore(t, t.TempDir())
	s.IngestAll()
	exp2, err := New(s, vaultDir, Options{
		Layout:    store.VaultLayoutV2,
		StatePath: statePath,
		Bridges:   map[string]string{"decisions": "B.md"},
	})
	if err != nil {
		t.Fatalf("New2: %v", err)
	}
	if err := exp2.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	if _, err := os.Stat(filepath.Join(vaultDir, "B.md")); err == nil {
		t.Error("new anchor B.md written while old block still stands (orphan)")
	}
	if string(mustRead(t, aPath)) != dup {
		t.Error("A.md modified despite duplicate fence")
	}
	st, _ := store.LoadState(statePath)
	if st.WrittenBridges["decisions"] != "A.md" {
		t.Errorf("WrittenBridges = %v, want decisions→A.md (retained for retry)", st.WrittenBridges)
	}
	if len(st.BridgeErrors) == 0 {
		t.Error("expected a bridge error for the failed relocation strip")
	}
}

// #3: a no-op upsert must not rewrite the user-owned anchor (no mtime
// bump, no rename that would break hardlinks / sync heuristics).
func TestUpsertBridgeBlockIdempotentNoRewrite(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "M.md")
	body := []string{"- a", "- b"}
	if err := upsertBridgeBlock(anchor, "decisions", body); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Backdate mtime; a no-op second upsert must leave it untouched.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(anchor, past, past); err != nil {
		t.Fatal(err)
	}
	before := string(mustRead(t, anchor))
	if err := upsertBridgeBlock(anchor, "decisions", body); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	fi, err := os.Stat(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(past) {
		t.Errorf("no-op upsert rewrote the file (mtime bumped to %v)", fi.ModTime())
	}
	if string(mustRead(t, anchor)) != before {
		t.Error("content changed on a no-op upsert")
	}
}

// #2: containedAnchor rejects any anchor that escapes the vault root.
func TestContainedAnchorRejectsEscape(t *testing.T) {
	root := filepath.FromSlash("/vault/root")
	cases := map[string]bool{
		"x.md":           true,
		"sub/x.md":       true,
		"sub/../ok.md":   true,
		"../escape.md":   false,
		"../../etc/x.md": false,
	}
	for anchor, want := range cases {
		if _, ok := containedAnchor(root, filepath.FromSlash(anchor)); ok != want {
			t.Errorf("containedAnchor(%q) = %v, want %v", anchor, ok, want)
		}
	}
}

// #2 (integration): an escaping anchor writes nothing outside the vault
// and is surfaced as an error rather than silently honoured.
func TestSyncBridgesRejectsEscapingAnchor(t *testing.T) {
	exp, vaultDir, statePath := newBridgeExporter(t, map[string]string{
		"decisions": filepath.FromSlash("../escape.md"),
	})
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(vaultDir), "escape.md")); err == nil {
		t.Error("bridge wrote outside the vault root")
	}
	st, _ := store.LoadState(statePath)
	if len(st.BridgeErrors) == 0 || st.BridgeErrors[0].Reason != "anchor outside vault" {
		t.Errorf("expected 'anchor outside vault' error, got %+v", st.BridgeErrors)
	}
	if _, ok := st.WrittenBridges["decisions"]; ok {
		t.Error("escaping anchor should not be recorded as written")
	}
}

// #5: dropping back to a v1 layout strips a block a prior v2 sync wrote,
// rather than orphaning it — write is disabled but strip still runs.
func TestSyncBridgesV1StripsPriorBlock(t *testing.T) {
	exp, vaultDir, statePath := newBridgeExporter(t, map[string]string{
		"decisions": "A.md",
	})
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("v2 Sync: %v", err)
	}
	anchor := filepath.Join(vaultDir, "A.md")
	if !strings.Contains(string(mustRead(t, anchor)), bridgeStartDelim("decisions")) {
		t.Fatal("v2 sync did not write the block")
	}

	s := storetest.NewStore(t, t.TempDir())
	s.IngestAll()
	exp2, err := New(s, vaultDir, Options{
		Layout:    store.VaultLayoutV1,
		StatePath: statePath,
		Bridges:   map[string]string{"decisions": "A.md"},
	})
	if err != nil {
		t.Fatalf("New2: %v", err)
	}
	if err := exp2.Sync(context.Background()); err != nil {
		t.Fatalf("v1 Sync: %v", err)
	}
	if strings.Contains(string(mustRead(t, anchor)), "mnemo:bridge:decisions") {
		t.Error("v1 sync did not strip the prior bridge block")
	}
	st, _ := store.LoadState(statePath)
	if _, ok := st.WrittenBridges["decisions"]; ok {
		t.Error("v1 sync left the bridge tracked in WrittenBridges")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
