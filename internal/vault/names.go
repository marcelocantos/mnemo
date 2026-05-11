// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

var (
	nonAlphanumRE      = regexp.MustCompile(`[^a-z0-9]+`)
	usersHomePrefixRE  = regexp.MustCompile(`^users-[^-]+-(.+)$`)
)

// slugify creates a filename-safe slug from s: lowercase, non-alphanumeric
// runs replaced with single hyphens, leading/trailing hyphens removed,
// capped at 60 characters.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphanumRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	if s == "" {
		return "untitled"
	}
	return s
}

// shortProjectName returns a human-readable short name from a project path.
//
//   - Absolute paths ("/Users/alice/dev/myapp") → last component ("myapp")
//   - Claude project strings ("-Users-alice-dev-myapp") → strips the home-dir
//     prefix and one common intermediate segment (dev, documents, work, home)
//     to expose the meaningful leaf name ("myapp")
//
// Unlike slugify, there is no length cap so the full leaf name is preserved.
func shortProjectName(s string) string {
	if strings.HasPrefix(s, "/") {
		base := filepath.Base(s)
		if base != "" && base != "." && base != "/" {
			return slugify(base)
		}
	}
	// Normalise without length cap.
	slug := strings.ToLower(s)
	slug = nonAlphanumRE.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "untitled"
	}
	// Strip "users-<username>-" produced by Claude's project-dir encoding.
	if m := usersHomePrefixRE.FindStringSubmatch(slug); m != nil {
		rest := m[1]
		for _, pfx := range []string{"dev-", "documents-", "work-", "home-", "projects-", "code-", "repos-", "src-"} {
			if strings.HasPrefix(rest, pfx) && len(rest) > len(pfx) {
				rest = rest[len(pfx):]
				break
			}
		}
		if rest != "" {
			return rest
		}
	}
	return slug
}

// dateOf extracts the YYYY-MM-DD date from an RFC3339 timestamp string.
// Returns today's UTC date when ts is empty or shorter than 10 chars.
func dateOf(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return time.Now().UTC().Format("2006-01-02")
}

// timeOfDay extracts HH:MM:SS from an RFC3339 timestamp like
// "2026-05-10T14:23:45Z" → "14:23:45". The session date is shown once in
// the note header so per-message timestamps only need the time component.
// Falls back to the full input when the format doesn't match.
func timeOfDay(ts string) string {
	if len(ts) >= 19 && ts[10] == 'T' {
		return ts[11:19]
	}
	return ts
}

// pluralize formats "<n> <noun>" with a trailing s when n != 1. Keeps
// human-facing counts grammatically correct in repo/index metadata lines.
func pluralize(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// shortID returns the first min(8, len(id)) characters of id.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sessionPath returns the vault-relative file path for a session note.
// Format: sessions/<repo-slug>/YYYY-MM-DD-<topic-slug>-<id8>.md
func sessionPath(info store.SessionInfo) string {
	repo := repoSlugFor(info)
	date := dateOf(info.FirstMsg)
	topic := info.Topic
	if topic == "" {
		topic = "session"
	}
	name := date + "-" + slugify(topic) + "-" + shortID(info.SessionID)
	return filepath.Join("sessions", repo, name+".md")
}

// decisionPath returns the vault-relative file path for a decision note.
// Format: decisions/<short-repo>/YYYY-MM-DD-<session-id8>.md
func decisionPath(d store.DecisionInfo) string {
	repo := decisionRepoSlug(d.Repo)
	date := dateOf(d.Timestamp)
	return filepath.Join("decisions", repo, date+"-"+shortID(d.SessionID)+".md")
}

// memoryPath returns the vault-relative path for a memory note.
// Format: memories/<short-project>-<name-slug>.md
//
// Flat (no subdirectory) so that every file in the vault has a globally
// unique stem — Obsidian uses the filename as the page title for wikilinks
// and warns when two files share a title regardless of their directory.
func memoryPath(m store.MemoryInfo) string {
	proj := shortProjectName(m.Project)
	if proj == "" || proj == "untitled" {
		proj = "unknown"
	}
	name := slugify(m.Name)
	if name == "" || name == "untitled" {
		name = "memory"
	}
	return filepath.Join("memories", proj+"-"+name+".md")
}

// planPath returns the vault-relative path for a plan note.
// Format: plans/<short-repo>-<file-base-slug>.md
//
// Flat so plan files from different repos with the same base name don't clash.
func planPath(p store.PlanInfo) string {
	repo := shortProjectName(p.Repo)
	if repo == "" || repo == "untitled" {
		repo = "unknown"
	}
	base := strings.TrimSuffix(filepath.Base(p.FilePath), ".md")
	if base == "" {
		base = "plan"
	}
	return filepath.Join("plans", repo+"-"+slugify(base)+".md")
}

// targetPath returns the vault-relative path for a convergence target note.
// Format: targets/<short-repo>/<target-id-slug>.md
func targetPath(t store.TargetInfo) string {
	repo := shortProjectName(t.Repo)
	if repo == "" || repo == "untitled" {
		repo = "unknown"
	}
	id := slugify(t.TargetID)
	if id == "" || id == "untitled" {
		id = fmt.Sprintf("target-%d", t.ID)
	}
	return filepath.Join("targets", repo, id+".md")
}

// ciRunPath returns the vault-relative path for a CI run note.
// Format: ci/<short-repo>/YYYY-MM-DD-<workflow-slug>-<run-id mod 10000>.md
func ciRunPath(r store.CIRun) string {
	repo := shortProjectName(r.Repo)
	if repo == "" || repo == "untitled" {
		repo = "unknown"
	}
	date := dateOf(r.StartedAt)
	name := fmt.Sprintf("%s-%s-%04d", date, slugify(r.Workflow), r.RunID%10000)
	return filepath.Join("ci", repo, name+".md")
}

// prPath returns the vault-relative path for a PR or issue note.
// Format: prs/<short-repo>/YYYY-MM-DD-<type>-<number>-<title-slug>.md
func prPath(r store.GitHubActivityResult) string {
	repo := shortProjectName(r.Repo)
	if repo == "" || repo == "untitled" {
		repo = "unknown"
	}
	date := dateOf(r.CreatedAt)
	name := fmt.Sprintf("%s-%s-%d-%s", date, r.Type, r.Number, slugify(r.Title))
	return filepath.Join("prs", repo, name+".md")
}

// repoIndexPath returns the vault-relative path for a repo index note.
// Format: repos/<short-repo>.md
func repoIndexPath(repo string) string {
	return filepath.Join("repos", shortProjectName(repo)+".md")
}

// skillPath returns the vault-relative path for a skill note.
// Format: skills/<name-slug>.md
// Flat (global) since skills live in ~/.claude/skills/ with no repo grouping.
func skillPath(s store.SkillInfo) string {
	name := slugify(s.Name)
	if name == "" || name == "untitled" {
		name = "skill"
	}
	return filepath.Join("skills", name+".md")
}

// configPath returns the vault-relative path for a CLAUDE.md config note.
// Format: configs/<short-repo>.md
func configPath(c store.ClaudeConfigInfo) string {
	repo := shortProjectName(c.Repo)
	if repo == "" || repo == "untitled" {
		repo = "global"
	}
	return filepath.Join("configs", repo+".md")
}

// repoSlugFor extracts a human-readable short slug from a SessionInfo's Repo
// field, falling back to Project then "unknown".
func repoSlugFor(info store.SessionInfo) string {
	if info.Repo != "" {
		return shortProjectName(info.Repo)
	}
	if info.Project != "" {
		return shortProjectName(info.Project)
	}
	return "unknown"
}

// decisionRepoSlug extracts a human-readable short slug for a decision's repo.
func decisionRepoSlug(repo string) string {
	if repo == "" {
		return "unknown"
	}
	return shortProjectName(repo)
}
