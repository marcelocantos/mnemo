// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

// ---- slugify ----------------------------------------------------------------

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"vault feature implementation", "vault-feature-implementation"},
		{"  leading and trailing  ", "leading-and-trailing"},
		{"special!@#$chars", "special-chars"},
		{"already-slug", "already-slug"},
		{"", "untitled"},
		{strings.Repeat("a", 100), strings.Repeat("a", 60)},
		{"ends-with-special!!!!!", "ends-with-special"},
	}
	for _, c := range cases {
		got := slugify(c.in)
		if got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- dateOf -----------------------------------------------------------------

func TestDateOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-05-10T14:23:45Z", "2026-05-10"},
		{"2026-05-10T14:23:45.123Z", "2026-05-10"},
		{"2026-05-10", "2026-05-10"},
	}
	for _, c := range cases {
		got := dateOf(c.in)
		if got != c.want {
			t.Errorf("dateOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDateOfEmpty(t *testing.T) {
	// Empty input returns today's date; just check it's YYYY-MM-DD shaped.
	got := dateOf("")
	if len(got) != 10 || got[4] != '-' || got[7] != '-' {
		t.Errorf("dateOf(\"\") = %q, want YYYY-MM-DD", got)
	}
}

// ---- shortID ----------------------------------------------------------------

func TestShortID(t *testing.T) {
	if got := shortID("abc123def456"); got != "abc123de" {
		t.Errorf("shortID long: %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID short: %q", got)
	}
}

// ---- shortProjectName -------------------------------------------------------

func TestShortProjectName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/navnita/dev/mnemo", "mnemo"},
		{"/Users/navnita/dev/riot-mind", "riot-mind"},
		{"-Users-navnita-dev-mnemo", "mnemo"},
		{"-Users-navnita-dev-thittam", "thittam"},
		{"-Users-navnita-documents-writing-the-apothecary-diaries", "writing-the-apothecary-diaries"},
		{"-Users-alice-work-myapp", "myapp"},
		{"-Users-navnita", "users-navnita"}, // root home project, no stripping
		{"simple-name", "simple-name"},
	}
	for _, c := range cases {
		got := shortProjectName(c.in)
		if got != c.want {
			t.Errorf("shortProjectName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- path generation --------------------------------------------------------

func TestSessionPath(t *testing.T) {
	info := store.SessionInfo{
		SessionID: "abc123def456789x",
		Repo:      "mnemo",
		Topic:     "vault feature",
		FirstMsg:  "2026-05-10T14:23:45Z",
	}
	got := sessionPath(info)
	if !strings.HasPrefix(got, "sessions/mnemo/") {
		t.Errorf("expected sessions/mnemo/ prefix, got %q", got)
	}
	if !strings.Contains(got, "2026-05-10") {
		t.Errorf("expected date in path, got %q", got)
	}
	if !strings.Contains(got, "vault-feature") {
		t.Errorf("expected topic slug in path, got %q", got)
	}
	if !strings.Contains(got, "abc123de") {
		t.Errorf("expected short session ID in path, got %q", got)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Errorf("expected .md suffix, got %q", got)
	}
}

func TestSessionPathNoRepo(t *testing.T) {
	info := store.SessionInfo{
		SessionID: "abc123def456789x",
		Project:   "-Users-alice-dev-myapp",
		FirstMsg:  "2026-05-10T14:23:45Z",
	}
	got := sessionPath(info)
	if !strings.HasPrefix(got, "sessions/") {
		t.Errorf("expected sessions/ prefix, got %q", got)
	}
}

func TestDecisionPath(t *testing.T) {
	d := store.DecisionInfo{
		SessionID: "sess1234abcd",
		Repo:      "my-repo",
		Timestamp: "2026-05-10T10:00:00Z",
	}
	got := decisionPath(d)
	if !strings.HasPrefix(got, "decisions/my-repo/") {
		t.Errorf("expected decisions/my-repo/ prefix, got %q", got)
	}
	if !strings.Contains(got, "2026-05-10") {
		t.Errorf("expected date, got %q", got)
	}
	if !strings.Contains(got, "sess1234") {
		t.Errorf("expected session ID prefix, got %q", got)
	}
}

func TestMemoryPath(t *testing.T) {
	m := store.MemoryInfo{
		Project: "-Users-alice-dev-myapp",
		Name:    "My Memory",
	}
	got := memoryPath(m)
	if !strings.HasPrefix(got, "memories/") {
		t.Errorf("expected memories/ prefix, got %q", got)
	}
	if !strings.Contains(got, "my-memory") {
		t.Errorf("expected name slug, got %q", got)
	}
}

func TestRepoIndexPath(t *testing.T) {
	got := repoIndexPath("My Repo")
	if got != filepath.Join("repos", "my-repo.md") {
		t.Errorf("repoIndexPath = %q, want repos/my-repo.md", got)
	}
}

// ---- writeNote fence preservation -------------------------------------------

func TestWriteNoteFencePreservesHumanContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")

	// Initial write.
	if err := writeNote(path, "# Generated\n\nGenerated v1."); err != nil {
		t.Fatalf("initial writeNote: %v", err)
	}

	// Simulate human adding content below the fence.
	raw, _ := os.ReadFile(path)
	withHuman := string(raw) + "\n## My notes\n\nHuman wrote this.\n"
	os.WriteFile(path, []byte(withHuman), 0o644)

	// Re-sync with updated generated content.
	if err := writeNote(path, "# Generated\n\nGenerated v2."); err != nil {
		t.Fatalf("re-sync writeNote: %v", err)
	}

	final, _ := os.ReadFile(path)
	s := string(final)

	if !strings.Contains(s, "Generated v2") {
		t.Error("re-sync should update generated content")
	}
	if strings.Contains(s, "Generated v1") {
		t.Error("old generated content should be replaced")
	}
	if !strings.Contains(s, "Human wrote this") {
		t.Error("re-sync should preserve human content")
	}
	if !strings.Contains(s, generatedFence) {
		t.Error("fence marker must be present")
	}

	// Human content must appear after the fence.
	fenceIdx := strings.Index(s, generatedFence)
	humanIdx := strings.Index(s, "Human wrote this")
	if humanIdx < fenceIdx {
		t.Error("human content must appear after the fence marker")
	}
}

// TestWriteNoteFenceNoCascade verifies that repeated re-syncs without human
// edits do not accumulate multiple fence markers (the stacked-fence bug).
func TestWriteNoteFenceNoCascade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")

	for i := 0; i < 3; i++ {
		if err := writeNote(path, "# Generated"); err != nil {
			t.Fatalf("writeNote run %d: %v", i, err)
		}
	}

	raw, _ := os.ReadFile(path)
	s := string(raw)
	count := strings.Count(s, generatedFence)
	if count != 1 {
		t.Errorf("expected exactly 1 fence after 3 syncs, got %d\ncontent:\n%s", count, s)
	}
}

func TestWriteNoteNoFenceNoHuman(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")

	if err := writeNote(path, "# Title\n\nContent."); err != nil {
		t.Fatalf("writeNote: %v", err)
	}

	raw, _ := os.ReadFile(path)
	s := string(raw)
	if !strings.Contains(s, "# Title") {
		t.Error("title missing")
	}
	if !strings.Contains(s, generatedFence) {
		t.Error("fence must always be written")
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("file must end with newline")
	}
}

func TestWriteNoteCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "note.md")
	if err := writeNote(path, "hello"); err != nil {
		t.Fatalf("writeNote with deep path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// ---- renderSession ----------------------------------------------------------

func TestRenderSessionFrontmatter(t *testing.T) {
	info := store.SessionInfo{
		SessionID: "abc123def456",
		Repo:      "mnemo",
		Topic:     "test session",
		WorkType:  "feature",
		FirstMsg:  "2026-05-10T10:00:00Z",
		LastMsg:   "2026-05-10T11:00:00Z",
	}
	msgs := []store.SessionMessage{
		{Role: "user", Text: "Hello", Timestamp: "2026-05-10T10:00:00Z"},
		{Role: "assistant", Text: "Hi there", Timestamp: "2026-05-10T10:00:01Z"},
	}
	note := renderSession(info, msgs)

	checks := []struct {
		want string
		desc string
	}{
		{"session_id: abc123def456", "session_id in frontmatter"},
		{"repo: mnemo", "repo in frontmatter"},
		{"work_type: feature", "work_type in frontmatter"},
		{"- session\n", "session tag"},
		{"- feature\n", "feature tag"},
		{"# test session\n", "title heading"},
		{"[[repos/mnemo]]", "repo wikilink"},
		{"**Human**", "Human role marker"},
		{"**Claude**", "Claude role marker"},
		{"Hello", "user message text"},
		{"Hi there", "assistant message text"},
	}
	for _, c := range checks {
		if !strings.Contains(note, c.want) {
			t.Errorf("renderSession: missing %s (%q not found)", c.desc, c.want)
		}
	}
}

func TestRenderSessionNoiseFiltered(t *testing.T) {
	info := store.SessionInfo{
		SessionID: "abc123",
		FirstMsg:  "2026-05-10T10:00:00Z",
	}
	msgs := []store.SessionMessage{
		{Role: "user", Text: "Real message", IsNoise: false},
		{Role: "user", Text: "Noise message", IsNoise: true},
	}
	note := renderSession(info, msgs)
	if strings.Contains(note, "Noise message") {
		t.Error("noise messages should be filtered from session note")
	}
	if !strings.Contains(note, "Real message") {
		t.Error("real messages must appear in note")
	}
}

// ---- renderDecision ---------------------------------------------------------

func TestRenderDecision(t *testing.T) {
	d := store.DecisionInfo{
		SessionID:        "sess9999",
		Repo:             "myrepo",
		Timestamp:        "2026-05-10T12:00:00Z",
		ProposalText:     "We should use FTS5.",
		ConfirmationText: "Agreed, FTS5 it is.",
	}
	relPath := "sessions/myrepo/2026-05-10-session-sess9999.md"
	note := renderDecision(d, relPath)

	if !strings.Contains(note, "session_id: sess9999") {
		t.Error("missing session_id")
	}
	if !strings.Contains(note, "# Decision") {
		t.Error("missing decision title")
	}
	if !strings.Contains(note, "[[sessions/myrepo/2026-05-10-session-sess9999]]") {
		t.Error("missing session wikilink")
	}
	if !strings.Contains(note, "We should use FTS5") {
		t.Error("missing proposal text")
	}
	if !strings.Contains(note, "Agreed, FTS5 it is") {
		t.Error("missing confirmation text")
	}
}

// ---- renderRootIndex --------------------------------------------------------

func TestRenderRootIndex(t *testing.T) {
	repos := []store.RepoInfo{
		{Repo: "mnemo", Sessions: 42, LastActivity: "2026-05-10"},
		{Repo: "other-repo", Sessions: 10, LastActivity: "2026-05-09"},
	}
	note := renderRootIndex(repos, nil)

	if !strings.Contains(note, "[[repos/mnemo]]") {
		t.Error("missing mnemo repo wikilink")
	}
	if !strings.Contains(note, "[[repos/other-repo]]") {
		t.Error("missing other-repo wikilink")
	}
	if !strings.Contains(note, "42 sessions") {
		t.Error("missing session count")
	}
}

// ---- writeYAML --------------------------------------------------------------

func TestWriteYAMLQuoting(t *testing.T) {
	var b strings.Builder
	writeYAML(&b, "key", "value: with colon")
	got := b.String()
	if !strings.Contains(got, `"value: with colon"`) {
		t.Errorf("expected quoted value, got %q", got)
	}
}

func TestWriteYAMLEmpty(t *testing.T) {
	var b strings.Builder
	writeYAML(&b, "key", "")
	if b.String() != "" {
		t.Errorf("empty value should produce no output, got %q", b.String())
	}
}

// ---- summarize --------------------------------------------------------------

func TestSummarize(t *testing.T) {
	if got := summarize("short", 80); got != "short" {
		t.Errorf("short string changed: %q", got)
	}
	long := strings.Repeat("word ", 20)
	s := summarize(long, 20)
	if len(s) > 23 { // 20 + "..."
		t.Errorf("summarize exceeded maxLen: %q", s)
	}
	if !strings.HasSuffix(s, "...") {
		t.Errorf("summarize should end with ellipsis: %q", s)
	}
}

// ---- needsUpdate ------------------------------------------------------------

func TestNeedsUpdateNoFile(t *testing.T) {
	if !needsUpdate("/nonexistent/path/file.md", "2026-05-10T10:00:00Z") {
		t.Error("missing file should need update")
	}
}

func TestNeedsUpdateEmptyTS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	os.WriteFile(path, []byte("x"), 0o644)
	if !needsUpdate(path, "") {
		t.Error("empty timestamp should always need update")
	}
}

func TestNeedsUpdateFreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	os.WriteFile(path, []byte("x"), 0o644)
	// File was just created; any past timestamp should not need update.
	if needsUpdate(path, "2020-01-01T00:00:00Z") {
		t.Error("fresh file with old timestamp should not need update")
	}
}

// ---- skillPath / configPath -------------------------------------------------

func TestSkillPath(t *testing.T) {
	s := store.SkillInfo{Name: "My Skill", FilePath: "/home/user/.claude/skills/my-skill.md"}
	got := skillPath(s)
	if got != filepath.Join("skills", "my-skill.md") {
		t.Errorf("skillPath = %q, want skills/my-skill.md", got)
	}
}

func TestConfigPath(t *testing.T) {
	c := store.ClaudeConfigInfo{Repo: "-Users-alice-dev-myapp", FilePath: "/Users/alice/dev/myapp/CLAUDE.md"}
	got := configPath(c)
	if got != filepath.Join("configs", "myapp.md") {
		t.Errorf("configPath = %q, want configs/myapp.md", got)
	}
}

// ---- renderSkill / renderConfig ---------------------------------------------

func TestRenderSkill(t *testing.T) {
	s := store.SkillInfo{
		Name:        "commit",
		FilePath:    "/home/user/.claude/skills/commit.md",
		Description: "Generate conventional commit messages",
		Content:     "## Usage\n\nRun /commit to generate a commit message.",
		UpdatedAt:   "2026-05-10T10:00:00Z",
	}
	note := renderSkill(s)
	checks := []struct{ want, desc string }{
		{"name: commit", "name in frontmatter"},
		{"- skill", "skill tag"},
		{"# Skill: commit", "title heading"},
		{"Generate conventional commit messages", "description"},
		{"## Usage", "content included"},
	}
	for _, c := range checks {
		if !strings.Contains(note, c.want) {
			t.Errorf("renderSkill: missing %s (%q not found)", c.desc, c.want)
		}
	}
}

func TestRenderConfig(t *testing.T) {
	c := store.ClaudeConfigInfo{
		Repo:      "mnemo",
		FilePath:  "/Users/alice/dev/mnemo/CLAUDE.md",
		Content:   "# mnemo\n\nBuild with go build -tags sqlite_fts5.",
		UpdatedAt: "2026-05-10T10:00:00Z",
	}
	note := renderConfig(c)
	checks := []struct{ want, desc string }{
		{"repo: mnemo", "repo in frontmatter"},
		{"- config", "config tag"},
		{"- claude-md", "claude-md tag"},
		{"# CLAUDE.md: mnemo", "title heading"},
		{"[[repos/mnemo]]", "repo wikilink"},
		{"go build -tags sqlite_fts5", "content included"},
	}
	for _, c := range checks {
		if !strings.Contains(note, c.want) {
			t.Errorf("renderConfig: missing %s (%q not found)", c.desc, c.want)
		}
	}
}

// ---- session aliases --------------------------------------------------------

func TestRenderSessionAliases(t *testing.T) {
	info := store.SessionInfo{
		SessionID: "abc123def456",
		Topic:     "my feature topic",
		FirstMsg:  "2026-05-10T10:00:00Z",
	}
	note := renderSession(info, nil)
	if !strings.Contains(note, "aliases:") {
		t.Error("session note missing aliases frontmatter")
	}
	if !strings.Contains(note, `"my feature topic"`) {
		t.Error("session note missing topic alias")
	}
	if !strings.Contains(note, `"abc123de"`) {
		t.Error("session note missing short-ID alias")
	}
}

// ---- Exporter.Sync integration ----------------------------------------------

// TestExporterSyncCreatesFiles verifies that Sync writes the expected vault
// files when the store has been populated with a minimal transcript fixture.
func TestExporterSyncCreatesFiles(t *testing.T) {
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-vault-01", []map[string]any{
		storetest.MetaMsg("user", "hello vault", "2026-05-10T10:00:00Z",
			"/Users/alice/dev/myapp", "main"),
		storetest.Msg("assistant", "vault response", "2026-05-10T10:00:01Z"),
	})

	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	vaultDir := t.TempDir()
	exp, err := New(s, vaultDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Root index must exist.
	if _, err := os.Stat(filepath.Join(vaultDir, "index.md")); err != nil {
		t.Errorf("index.md missing: %v", err)
	}
	// Session note must exist under sessions/<repo-short>/.
	sessions, _ := filepath.Glob(filepath.Join(vaultDir, "sessions", "*", "*.md"))
	if len(sessions) == 0 {
		t.Error("expected at least one session note")
	}
	// Repo index note must exist.
	repos, _ := filepath.Glob(filepath.Join(vaultDir, "repos", "*.md"))
	if len(repos) == 0 {
		t.Error("expected at least one repo index note")
	}
	// Second Sync must be idempotent (all skipped, no errors).
	if err := exp.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	// After idempotent re-sync, fence must appear exactly once per file.
	for _, p := range sessions {
		raw, _ := os.ReadFile(p)
		if c := strings.Count(string(raw), generatedFence); c != 1 {
			t.Errorf("%s: expected 1 fence, got %d", p, c)
		}
	}
}
