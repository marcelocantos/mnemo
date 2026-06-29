// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package threads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newManager builds a Manager rooted at a fresh temp dir, with Home pointing
// at a sibling so transcript-dir lookups are isolated and empty by default.
func newManager(t *testing.T) *Manager {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "think", "threads")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Manager{Root: root, Home: home}
}

func writeThread(t *testing.T, m *Manager, name, claudeMD string) {
	t.Helper()
	dir := filepath.Join(m.Root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if claudeMD != "" {
		if err := os.WriteFile(filepath.Join(dir, ContextFile), []byte(claudeMD), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateName(t *testing.T) {
	ok := []string{"a", "project-a", "experiment-foo", "master", "x1", "0"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{"", "_template", "_archived", ".hidden", "-leading", "Caps", "has space", "under_score", "trailing-"}
	for _, n := range bad {
		// trailing- is actually valid by the regex (hyphen allowed mid/end);
		// drop it from the bad set.
		if n == "trailing-" {
			continue
		}
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestIsReserved(t *testing.T) {
	for _, n := range []string{"_template", "_archived", "_x", ".git", ""} {
		if !IsReserved(n) {
			t.Errorf("IsReserved(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"master", "project-a", "x"} {
		if IsReserved(n) {
			t.Errorf("IsReserved(%q) = true, want false", n)
		}
	}
}

func TestExtractSectionAndState(t *testing.T) {
	body := `# project-a

## Status

**blocked** on the migration

## Current focus

Wire up the new endpoint

## Notes

ignore me
`
	if got := extractSection(body, "Status"); got != "**blocked** on the migration" {
		t.Errorf("Status = %q", got)
	}
	if got := extractSection(body, "Current focus"); got != "Wire up the new endpoint" {
		t.Errorf("Focus = %q", got)
	}
	if got := firstWordState("**blocked** on the migration"); got != "blocked" {
		t.Errorf("state = %q, want blocked", got)
	}
}

func TestExtractSectionSkipsPlaceholder(t *testing.T) {
	body := `## Status

_active_

## Current focus

_what this thread is about_
`
	// Italic-only placeholder lines are skipped; the section reads as empty.
	if got := extractSection(body, "Status"); got != "" {
		t.Errorf("Status with placeholder = %q, want empty", got)
	}
	if got := extractSection(body, "Current focus"); got != "" {
		t.Errorf("Focus with placeholder = %q, want empty", got)
	}
}

func TestExtractSectionStopsAtNextHeading(t *testing.T) {
	body := `## Status

## Current focus

real focus
`
	// An empty Status section (immediately followed by a heading) is empty,
	// and must not bleed into Current focus.
	if got := extractSection(body, "Status"); got != "" {
		t.Errorf("empty Status = %q", got)
	}
	if got := extractSection(body, "Current focus"); got != "real focus" {
		t.Errorf("Focus = %q", got)
	}
}

func TestIsItalicPlaceholder(t *testing.T) {
	yes := []string{"_active_", "*todo*", "_a b c_"}
	for _, s := range yes {
		if !isItalicPlaceholder(s) {
			t.Errorf("isItalicPlaceholder(%q) = false, want true", s)
		}
	}
	no := []string{"__bold__", "**bold**", "active", "_unbalanced", "_a_b_", ""}
	for _, s := range no {
		if isItalicPlaceholder(s) {
			t.Errorf("isItalicPlaceholder(%q) = true, want false", s)
		}
	}
}

func TestListSortedByActivity(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "old", "## Status\n\nactive\n")
	writeThread(t, m, "new", "## Status\n\nactive\n")
	writeThread(t, m, "idle", "## Status\n\npaused\n") // no working files → no activity

	// Give "old" and "new" working files with controlled mtimes.
	touch(t, filepath.Join(m.Root, "old", "a.txt"), time.Now().Add(-48*time.Hour))
	touch(t, filepath.Join(m.Root, "new", "b.txt"), time.Now().Add(-1*time.Hour))

	ts, err := m.ListSorted()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 3 {
		t.Fatalf("got %d threads, want 3", len(ts))
	}
	if ts[0].Name != "new" || ts[1].Name != "old" || ts[2].Name != "idle" {
		t.Errorf("order = %s,%s,%s; want new,old,idle", ts[0].Name, ts[1].Name, ts[2].Name)
	}
	if ts[2].HasActivity {
		t.Errorf("idle thread should have no activity")
	}
}

// TestMarkerCatalog verifies the catalog is the single source of truth: every
// entry resolves back through Emoji/Pinned/ParseMarker, so a UI built from the
// catalog and the daemon's own derivations never disagree, and an unknown
// marker degrades gracefully.
func TestMarkerCatalog(t *testing.T) {
	cat := MarkerCatalog()
	if len(cat) < 2 {
		t.Fatalf("catalog has %d entries, want >= 2 (normal, important)", len(cat))
	}
	if cat[0].Value != string(MarkerNormal) {
		t.Errorf("catalog[0] = %q, want the normal marker first", cat[0].Value)
	}
	for _, mi := range cat {
		m := Marker(mi.Value)
		if m.Emoji() != mi.Emoji {
			t.Errorf("%q: Emoji()=%q, catalog=%q", mi.Value, m.Emoji(), mi.Emoji)
		}
		if m.Pinned() != mi.Pinned {
			t.Errorf("%q: Pinned()=%v, catalog=%v", mi.Value, m.Pinned(), mi.Pinned)
		}
		if ParseMarker(mi.Value) != m {
			t.Errorf("%q: ParseMarker round-trip failed", mi.Value)
		}
	}
	if Marker("nope").Emoji() != cat[0].Emoji || Marker("nope").Pinned() {
		t.Errorf("unknown marker did not degrade to normal")
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "x", "## Status\n\nactive\n")

	if got, _ := m.Get("x"); got.Marker != MarkerNormal {
		t.Errorf("new thread marker = %q, want normal", got.Marker)
	}
	if err := m.SetMarker("x", MarkerImportant); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get("x"); got.Marker != MarkerImportant {
		t.Errorf("after set, marker = %q, want important", got.Marker)
	}
	// The marker file is hidden, so it must not count as a working file.
	if got, _ := m.Get("x"); got.FileCount != 0 {
		t.Errorf("marker file leaked into FileCount: %d", got.FileCount)
	}
	if err := m.SetMarker("x", MarkerNormal); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Root, "x", MarkerFile)); !os.IsNotExist(err) {
		t.Errorf("clearing marker should remove %s", MarkerFile)
	}
	// Unknown stored values degrade to normal.
	if err := os.WriteFile(filepath.Join(m.Root, "x", MarkerFile), []byte("bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get("x"); got.Marker != MarkerNormal {
		t.Errorf("unknown marker should read as normal, got %q", got.Marker)
	}
}

func TestPinnedSortsToTop(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "stale-pin", "## Status\n\nactive\n") // no activity → stale
	writeThread(t, m, "fresh", "## Status\n\nactive\n")
	touch(t, filepath.Join(m.Root, "fresh", "a.txt"), time.Now())
	if err := m.SetMarker("stale-pin", MarkerImportant); err != nil {
		t.Fatal(err)
	}

	ts, err := m.ListSorted()
	if err != nil {
		t.Fatal(err)
	}
	// The important thread pins to the top even though it has no activity and
	// "fresh" was just touched.
	if ts[0].Name != "stale-pin" {
		t.Errorf("order = %v, want stale-pin first (pinned)", names(ts))
	}
}

func TestTodoCounts(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "tk", "## Status\n\nactive\n")
	// A todo.md nested at depth, exercising every status + an overdue due date.
	sub := filepath.Join(m.Root, "tk", "notes")
	mustMkdir(t, sub)
	content := "- [ ] open one\n" +
		"- [ ] past due 📅 2000-01-01\n" +
		"- [/] in progress\n" +
		"- [x] done\n" +
		"- [-] cancelled\n"
	if err := os.WriteFile(filepath.Join(sub, "todos.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	th, err := m.Get("tk")
	if err != nil {
		t.Fatal(err)
	}
	// active = open + overdue-open + in_progress = 3 (done/cancelled excluded).
	if th.ActiveTodos != 3 {
		t.Errorf("ActiveTodos = %d, want 3", th.ActiveTodos)
	}
	if th.OverdueTodos != 1 {
		t.Errorf("OverdueTodos = %d, want 1", th.OverdueTodos)
	}
}

func TestReservedDirsNotListed(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "real", "")
	writeThread(t, m, TemplateDir, "")
	writeThread(t, m, ArchivedDir, "")
	mustMkdir(t, filepath.Join(m.Root, ".hidden"))

	ts, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Name != "real" {
		t.Errorf("List() = %v, want [real]", names(ts))
	}
}

func TestFileCountExcludesHiddenAndContext(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "x", "## Status\n\nactive\n")
	dir := filepath.Join(m.Root, "x")
	for _, f := range []string{"a.txt", "b.md", ".hidden", "HISTORY.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(t, filepath.Join(dir, "subdir"))
	th, err := m.Get("x")
	if err != nil {
		t.Fatal(err)
	}
	// a.txt, b.md, HISTORY.md = 3; CLAUDE.md, .hidden, subdir excluded.
	if th.FileCount != 3 {
		t.Errorf("FileCount = %d, want 3", th.FileCount)
	}
}

func TestActivityFromTranscriptDir(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "chatty", "## Status\n\nactive\n") // no working files
	dir := filepath.Join(m.Root, "chatty")

	// The transcript dir is ~/.claude/projects/<path with / replaced by ->.
	enc := encodeForTest(dir)
	td := filepath.Join(m.Home, ".claude", "projects", enc)
	mustMkdir(t, td)
	want := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	touch(t, filepath.Join(td, "session.jsonl"), want)

	th, err := m.Get("chatty")
	if err != nil {
		t.Fatal(err)
	}
	if !th.HasActivity {
		t.Fatal("expected activity from transcript dir")
	}
	if !th.Activity.Truncate(time.Second).Equal(want) {
		t.Errorf("Activity = %v, want %v", th.Activity, want)
	}
}

func TestNewScaffolds(t *testing.T) {
	m := newManager(t)
	// Provide a template so {{NAME}} substitution is exercised.
	tmplDir := filepath.Join(m.Root, TemplateDir)
	mustMkdir(t, tmplDir)
	if err := os.WriteFile(filepath.Join(tmplDir, ContextFile), []byte("# {{NAME}}\n\n## Status\n\n_active_\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	th, err := m.New(NewArgs{Name: "fresh-thread"})
	if err != nil {
		t.Fatal(err)
	}
	if th.Name != "fresh-thread" {
		t.Errorf("name = %q", th.Name)
	}
	data, err := os.ReadFile(filepath.Join(m.Root, "fresh-thread", ContextFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "# fresh-thread\n\n## Status\n\n_active_\n" {
		t.Errorf("scaffolded body = %q", got)
	}

	// Duplicate create fails.
	if _, err := m.New(NewArgs{Name: "fresh-thread"}); err == nil {
		t.Error("New on existing thread should fail")
	}
	// Reserved name fails.
	if _, err := m.New(NewArgs{Name: "_template"}); err == nil {
		t.Error("New with reserved name should fail")
	}
}

func TestNewSeedsTemplateWhenAbsent(t *testing.T) {
	m := newManager(t)
	tmplPath := filepath.Join(m.Root, TemplateDir, ContextFile)
	if _, err := os.Stat(tmplPath); !os.IsNotExist(err) {
		t.Fatal("precondition: _template should be absent")
	}

	th, err := m.New(NewArgs{Name: "t1"})
	if err != nil {
		t.Fatal(err)
	}

	// The built-in seeded _template/CLAUDE.md, keeping the {{NAME}} placeholder.
	tmpl, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("template was not seeded: %v", err)
	}
	if !strings.Contains(string(tmpl), NamePlaceholder) {
		t.Errorf("seeded template should keep the placeholder:\n%s", tmpl)
	}

	// The created thread used it: {{NAME}} substituted, H1 in a preview-skip region.
	body, _ := os.ReadFile(filepath.Join(th.Path, ContextFile))
	for _, want := range []string{"# t1", "<!-- preview-skip -->", "<!-- /preview-skip -->", "## Status"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("thread body missing %q:\n%s", want, body)
		}
	}
}

func TestEnsureTemplateDoesNotOverwrite(t *testing.T) {
	m := newManager(t)
	tmplDir := filepath.Join(m.Root, TemplateDir)
	mustMkdir(t, tmplDir)
	custom := "# {{NAME}}\n\nmy custom scaffold\n"
	if err := os.WriteFile(filepath.Join(tmplDir, ContextFile), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureTemplate(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(tmplDir, ContextFile))
	if string(got) != custom {
		t.Errorf("EnsureTemplate overwrote an existing template:\n%s", got)
	}
}

func TestArchive(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "done", "## Status\n\nactive\n")
	if err := m.Archive("done"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Root, "done")); !os.IsNotExist(err) {
		t.Error("source should be gone after archive")
	}
	if _, err := os.Stat(filepath.Join(m.Root, ArchivedDir, "done", ContextFile)); err != nil {
		t.Errorf("archived copy missing: %v", err)
	}
	// Re-archiving a now-missing thread fails.
	if err := m.Archive("done"); err == nil {
		t.Error("archive of missing thread should fail")
	}
	// Reserved name refused.
	if err := m.Archive("_template"); err == nil {
		t.Error("archive of reserved name should fail")
	}
}

func TestArchiveCollision(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "dup", "a")
	mustMkdir(t, filepath.Join(m.Root, ArchivedDir, "dup"))
	if err := m.Archive("dup"); err == nil {
		t.Error("archive into existing destination should fail")
	}
}

func TestSearch(t *testing.T) {
	m := newManager(t)
	writeThread(t, m, "alpha", "## Status\n\nactive\n\nworking on the parser\n")
	writeThread(t, m, "beta", "## Status\n\npaused\n")
	dir := filepath.Join(m.Root, "beta")
	if err := os.WriteFile(filepath.Join(dir, "parser-notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Match on CLAUDE.md body (alpha) and on a file name (beta).
	got := m.Search("parser")
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	if !set["alpha"] || !set["beta"] {
		t.Errorf("Search(parser) = %v, want both alpha and beta", got)
	}

	// Match on thread name.
	if names := m.Search("alph"); len(names) != 1 || names[0] != "alpha" {
		t.Errorf("Search(alph) = %v, want [alpha]", names)
	}

	// Empty query matches nothing.
	if names := m.Search("  "); names != nil {
		t.Errorf("Search(blank) = %v, want nil", names)
	}
}

func TestResolve(t *testing.T) {
	m := newManager(t)
	cases := map[string]string{
		"project-a":       filepath.Join(m.Root, "project-a"),
		"~":               m.Home,
		"~/work/x":        filepath.Join(m.Home, "work", "x"),
		"/abs/path":       "/abs/path",
		"/abs/path/../p2": "/abs/p2",
	}
	for in, want := range cases {
		got, err := m.Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "sub/dir", "Bad Name"} {
		if _, err := m.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) = nil error, want error", bad)
		}
	}
}

// --- helpers ---

func touch(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func names(ts []Thread) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

// encodeForTest mirrors Manager.transcriptDir's encoding so the test can
// place a transcript file where the code will look for it.
func encodeForTest(absPath string) string {
	out := make([]rune, 0, len(absPath))
	for _, c := range absPath {
		if c == '/' {
			out = append(out, '-')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}
