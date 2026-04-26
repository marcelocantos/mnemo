// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

func TestExtractClaudeMDSummary(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "skips top-level heading then takes first sentence",
			in: `# mnemo

Searchable memory across Claude Code sessions. Indexes JSONL files
and exposes search via MCP.
`,
			want: "Searchable memory across Claude Code sessions.",
		},
		{
			name: "skips multiple heading levels and blank lines",
			in: `# bullseye


# Bullseye target tracker

Track convergence targets across projects. Each target has a
status, weight, and evaluation criteria.
`,
			want: "Track convergence targets across projects.",
		},
		{
			name: "no period — returns whole first body line",
			in: `# claudia

A small Go SDK for spawning Claude Code subprocesses
`,
			want: "A small Go SDK for spawning Claude Code subprocesses",
		},
		{
			name: "long single sentence — truncated with ellipsis",
			in: `# foo

` + strings.Repeat("x", 200),
			want: strings.Repeat("x", 119) + "…",
		},
		{
			name: "empty content — empty result",
			in:   "",
			want: "",
		},
		{
			name: "headings only — empty result",
			in: `# foo
## bar
### baz
`,
			want: "",
		},
		{
			name: "first sentence stops at period+space, not period+newline",
			in: `# x

Version 1.0 of mnemo. Federation across instances.
`,
			want: "Version 1.0 of mnemo.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractClaudeMDSummary(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestListReposIncludesSummaryAndLastCommit injects fake CLAUDE.md
// content + git commit rows alongside the session metadata produced by
// the standard ingest path, then verifies ListRepos picks them up via
// the new sub-selects in the SQL query.
func TestListReposIncludesSummaryAndLastCommit(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "demo", "sess-123", []map[string]any{
		metaMsg("user", "hello", "2026-04-01T10:00:00Z",
			"/Users/dev/work/github.com/acme/demo", "main"),
		msg("assistant", "world", "2026-04-01T10:00:05Z"),
		msg("user", "more", "2026-04-01T10:01:00Z"),
		msg("assistant", "ok", "2026-04-01T10:01:10Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Find the auto-derived repo string the ingest assigned.
	pre, err := s.ListRepos("")
	if err != nil {
		t.Fatalf("ListRepos pre-seed: %v", err)
	}
	if len(pre) == 0 {
		t.Fatal("no repos after ingest")
	}
	repo := pre[0].Repo

	// Inject a CLAUDE.md row + a couple of git commits keyed on that
	// repo. ListRepos's sub-selects join purely on the repo column.
	if _, err := s.db.Exec(`
		INSERT INTO claude_configs (repo, file_path, content, updated_at) VALUES
			(?, ?, ?, '2026-04-25T00:00:00Z'),
			(?, ?, ?, '2026-04-25T00:00:00Z')
	`,
		repo, "/path/to/repo/CLAUDE.md",
		"# demo\n\nThe demo repo. Used for testing.\n",
		repo, "/path/to/repo/sub/CLAUDE.md",
		"# sub\n\nSubdirectory notes — should be ignored.\n",
	); err != nil {
		t.Fatalf("insert claude_configs: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO git_commits (repo, commit_hash, author_name, author_email, commit_date, subject) VALUES
			(?, 'abc123', 'A', 'a@x', '2026-04-26T08:00:00Z', 'first'),
			(?, 'def456', 'A', 'a@x', '2026-04-26T12:34:56Z', 'second')
	`, repo, repo); err != nil {
		t.Fatalf("insert git_commits: %v", err)
	}

	got, err := s.ListRepos("")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(got))
	}
	r := got[0]
	if r.Summary != "The demo repo." {
		t.Errorf("Summary = %q, want %q (root CLAUDE.md should win over subdir)",
			r.Summary, "The demo repo.")
	}
	// LastCommit should match the latest of the two seeded commits,
	// truncated to second precision.
	if r.LastCommit != "2026-04-26T12:34:56" {
		t.Errorf("LastCommit = %q, want 2026-04-26T12:34:56", r.LastCommit)
	}
}

// TestListReposNoClaudeMDOrCommits exercises the LEFT-join fallback —
// repos with no indexed CLAUDE.md or git_commits rows must still
// appear in the listing, with empty Summary and LastCommit.
func TestListReposNoClaudeMDOrCommits(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "bare", "sess-bare", []map[string]any{
		metaMsg("user", "hello", "2026-04-01T10:00:00Z",
			"/Users/dev/work/github.com/acme/bare", "main"),
		msg("assistant", "world", "2026-04-01T10:00:05Z"),
		msg("user", "more", "2026-04-01T10:01:00Z"),
		msg("assistant", "ok", "2026-04-01T10:01:10Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListRepos("")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(got))
	}
	if got[0].Summary != "" {
		t.Errorf("Summary should be empty when no CLAUDE.md indexed, got %q", got[0].Summary)
	}
	if got[0].LastCommit != "" {
		t.Errorf("LastCommit should be empty when no commits indexed, got %q", got[0].LastCommit)
	}
}
