// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// renderSession produces a complete Markdown note for a session.
// The note includes YAML frontmatter, a metadata header with repo
// wikilink, and the full conversation formatted as Human/Claude blocks.
func renderSession(info store.SessionInfo, msgs []store.SessionMessage) string {
	var b strings.Builder

	// --- YAML frontmatter ---
	b.WriteString("---\n")
	writeYAML(&b, "session_id", info.SessionID)
	writeYAML(&b, "repo", info.Repo)
	writeYAML(&b, "project", info.Project)
	writeYAML(&b, "date", dateOf(info.FirstMsg))
	writeYAML(&b, "first_message", info.FirstMsg)
	writeYAML(&b, "last_message", info.LastMsg)
	if info.Topic != "" {
		writeYAML(&b, "topic", info.Topic)
	}
	if info.WorkType != "" {
		writeYAML(&b, "work_type", info.WorkType)
	}
	if info.GitBranch != "" {
		writeYAML(&b, "git_branch", info.GitBranch)
	}
	// Obsidian aliases: lets users find the session by topic without
	// knowing the session ID; the short ID is always included as fallback.
	// Topics are extracted from conversation text and may contain newlines
	// or other YAML-breaking characters, so route through yamlEscapeQuoted
	// (same escape pass as writeYAML).
	b.WriteString("aliases:\n")
	if info.Topic != "" {
		fmt.Fprintf(&b, "  - \"%s\"\n", yamlEscapeQuoted(info.Topic))
	}
	fmt.Fprintf(&b, "  - \"%s\"\n", yamlEscapeQuoted(shortID(info.SessionID)))
	b.WriteString("tags:\n")
	b.WriteString("  - session\n")
	if r := shortProjectName(info.Repo); r != "" && r != "untitled" && r != "unknown" {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	if info.WorkType != "" {
		fmt.Fprintf(&b, "  - %s\n", slugify(info.WorkType))
	}
	b.WriteString("---\n\n")

	// --- Title ---
	title := info.Topic
	if title == "" {
		title = "Session " + shortID(info.SessionID)
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	// --- Metadata line (human-friendly: date, repo, work type) ---
	meta := "*" + dateOf(info.FirstMsg)
	if info.Repo != "" {
		meta += fmt.Sprintf(" · [[repos/%s|%s]]", shortProjectName(info.Repo), shortProjectName(info.Repo))
	}
	if info.WorkType != "" {
		meta += fmt.Sprintf(" · %s", info.WorkType)
	}
	meta += "*"
	fmt.Fprintf(&b, "%s\n\n", meta)

	// --- Conversation ---
	substantive := 0
	for _, msg := range msgs {
		if !msg.IsNoise {
			substantive++
		}
	}
	if substantive > 0 {
		b.WriteString("## Conversation\n\n")
		for _, msg := range msgs {
			if msg.IsNoise {
				continue
			}
			role := "Human"
			if msg.Role == "assistant" {
				role = "Claude"
			}
			// Use time-of-day only (the session date is already in the
			// metadata header). Drop the inter-message `---` rule because
			// the role heading already separates turns visually and `---`
			// inside Markdown is parsed as a horizontal rule by Obsidian
			// AND as a YAML frontmatter delimiter when it appears at the
			// top of a block — confusing readers and tools.
			fmt.Fprintf(&b, "### %s · %s\n\n%s\n\n", role, timeOfDay(msg.Timestamp), msg.Text)
		}
	}

	return b.String()
}

// renderDecision produces a Markdown note for a detected decision.
func renderDecision(d store.DecisionInfo, sessionRelPath string) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "session_id", d.SessionID)
	writeYAML(&b, "repo", d.Repo)
	writeYAML(&b, "date", dateOf(d.Timestamp))
	writeYAML(&b, "timestamp", d.Timestamp)
	b.WriteString("tags:\n")
	b.WriteString("  - decision\n")
	if r := shortProjectName(d.Repo); r != "" && r != "untitled" && r != "unknown" {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# Decision · %s\n\n", dateOf(d.Timestamp))

	if sessionRelPath != "" {
		link := strings.TrimSuffix(filepath.ToSlash(sessionRelPath), ".md")
		fmt.Fprintf(&b, "*From [[%s]]*\n\n", link)
	} else {
		fmt.Fprintf(&b, "*From session `%s`*\n\n", shortID(d.SessionID))
	}

	b.WriteString("## Proposal\n\n")
	b.WriteString(strings.TrimSpace(d.ProposalText))
	b.WriteString("\n\n")

	b.WriteString("## Outcome\n\n")
	b.WriteString(strings.TrimSpace(d.ConfirmationText))
	b.WriteString("\n\n")

	return b.String()
}

// renderMemory produces a Markdown note for an indexed memory file.
func renderMemory(m store.MemoryInfo) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "project", m.Project)
	writeYAML(&b, "name", m.Name)
	writeYAML(&b, "memory_type", m.MemoryType)
	writeYAML(&b, "updated_at", m.UpdatedAt)
	if m.Description != "" {
		writeYAML(&b, "description", m.Description)
	}
	b.WriteString("tags:\n")
	b.WriteString("  - memory\n")
	if r := shortProjectName(m.Project); r != "" && r != "untitled" {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	if m.MemoryType != "" {
		fmt.Fprintf(&b, "  - memory-%s\n", slugify(m.MemoryType))
	}
	b.WriteString("---\n\n")

	name := m.Name
	if name == "" {
		name = "Memory"
	}
	fmt.Fprintf(&b, "# %s\n\n", name)

	if m.Project != "" {
		fmt.Fprintf(&b, "*Project: `%s` · [[repos/%s]]*\n\n", m.Project, shortProjectName(m.Project))
	}

	b.WriteString(strings.TrimSpace(m.Content))
	b.WriteString("\n\n")

	return b.String()
}

// renderPlan produces a Markdown note for an indexed plan file.
func renderPlan(p store.PlanInfo) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "repo", p.Repo)
	writeYAML(&b, "file_path", p.FilePath)
	writeYAML(&b, "phase", p.Phase)
	writeYAML(&b, "updated_at", p.UpdatedAt)
	b.WriteString("tags:\n")
	b.WriteString("  - plan\n")
	if r := shortProjectName(p.Repo); r != "" && r != "untitled" {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	b.WriteString("---\n\n")

	base := strings.TrimSuffix(filepath.Base(p.FilePath), ".md")
	if base == "" {
		base = "Plan"
	}
	fmt.Fprintf(&b, "# Plan: %s\n\n", base)

	if p.Repo != "" {
		fmt.Fprintf(&b, "*Repo: [[repos/%s]]", shortProjectName(p.Repo))
		if p.Phase != "" {
			fmt.Fprintf(&b, " · Phase: %s", p.Phase)
		}
		b.WriteString("*\n\n")
	}

	b.WriteString(strings.TrimSpace(p.Content))
	b.WriteString("\n\n")

	return b.String()
}

// renderTarget produces a Markdown note for a convergence target.
func renderTarget(t store.TargetInfo) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "repo", t.Repo)
	writeYAML(&b, "target_id", t.TargetID)
	writeYAML(&b, "name", t.Name)
	writeYAML(&b, "status", t.Status)
	fmt.Fprintf(&b, "weight: %.1f\n", t.Weight)
	b.WriteString("tags:\n")
	b.WriteString("  - target\n")
	if r := shortProjectName(t.Repo); r != "" && r != "untitled" {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	if t.Status != "" {
		fmt.Fprintf(&b, "  - target-%s\n", slugify(t.Status))
	}
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# %s: %s\n\n", t.TargetID, t.Name)

	fmt.Fprintf(&b, "*Repo: [[repos/%s]] · Status: **%s** · Weight: %.1f*\n\n",
		shortProjectName(t.Repo), t.Status, t.Weight)

	if t.Description != "" {
		b.WriteString(strings.TrimSpace(t.Description))
		b.WriteString("\n\n")
	}

	return b.String()
}

// renderCIRun produces a Markdown note for a CI run.
func renderCIRun(r store.CIRun) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "repo", r.Repo)
	fmt.Fprintf(&b, "run_id: %d\n", r.RunID)
	writeYAML(&b, "workflow", r.Workflow)
	writeYAML(&b, "branch", r.Branch)
	writeYAML(&b, "commit_sha", r.CommitSHA)
	writeYAML(&b, "status", r.Status)
	writeYAML(&b, "conclusion", r.Conclusion)
	writeYAML(&b, "started_at", r.StartedAt)
	writeYAML(&b, "completed_at", r.CompletedAt)
	writeYAML(&b, "url", r.URL)
	b.WriteString("tags:\n")
	b.WriteString("  - ci\n")
	if repo := shortProjectName(r.Repo); repo != "" && repo != "untitled" {
		fmt.Fprintf(&b, "  - %s\n", repo)
	}
	if r.Conclusion != "" {
		fmt.Fprintf(&b, "  - ci-%s\n", slugify(r.Conclusion))
	}
	b.WriteString("---\n\n")

	label := r.Conclusion
	if label == "" {
		label = r.Status
	}
	fmt.Fprintf(&b, "# CI: %s (%s)\n\n", r.Workflow, label)

	fmt.Fprintf(&b, "*Repo: [[repos/%s]] · Branch: `%s` · %s*\n\n",
		shortProjectName(r.Repo), r.Branch, r.StartedAt)

	if r.LogSummary != "" {
		b.WriteString("## Log summary\n\n")
		b.WriteString(strings.TrimSpace(r.LogSummary))
		b.WriteString("\n\n")
	}

	if r.URL != "" {
		fmt.Fprintf(&b, "[View run on GitHub](%s)\n\n", r.URL)
	}

	return b.String()
}

// renderPR produces a Markdown note for a GitHub PR or issue.
func renderPR(r store.GitHubActivityResult) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "repo", r.Repo)
	writeYAML(&b, "type", r.Type)
	fmt.Fprintf(&b, "number: %d\n", r.Number)
	writeYAML(&b, "title", r.Title)
	writeYAML(&b, "state", r.State)
	writeYAML(&b, "author", r.Author)
	writeYAML(&b, "created_at", r.CreatedAt)
	writeYAML(&b, "updated_at", r.UpdatedAt)
	if r.MergedAt != "" {
		writeYAML(&b, "merged_at", r.MergedAt)
	}
	writeYAML(&b, "url", r.URL)
	b.WriteString("tags:\n")
	if r.Type != "" {
		fmt.Fprintf(&b, "  - %s\n", r.Type)
	}
	if repo := shortProjectName(r.Repo); repo != "" && repo != "untitled" {
		fmt.Fprintf(&b, "  - %s\n", repo)
	}
	if r.Type != "" && r.State != "" {
		fmt.Fprintf(&b, "  - %s-%s\n", r.Type, slugify(r.State))
	}
	b.WriteString("---\n\n")

	kind := "PR"
	if r.Type == "issue" {
		kind = "Issue"
	}
	fmt.Fprintf(&b, "# %s #%d: %s\n\n", kind, r.Number, r.Title)

	fmt.Fprintf(&b, "*Repo: [[repos/%s]] · %s · by @%s · %s*\n\n",
		shortProjectName(r.Repo), r.State, r.Author, r.CreatedAt)

	if r.Body != "" {
		b.WriteString(strings.TrimSpace(r.Body))
		b.WriteString("\n\n")
	}

	if r.URL != "" {
		fmt.Fprintf(&b, "[View on GitHub](%s)\n\n", r.URL)
	}

	return b.String()
}

// renderSkill produces a Markdown note for a skill file.
func renderSkill(s store.SkillInfo) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "name", s.Name)
	writeYAML(&b, "file_path", s.FilePath)
	writeYAML(&b, "updated_at", s.UpdatedAt)
	if s.Description != "" {
		writeYAML(&b, "description", s.Description)
	}
	b.WriteString("tags:\n")
	b.WriteString("  - skill\n")
	b.WriteString("---\n\n")

	name := s.Name
	if name == "" {
		name = filepath.Base(s.FilePath)
	}
	fmt.Fprintf(&b, "# Skill: %s\n\n", name)

	if s.Description != "" {
		fmt.Fprintf(&b, "*%s*\n\n", s.Description)
	}

	b.WriteString(strings.TrimSpace(s.Content))
	b.WriteString("\n\n")

	return b.String()
}

// renderConfig produces a Markdown note for a CLAUDE.md config file.
func renderConfig(c store.ClaudeConfigInfo) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "repo", c.Repo)
	writeYAML(&b, "file_path", c.FilePath)
	writeYAML(&b, "updated_at", c.UpdatedAt)
	b.WriteString("tags:\n")
	b.WriteString("  - config\n")
	b.WriteString("  - claude-md\n")
	if r := shortProjectName(c.Repo); r != "" && r != "untitled" && r != "global" {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	b.WriteString("---\n\n")

	title := shortProjectName(c.Repo)
	if title == "" || title == "untitled" {
		title = "global"
	}
	fmt.Fprintf(&b, "# CLAUDE.md: %s\n\n", title)

	if c.Repo != "" {
		fmt.Fprintf(&b, "*Repo: [[repos/%s]]*\n\n", shortProjectName(c.Repo))
	} else {
		fmt.Fprintf(&b, "*Global config (`~/.claude/CLAUDE.md`)*\n\n")
	}

	b.WriteString(strings.TrimSpace(c.Content))
	b.WriteString("\n\n")

	return b.String()
}

// renderRepoIndex produces the Markdown index note for a single repository.
func renderRepoIndex(repo store.RepoInfo, sessions []store.SessionInfo, decisions []store.DecisionInfo, sessionPaths map[string]string) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "repo", repo.Repo)
	writeYAML(&b, "path", repo.Path)
	writeYAML(&b, "last_activity", repo.LastActivity)
	fmt.Fprintf(&b, "sessions: %d\n", repo.Sessions)
	b.WriteString("tags:\n")
	b.WriteString("  - repo\n")
	b.WriteString("  - index\n")
	if r := shortProjectName(repo.Repo); r != "" && r != "untitled" {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	b.WriteString("---\n\n")

	// Title uses the friendly short name; the full path lives in the
	// metadata line below for traceability.
	title := shortProjectName(repo.Repo)
	if title == "" || title == "untitled" {
		title = repo.Repo
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	meta := fmt.Sprintf("*Path: `%s` · %s · last active %s*",
		repo.Path, pluralize(repo.Sessions, "session"), dateOf(repo.LastActivity))
	fmt.Fprintf(&b, "%s\n\n", meta)

	if repo.Summary != "" {
		b.WriteString(repo.Summary)
		b.WriteString("\n\n")
	}

	if len(sessions) > 0 {
		b.WriteString("## Recent sessions\n\n")
		for _, s := range sessions {
			p, ok := sessionPaths[s.SessionID]
			if !ok {
				p = sessionPath(s)
			}
			link := strings.TrimSuffix(filepath.ToSlash(p), ".md")
			topic := s.Topic
			if topic == "" {
				topic = "session " + shortID(s.SessionID)
			}
			fmt.Fprintf(&b, "- %s · [[%s|%s]]\n", dateOf(s.FirstMsg), link, topic)
		}
		b.WriteString("\n")
	}

	if len(decisions) > 0 {
		b.WriteString("## Recent decisions\n\n")
		for _, d := range decisions {
			p := strings.TrimSuffix(filepath.ToSlash(decisionPath(d)), ".md")
			summary := summarize(d.ProposalText, 80)
			fmt.Fprintf(&b, "- %s · [[%s|%s]]\n", dateOf(d.Timestamp), p, summary)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// renderRootIndex produces the vault root index.md.
func renderRootIndex(repos []store.RepoInfo, stats *store.StatsResult) string {
	var b strings.Builder

	b.WriteString("---\n")
	b.WriteString("tags:\n")
	b.WriteString("  - mnemo\n")
	b.WriteString("  - index\n")
	b.WriteString("  - vault\n")
	b.WriteString("---\n\n")

	b.WriteString("# mnemo knowledge vault\n\n")
	b.WriteString("*Maintained by [mnemo](https://github.com/marcelocantos/mnemo). ")
	b.WriteString("Drop your own `.md` files anywhere here, or annotate below ")
	b.WriteString("the `<!-- mnemo:generated -->` fence — both flow into ")
	b.WriteString("`mnemo_search` alongside transcripts.*\n\n")

	if stats != nil {
		fmt.Fprintf(&b, "*%s · %d messages indexed*\n\n",
			pluralize(stats.TotalSessions, "session"), stats.TotalMessages)
	}

	if len(repos) > 0 {
		b.WriteString("## Repositories\n\n")
		for _, r := range repos {
			link := strings.TrimSuffix(filepath.ToSlash(repoIndexPath(r.Repo)), ".md")
			name := shortProjectName(r.Repo)
			if name == "" || name == "untitled" {
				name = r.Repo
			}
			fmt.Fprintf(&b, "- [[%s|%s]] — %s · last active %s\n",
				link, name, pluralize(r.Sessions, "session"), dateOf(r.LastActivity))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Sections\n\n")
	b.WriteString("- `sessions/` — full session transcripts (one file per session)\n")
	b.WriteString("- `decisions/` — extracted decisions with proposal + outcome\n")
	b.WriteString("- `memories/` — project memory files\n")
	b.WriteString("- `skills/` — reusable skill procedures from `~/.claude/skills/`\n")
	b.WriteString("- `configs/` — CLAUDE.md project instruction files\n")
	b.WriteString("- `plans/` — planning documents\n")
	b.WriteString("- `targets/` — convergence targets\n")
	b.WriteString("- `ci/` — CI run summaries\n")
	b.WriteString("- `prs/` — pull requests and issues\n")
	b.WriteString("- `repos/` — per-repo index notes\n\n")

	return b.String()
}

// writeYAML appends "key: value\n" to b, quoting value when it contains
// characters that would break YAML parsing. Empty values are omitted.
func writeYAML(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	// Quote if value contains YAML-special characters, control chars, or
	// leading/trailing whitespace that would be lost without quoting.
	needsQuote := strings.ContainsAny(value, ":{}[]|>&*!,'\"#%@`\n\t\r") ||
		value != strings.TrimSpace(value)
	if needsQuote {
		fmt.Fprintf(b, "%s: \"%s\"\n", key, yamlEscapeQuoted(value))
	} else {
		fmt.Fprintf(b, "%s: %s\n", key, value)
	}
}

// yamlEscapeQuoted returns s escaped for use inside a YAML double-quoted
// scalar. Escapes backslash, double-quote, and the control characters
// \n/\r/\t that would otherwise break the scalar across lines.
func yamlEscapeQuoted(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// summarize truncates s to at most maxLen printable characters, breaking
// at the last word boundary before the limit.
func summarize(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ") // normalise whitespace
	if len(s) <= maxLen {
		return s
	}
	if idx := strings.LastIndexByte(s[:maxLen], ' '); idx > 0 {
		return s[:idx] + "..."
	}
	return s[:maxLen] + "..."
}
