// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"errors"
	"fmt"
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
	if err := writeNote(path, "# Generated\n\nGenerated v1.", ""); err != nil {
		t.Fatalf("initial writeNote: %v", err)
	}

	// Simulate human adding content below the fence.
	raw, _ := os.ReadFile(path)
	withHuman := string(raw) + "\n## My notes\n\nHuman wrote this.\n"
	os.WriteFile(path, []byte(withHuman), 0o644)

	// Re-sync with updated generated content.
	if err := writeNote(path, "# Generated\n\nGenerated v2.", ""); err != nil {
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
		if err := writeNote(path, "# Generated", ""); err != nil {
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

	if err := writeNote(path, "# Title\n\nContent.", ""); err != nil {
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
	if err := writeNote(path, "hello", ""); err != nil {
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
		{"[[repos/mnemo", "repo wikilink"},
		{"### Human", "Human role marker"},
		{"### Claude", "Claude role marker"},
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

	if !strings.Contains(note, "[[repos/mnemo") {
		t.Error("missing mnemo repo wikilink")
	}
	if !strings.Contains(note, "[[repos/other-repo") {
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
	const ts = "2020-01-01T00:00:00Z"
	// Write via writeNote so the entity_ts comment is embedded.
	if err := writeNote(path, "content", ts); err != nil {
		t.Fatalf("writeNote: %v", err)
	}
	// Same timestamp → no update needed.
	if needsUpdate(path, ts) {
		t.Error("file with same entity timestamp should not need update")
	}
	// Older entity timestamp → no update needed.
	if needsUpdate(path, "2019-01-01T00:00:00Z") {
		t.Error("file with newer recorded timestamp should not need update")
	}
	// Newer entity timestamp → update needed.
	if !needsUpdate(path, "2026-01-01T00:00:00Z") {
		t.Error("file with older recorded timestamp should need update")
	}
}

func TestNeedsUpdateHumanEditDoesNotSuppressRegeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	const entityTS = "2020-01-01T00:00:00Z"
	if err := writeNote(path, "generated", entityTS); err != nil {
		t.Fatalf("writeNote: %v", err)
	}
	// Simulate human touching the file — bump mtime to now.
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, raw, 0o644)

	// Entity timestamp recorded in file is still 2020; a newer entity_ts
	// must trigger regeneration regardless of file mtime.
	if !needsUpdate(path, "2026-01-01T00:00:00Z") {
		t.Error("newer entity timestamp should trigger update even after human edit")
	}
	// But same entity timestamp must NOT trigger regeneration.
	if needsUpdate(path, entityTS) {
		t.Error("same entity timestamp should not trigger update after human edit")
	}
}

func TestWriteNotePreservesPreExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.md")
	// Pre-existing file with no fence (e.g. user's own Obsidian note).
	preExisting := "# My Notes\n\nThis is my content.\n"
	os.WriteFile(path, []byte(preExisting), 0o644)

	if err := writeNote(path, "# Generated", ""); err != nil {
		t.Fatalf("writeNote: %v", err)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)
	if !strings.Contains(s, "My Notes") {
		t.Error("pre-existing content must be preserved as human content")
	}
	if !strings.Contains(s, "This is my content") {
		t.Error("pre-existing content body must survive writeNote")
	}
	if !strings.Contains(s, "# Generated") {
		t.Error("generated content must be written")
	}
	if !strings.Contains(s, generatedFence) {
		t.Error("fence must be present")
	}
	// Pre-existing content must appear after the fence.
	fenceIdx := strings.Index(s, generatedFence)
	userIdx := strings.Index(s, "My Notes")
	if userIdx < fenceIdx {
		t.Error("pre-existing content must appear after fence")
	}
}

// TestWriteNoteRepackagesPreExistingFrontmatter verifies bug B fix:
// a pre-existing user file whose content begins with its own YAML
// frontmatter block must NOT produce a double --- fence below mnemo's
// generated frontmatter. Obsidian only treats the first --- pair as
// frontmatter and renders the second as literal text, which is ugly.
// The user's frontmatter is preserved as a yaml code block under a
// "Preserved frontmatter" heading.
func TestWriteNoteRepackagesPreExistingFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.md")
	preExisting := "---\ntitle: User's note\ntags: [todo]\n---\n\n# My Notes\n\nBody.\n"
	if err := os.WriteFile(path, []byte(preExisting), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	generated := "---\nrepo: mnemo\n---\n\n# Generated\n"
	if err := writeNote(path, generated, ""); err != nil {
		t.Fatalf("writeNote: %v", err)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)

	// Exactly one frontmatter fence pair: mnemo's. The user's --- pair
	// must have been repackaged.
	if got := strings.Count(s, "\n---\n"); got > 1 {
		// One leading "---\n" + one closing "\n---\n" expected from
		// mnemo's frontmatter only.
		t.Errorf("expected single frontmatter block, got %d \"---\" lines in:\n%s", got, s)
	}

	// User's body must be preserved.
	if !strings.Contains(s, "# My Notes") || !strings.Contains(s, "Body.") {
		t.Error("user body must survive repackaging")
	}

	// User's frontmatter must appear in a yaml code block.
	if !strings.Contains(s, "## Preserved frontmatter") {
		t.Error("missing 'Preserved frontmatter' section heading")
	}
	if !strings.Contains(s, "```yaml\ntitle: User's note") {
		t.Errorf("user frontmatter must be in a yaml code block, got:\n%s", s)
	}

	// All of it must appear AFTER the generated fence.
	fenceIdx := strings.Index(s, generatedFence)
	preservedIdx := strings.Index(s, "## Preserved frontmatter")
	if preservedIdx < fenceIdx {
		t.Error("preserved frontmatter must appear after the fence")
	}
}

// TestExtractLeadingFrontmatter covers the helper directly: present,
// absent, and malformed (no closing ---) frontmatter cases.
func TestExtractLeadingFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantBody string
		wantRest string
		wantOK   bool
	}{
		{"present", "---\nkey: value\n---\nbody\n", "key: value\n", "body\n", true},
		{"present-no-trailing-newline", "---\nkey: value\n---", "key: value\n", "", true},
		{"absent", "no frontmatter here\n", "", "no frontmatter here\n", false},
		{"empty", "", "", "", false},
		{"only-opening", "---\nkey: value\n", "", "---\nkey: value\n", false},
		{"empty-body", "---\n---\nbody\n", "", "body\n", true},
		// CRLF on opening fence is tolerated for consistency with
		// fenceLineIndex. Inner lines are LF-terminated (the trailing \r
		// would be stripped by TrimRight on the closing-fence line check).
		{"crlf-opening", "---\r\nkey: value\n---\nbody\n", "key: value\n", "body\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, rest, ok := extractLeadingFrontmatter(c.in)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if body != c.wantBody {
				t.Errorf("body = %q, want %q", body, c.wantBody)
			}
			if rest != c.wantRest {
				t.Errorf("rest = %q, want %q", rest, c.wantRest)
			}
		})
	}
}

// TestParseEntityTSLineAnchored verifies bug C fix: the entity timestamp
// must be located via a per-line scan rather than plain LastIndex, so a
// user pasting the literal entityTSComment string into their annotations
// doesn't poison the parse. Mirrors the line-anchored fence detection
// added in the fifth-pass commit.
func TestParseEntityTSLineAnchored(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantTS string
		wantOK bool
	}{
		{
			name:   "valid-line-anchored",
			raw:    "preamble\n<!-- mnemo:entity_ts 2026-05-10T10:00:00Z -->\n" + generatedFence + "\n",
			wantTS: "2026-05-10T10:00:00Z",
			wantOK: true,
		},
		{
			name: "inline-string-in-prose-not-matched",
			// User wrote a paragraph that includes the literal comment prefix
			// inline (no newline before it). The whole-line check must skip it.
			raw:    "see how the marker `<!-- mnemo:entity_ts FAKE -->` is parsed\nbody only, no real marker\n",
			wantOK: false,
		},
		{
			name:   "last-line-anchored-wins-over-earlier-inline",
			raw:    "quoted `<!-- mnemo:entity_ts INLINE -->` here\n<!-- mnemo:entity_ts 2026-05-11T00:00:00Z -->\n" + generatedFence + "\n",
			wantTS: "2026-05-11T00:00:00Z",
			wantOK: true,
		},
		{
			name:   "missing",
			raw:    "no marker at all\n",
			wantOK: false,
		},
		{
			// Degenerate input: prefix ends with a space, suffix starts
			// with a space; on the literal "<!-- mnemo:entity_ts -->" line
			// they share that space. HasPrefix+HasSuffix both pass, but
			// the substring between them is negative-length. Without the
			// length guard, slicing would panic — a user pasting this
			// literal into their annotations would crash the next sync.
			name:   "prefix-suffix-overlap-no-timestamp",
			raw:    "<!-- mnemo:entity_ts -->\n",
			wantOK: false,
		},
		{
			// Adjacent prefix+suffix with zero-width timestamp slot
			// ("<!-- mnemo:entity_ts  -->" — two spaces). Both fences
			// match; the slot between them is empty but well-formed.
			// Treat as a valid match with an empty timestamp; needsUpdate
			// will fail to parse it and fall through to "regenerate".
			name:   "prefix-suffix-empty-timestamp",
			raw:    "<!-- mnemo:entity_ts  -->\n",
			wantTS: "",
			wantOK: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, ok := parseEntityTS(c.raw)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if ts != c.wantTS {
				t.Errorf("ts = %q, want %q", ts, c.wantTS)
			}
		})
	}
}

func TestWriteYAMLNewlineEscaping(t *testing.T) {
	var b strings.Builder
	writeYAML(&b, "title", "Bug: crash\nin handler")
	got := b.String()
	if strings.Contains(got, "\n  ") {
		t.Errorf("newline must be escaped, got literal newline in: %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("expected \\n escape sequence, got: %q", got)
	}
}

func TestWriteYAMLTabEscaping(t *testing.T) {
	var b strings.Builder
	writeYAML(&b, "key", "val\twith\ttabs")
	got := b.String()
	if strings.ContainsRune(got, '\t') {
		t.Errorf("tab must be escaped, got literal tab in: %q", got)
	}
	if !strings.Contains(got, `\t`) {
		t.Errorf("expected \\t escape sequence, got: %q", got)
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

// TestRenderSessionAliasesNewlineEscaping verifies that topics containing
// newlines/tabs/quotes do not break the YAML aliases array. Topics are
// extracted from conversation text and may contain any character.
func TestRenderSessionAliasesNewlineEscaping(t *testing.T) {
	info := store.SessionInfo{
		SessionID: "abc123def456",
		Topic:     "first line\nsecond line\twith\ttabs and \"quotes\"",
		FirstMsg:  "2026-05-10T10:00:00Z",
	}
	note := renderSession(info, nil)

	// Extract aliases block (between "aliases:\n" and the next key/end of frontmatter).
	aliasIdx := strings.Index(note, "aliases:\n")
	if aliasIdx < 0 {
		t.Fatal("session note missing aliases frontmatter")
	}
	rest := note[aliasIdx+len("aliases:\n"):]
	tagsIdx := strings.Index(rest, "tags:")
	if tagsIdx < 0 {
		t.Fatal("session note missing tags frontmatter")
	}
	aliasBlock := rest[:tagsIdx]

	// Every line in the alias block must be a list item ("  - \"...\"").
	// A raw newline in the topic would split the alias across multiple lines,
	// producing a malformed entry like `  - "first line` followed by a bare
	// `second line"` line.
	for _, line := range strings.Split(strings.TrimRight(aliasBlock, "\n"), "\n") {
		if !strings.HasPrefix(line, "  - \"") || !strings.HasSuffix(line, "\"") {
			t.Errorf("alias line not properly quoted: %q", line)
		}
	}

	// Escape sequences must be present (not literal control chars).
	if strings.Contains(aliasBlock, "\n  - \"second") {
		t.Error("topic newline not escaped — alias array is malformed")
	}
	if strings.ContainsRune(aliasBlock, '\t') {
		t.Error("topic tab not escaped — alias array contains literal tab")
	}
	if !strings.Contains(aliasBlock, `\n`) {
		t.Errorf("expected \\n escape in alias, got: %q", aliasBlock)
	}
	if !strings.Contains(aliasBlock, `\t`) {
		t.Errorf("expected \\t escape in alias, got: %q", aliasBlock)
	}
	if !strings.Contains(aliasBlock, `\"quotes\"`) {
		t.Errorf("expected \\\" escape for embedded quotes, got: %q", aliasBlock)
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

// TestBidirectionalSync verifies that human annotations below the fence are
// indexed by IngestVaultAnnotations and returned by mnemo_search, while
// generated content above the fence is not re-indexed as a vault annotation.
func TestBidirectionalSync(t *testing.T) {
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-bidir-01", []map[string]any{
		storetest.MetaMsg("user", "hello bidir", "2026-05-10T10:00:00Z",
			"/Users/alice/dev/myapp", "main"),
		storetest.Msg("assistant", "bidir response", "2026-05-10T10:00:01Z"),
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

	// Locate the generated session note and add a human annotation below the fence.
	sessionFiles, _ := filepath.Glob(filepath.Join(vaultDir, "sessions", "*", "*.md"))
	if len(sessionFiles) == 0 {
		t.Fatal("no session files generated")
	}
	noteFile := sessionFiles[0]
	raw, _ := os.ReadFile(noteFile)
	annotation := "\n## My note\n\nThis is my unique annotation about the bidir feature.\n"
	annotated := string(raw) + annotation
	if err := os.WriteFile(noteFile, []byte(annotated), 0o644); err != nil {
		t.Fatalf("write annotation: %v", err)
	}

	// IngestVaultAnnotations should index the below-fence content.
	if err := s.IngestVaultAnnotations(vaultDir); err != nil {
		t.Fatalf("IngestVaultAnnotations: %v", err)
	}

	// Verify the annotation is found by Search.
	results, err := s.Search("unique annotation bidir feature", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Role == "vault" && strings.Contains(r.Text, "unique annotation") {
			found = true
			break
		}
	}
	if !found {
		summary := fmt.Sprintf("%d results:", len(results))
		for _, r := range results {
			n := len(r.Text)
			if n > 50 {
				n = 50
			}
			summary += fmt.Sprintf(" [%s]%q", r.Role, r.Text[:n])
		}
		t.Errorf("vault annotation not found in search results; %s", summary)
	}

	// Generated content above the fence must NOT appear as a vault annotation
	// (no feedback loop). Check that no vault result contains the session header.
	for _, r := range results {
		if r.Role == "vault" && strings.Contains(r.Text, "session_id:") {
			t.Error("generated frontmatter re-indexed as vault annotation — feedback loop!")
		}
	}

	// Removing the annotation and re-ingesting should drop the row.
	if err := os.WriteFile(noteFile, raw, 0o644); err != nil {
		t.Fatalf("rewrite without annotation: %v", err)
	}
	if err := s.IngestVaultAnnotations(vaultDir); err != nil {
		t.Fatalf("IngestVaultAnnotations after removal: %v", err)
	}
	results, err = s.Search("unique annotation bidir feature", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatalf("Search after removal: %v", err)
	}
	for _, r := range results {
		if r.Role == "vault" && strings.Contains(r.Text, "unique annotation") {
			t.Error("annotation row should be removed after content deletion")
		}
	}
}

// TestVaultOnlySearch verifies Phase 3 vault hits surface even when the
// transcript FTS produces zero hits — regression test for the early-return
// that previously short-circuited before Phase 3 ran.
func TestVaultOnlySearch(t *testing.T) {
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-vo-01", []map[string]any{
		storetest.MetaMsg("user", "completely different transcript content",
			"2026-05-10T10:00:00Z", "/Users/alice/dev/myapp", "main"),
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
	sessionFiles, _ := filepath.Glob(filepath.Join(vaultDir, "sessions", "*", "*.md"))
	if len(sessionFiles) == 0 {
		t.Fatal("no session files")
	}
	raw, _ := os.ReadFile(sessionFiles[0])
	annotated := string(raw) + "\nzymurgist-quibble-paradigm\n"
	os.WriteFile(sessionFiles[0], []byte(annotated), 0o644)
	if err := s.IngestVaultAnnotations(vaultDir); err != nil {
		t.Fatalf("IngestVaultAnnotations: %v", err)
	}

	// Search for a term that only appears in the annotation, not in any message.
	results, err := s.Search("zymurgist quibble paradigm", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("vault-only search returned zero results; Phase 3 must run when Phase 2 is empty")
	}
	if results[0].Role != "vault" {
		t.Errorf("expected first result Role=vault, got %q", results[0].Role)
	}
}

// TestUserCreatedFileIsIndexed verifies that a standalone .md file dropped
// into the vault by a user (no <!-- mnemo:generated --> fence) is indexed
// in full — this is the "humans can add new files to enhance mnemo's
// knowledge" half of bidirectional sync.
func TestUserCreatedFileIsIndexed(t *testing.T) {
	projDir := t.TempDir()
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()

	// User drops their own note into the vault (no fence — entirely human content).
	userFile := filepath.Join(vaultDir, "my-thoughts.md")
	body := "# Brainstorm\n\nIdeas about distributed consensus protocols and Byzantine generals.\n"
	if err := os.WriteFile(userFile, []byte(body), 0o644); err != nil {
		t.Fatalf("write user file: %v", err)
	}

	if err := s.IngestVaultAnnotations(vaultDir); err != nil {
		t.Fatalf("IngestVaultAnnotations: %v", err)
	}

	// User's note must be findable via mnemo_search.
	results, err := s.Search("Byzantine generals consensus", 10, "all", "", 0, 0, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Role == "vault" && strings.Contains(r.Text, "Byzantine") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("user-created standalone file not found in search; %d results", len(results))
	}
}

// TestFenceLineIndexLineAnchored verifies that a literal occurrence of
// the fence string inside the user's annotation (e.g. when documenting
// mnemo) is NOT treated as a fence — only an exact line match counts.
// Without line anchoring, vault.writeNote would silently drop any content
// between the real fence and the user-typed literal on next sync.
func TestFenceLineIndexLineAnchored(t *testing.T) {
	raw := "above\n" +
		"<!-- mnemo:generated -->\n" +
		"real human content\n" +
		"And here's how the fence looks inline: `<!-- mnemo:generated -->`\n" +
		"more human content\n"
	idx := fenceLineIndex(raw)
	if idx < 0 {
		t.Fatal("expected to find the real fence line")
	}
	below := strings.TrimLeft(raw[idx:], "\n")
	if !strings.Contains(below, "real human content") {
		t.Errorf("missing 'real human content' below fence; got: %q", below)
	}
	if !strings.Contains(below, "more human content") {
		t.Errorf("missing 'more human content' — inline literal incorrectly treated as fence; got: %q", below)
	}
}

func TestWriteNotePreservesContentWithInlineFenceLiteral(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")
	if err := writeNote(path, "# Generated v1", ""); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	// Human appends content that includes the fence string inline.
	raw, _ := os.ReadFile(path)
	annotated := string(raw) + "\nMy notes:\n\n- the fence is `<!-- mnemo:generated -->`\n- and this line must survive\n"
	if err := os.WriteFile(path, []byte(annotated), 0o644); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	// Re-sync.
	if err := writeNote(path, "# Generated v2", ""); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	final, _ := os.ReadFile(path)
	if !strings.Contains(string(final), "this line must survive") {
		t.Errorf("content after inline fence literal was dropped: %s", final)
	}
	if !strings.Contains(string(final), "Generated v2") {
		t.Error("re-sync didn't update generated content")
	}
}

// TestVaultDeletionPrunesRow verifies that deleting a vault .md file
// removes its row from the docs table on the next ingest pass — without
// this, search would return content from files the user has removed.
func TestVaultDeletionPrunesRow(t *testing.T) {
	projDir := t.TempDir()
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()

	notePath := filepath.Join(vaultDir, "ephemeral.md")
	body := "# Ephemeral\n\nThis content references the rare term flibbertigibbet.\n"
	if err := os.WriteFile(notePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}
	if err := s.IngestVaultAnnotations(vaultDir); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// Sanity: searchable before deletion.
	results, _ := s.Search("flibbertigibbet", 10, "all", "", 0, 0, false)
	if len(results) == 0 {
		t.Fatal("expected search to find note before deletion")
	}

	// Delete the file and re-ingest.
	if err := os.Remove(notePath); err != nil {
		t.Fatalf("remove note: %v", err)
	}
	if err := s.IngestVaultAnnotations(vaultDir); err != nil {
		t.Fatalf("post-deletion ingest: %v", err)
	}

	results, _ = s.Search("flibbertigibbet", 10, "all", "", 0, 0, false)
	for _, r := range results {
		if r.Role == "vault" {
			t.Errorf("deleted file still surfaces in search: %q", r.Text)
		}
	}
}

// TestVaultSkipsHiddenDirs verifies that .obsidian/, .git/ etc. are not
// scanned (avoids inotify exhaustion on Linux + skips tool config churn).
func TestVaultSkipsHiddenDirs(t *testing.T) {
	projDir := t.TempDir()
	s := storetest.NewStore(t, projDir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	vaultDir := t.TempDir()

	// Plant a .md file inside an Obsidian config dir — must NOT be indexed.
	hidden := filepath.Join(vaultDir, ".obsidian")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatalf("mkdir hidden: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "plugin-config.md"),
		[]byte("zibblefrotz-config-internal\n"), 0o644); err != nil {
		t.Fatalf("write hidden file: %v", err)
	}

	if err := s.IngestVaultAnnotations(vaultDir); err != nil {
		t.Fatalf("IngestVaultAnnotations: %v", err)
	}

	results, _ := s.Search("zibblefrotz config internal", 10, "all", "", 0, 0, false)
	for _, r := range results {
		if r.Role == "vault" {
			t.Errorf("hidden dir content leaked into search: %q", r.Text)
		}
	}
}

// TestVaultSyncCoalescesConcurrentCalls verifies that two concurrent
// Sync() calls don't both run a full pass — the second returns
// immediately while the first completes. This protects writeNote's
// read-modify-write of fenced files from interleaved racing writes.
func TestVaultSyncCoalescesConcurrentCalls(t *testing.T) {
	projDir := t.TempDir()
	storetest.WriteJSONL(t, projDir, "-Users-alice-dev-myapp", "sess-coal-01", []map[string]any{
		storetest.MetaMsg("user", "hi", "2026-05-10T10:00:00Z",
			"/Users/alice/dev/myapp", "main"),
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

	// Run two Syncs concurrently. Each must return either nil (ran
	// cleanly) or ErrSyncInFlight (coalesced — bug D fix). At least one
	// nil must be observed (the work actually happened) and at most one
	// nil is possible because the second call is gated on syncing. The
	// exact (1 nil, 1 inFlight) split is timing-dependent — on a very
	// fast empty store the two goroutines can run sequentially without
	// overlap, producing two nils. That's still correct behaviour, just
	// not exercising the coalescing path; TestSyncReturnsErrSyncInFlight-
	// WhenBusy below covers the sentinel deterministically.
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- exp.Sync(context.Background())
		}()
	}
	var nilCount, inFlightCount int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			nilCount++
		case errors.Is(err, ErrSyncInFlight):
			inFlightCount++
		default:
			t.Errorf("concurrent Sync failed: %v", err)
		}
	}
	if nilCount < 1 || nilCount+inFlightCount != 2 {
		t.Errorf("expected at least one nil and total of 2 results, got nil=%d inFlight=%d",
			nilCount, inFlightCount)
	}

	// Vault state must be consistent (fence present, file readable).
	sessionFiles, _ := filepath.Glob(filepath.Join(vaultDir, "sessions", "*", "*.md"))
	if len(sessionFiles) == 0 {
		t.Fatal("no session files after concurrent Sync")
	}
	raw, _ := os.ReadFile(sessionFiles[0])
	if c := strings.Count(string(raw), generatedFence); c != 1 {
		t.Errorf("expected 1 fence after concurrent Sync, got %d", c)
	}
}

// TestSyncReturnsErrSyncInFlightWhenBusy deterministically verifies the
// coalescing sentinel: when syncing is already true (simulating an
// in-flight Sync on another goroutine), a fresh Sync call must return
// ErrSyncInFlight without doing any work. The concurrent test above is
// timing-dependent; this one is not.
func TestSyncReturnsErrSyncInFlightWhenBusy(t *testing.T) {
	exp, err := New(nil, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	exp.syncMu.Lock()
	exp.syncing = true
	exp.syncMu.Unlock()
	// nil backend would panic if Sync proceeded into work — its return of
	// ErrSyncInFlight before touching the backend is what we're asserting.
	got := exp.Sync(context.Background())
	if !errors.Is(got, ErrSyncInFlight) {
		t.Fatalf("Sync = %v, want ErrSyncInFlight", got)
	}
}

