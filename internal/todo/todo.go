// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package todo parses Markdown TODO files written in the Obsidian Tasks
// dialect. A file is a flat sequence of lines; checkbox lines
// (`- [ ] …`, `- [x] …`, `- [-] …`, `- [/] …`) become Tasks, each
// annotated with the nearest preceding heading (Section), nesting depth
// (Indent), and any Obsidian Tasks decorations found in the body:
// emoji-tagged dates (📅 due, ⏳ scheduled, 🛫 start, ➕ created,
// ✅ done, ❌ cancelled), a priority signifier (🔺⏫🔼🔽⏬),
// a recurrence rule (🔁 …), #tags, and [[wikilinks]].
//
// The parser is deliberately lossless at the line level: every Task
// keeps the exact original line in RawLine, so a caller can rewrite a
// single task in place without disturbing the rest of the file
// (see Render/SetStatus in writeback.go).
package todo

import (
	"regexp"
	"strings"
)

// Status is a task's checkbox state. The string values are stable —
// they are persisted in the database and accepted as filter arguments
// by the mnemo_todos tool.
type Status string

const (
	StatusOpen       Status = "open"        // - [ ]
	StatusDone       Status = "done"        // - [x] or - [X]
	StatusCancelled  Status = "cancelled"   // - [-]
	StatusInProgress Status = "in_progress" // - [/]
)

// Priority orders tasks; a larger value is more urgent. The ordering
// follows Obsidian Tasks, where an unmarked task ("none") sits between
// medium and low rather than at the bottom:
//
//	Highest > High > Medium > None > Low > Lowest
//
// so sorting by Priority descending yields the Obsidian display order.
type Priority int

const (
	PriorityLowest  Priority = 1 // ⏬
	PriorityLow     Priority = 2 // 🔽
	PriorityNone    Priority = 3 // (no signifier)
	PriorityMedium  Priority = 4 // 🔼
	PriorityHigh    Priority = 5 // ⏫
	PriorityHighest Priority = 6 // 🔺
)

// String returns the lowercase priority name used by the mnemo_todos
// tool for both output and filter arguments.
func (p Priority) String() string {
	switch p {
	case PriorityLowest:
		return "lowest"
	case PriorityLow:
		return "low"
	case PriorityMedium:
		return "medium"
	case PriorityHigh:
		return "high"
	case PriorityHighest:
		return "highest"
	default:
		return "none"
	}
}

// PriorityFromString maps a priority name (as produced by
// Priority.String) back to a Priority. Unknown names — including the
// empty string — map to PriorityNone, so an absent filter is a no-op.
func PriorityFromString(s string) Priority {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "lowest":
		return PriorityLowest
	case "low":
		return PriorityLow
	case "medium":
		return PriorityMedium
	case "high":
		return PriorityHigh
	case "highest":
		return PriorityHighest
	default:
		return PriorityNone
	}
}

// Task is one parsed checkbox line.
type Task struct {
	Line     int      `json:"line"`    // 1-based line number in the file
	Indent   int      `json:"indent"`  // leading-whitespace columns (tab = 4)
	Status   Status   `json:"status"`  //
	Text     string   `json:"text"`    // task body with decorations stripped
	RawLine  string   `json:"-"`       // the exact original line (for write-back)
	Section  string   `json:"section"` // nearest preceding heading, "" if none
	Priority Priority `json:"-"`       //

	// Obsidian Tasks dates, ISO "YYYY-MM-DD", empty when absent.
	Due       string `json:"due,omitempty"`
	Scheduled string `json:"scheduled,omitempty"`
	Start     string `json:"start,omitempty"`
	Created   string `json:"created,omitempty"`
	Done      string `json:"done,omitempty"`
	Cancelled string `json:"cancelled,omitempty"`

	Recurrence string   `json:"recurrence,omitempty"` // 🔁 rule text
	Tags       []string `json:"tags,omitempty"`       // #tags, without the leading '#'
	Links      []string `json:"links,omitempty"`      // [[wikilink]] targets
}

// PriorityName is Priority.String exposed as a struct-friendly field for
// JSON callers (Priority itself is json:"-" so the integer never leaks).
func (t Task) PriorityName() string { return t.Priority.String() }

// Date emoji signifiers. Defined as constants so the parse table and any
// write-back code share one source of truth.
const (
	emojiDue       = "📅"
	emojiScheduled = "⏳"
	emojiStart     = "🛫"
	emojiCreated   = "➕"
	emojiDone      = "✅"
	emojiCancelled = "❌"

	emojiPrioHighest = "🔺"
	emojiPrioHigh    = "⏫"
	emojiPrioMedium  = "🔼"
	emojiPrioLow     = "🔽"
	emojiPrioLowest  = "⏬"
)

var (
	// checkboxRe matches a list item with a checkbox: optional leading
	// whitespace, a bullet (-, *, +), the [x] marker, and the body.
	checkboxRe = regexp.MustCompile(`^(\s*)[-*+]\s+\[([ xX/\-])\]\s?(.*)$`)

	// headingRe matches an ATX heading (# … through ###### …).
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

	// dateRe captures one emoji-tagged date. The emoji alternation is
	// built from the constants above in init.
	dateRe *regexp.Regexp

	// recurRe captures a recurrence rule: 🔁 followed by everything up
	// to the next decoration emoji or end of line.
	recurRe = regexp.MustCompile(`🔁\s*([^📅⏳🛫➕✅❌🔺⏫🔼🔽⏬#]+)`)

	// tagRe matches an Obsidian #tag (letters, digits, _, -, /, and the
	// nested form tag/sub). Leading '#' is not captured.
	tagRe = regexp.MustCompile(`(^|\s)#([A-Za-z0-9][A-Za-z0-9_/\-]*)`)

	// wikilinkRe matches [[target]] or [[target|alias]]; captures target,
	// used to populate Task.Links for navigation/indexing.
	wikilinkRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)

	// wikilinkDisplayRe captures a wikilink's display text — the alias
	// after '|' when present, else the target. cleanText substitutes
	// this so the human-readable task text reads as Obsidian renders it.
	wikilinkDisplayRe = regexp.MustCompile(`\[\[(?:[^\]|]+\|)?([^\]]+)\]\]`)
)

func init() {
	emojis := strings.Join([]string{
		emojiDue, emojiScheduled, emojiStart,
		emojiCreated, emojiDone, emojiCancelled,
	}, "|")
	dateRe = regexp.MustCompile(`(` + emojis + `)\s*(\d{4}-\d{2}-\d{2})`)
}

// Parse extracts every checkbox line from a TODO file's content.
// Lines are 1-based. Non-checkbox lines are skipped except headings,
// which update the running Section applied to subsequent tasks.
func Parse(content string) []Task {
	lines := strings.Split(content, "\n")
	var tasks []Task
	section := ""

	for i, line := range lines {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			section = strings.TrimSpace(m[2])
			continue
		}
		m := checkboxRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		tasks = append(tasks, parseTask(i+1, line, m[1], m[2], m[3], section))
	}
	return tasks
}

// parseTask builds a Task from a matched checkbox line. indentWS is the
// captured leading whitespace, marker the single status character, and
// body the text after the checkbox.
func parseTask(lineNo int, raw, indentWS, marker, body, section string) Task {
	t := Task{
		Line:     lineNo,
		Indent:   indentWidth(indentWS),
		Status:   statusFromMarker(marker),
		RawLine:  raw,
		Section:  section,
		Priority: PriorityNone,
	}

	for _, m := range dateRe.FindAllStringSubmatch(body, -1) {
		switch m[1] {
		case emojiDue:
			t.Due = m[2]
		case emojiScheduled:
			t.Scheduled = m[2]
		case emojiStart:
			t.Start = m[2]
		case emojiCreated:
			t.Created = m[2]
		case emojiDone:
			t.Done = m[2]
		case emojiCancelled:
			t.Cancelled = m[2]
		}
	}

	t.Priority = priorityFromBody(body)

	if m := recurRe.FindStringSubmatch(body); m != nil {
		t.Recurrence = strings.TrimSpace(m[1])
	}

	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		t.Tags = append(t.Tags, m[2])
	}
	for _, m := range wikilinkRe.FindAllStringSubmatch(body, -1) {
		t.Links = append(t.Links, strings.TrimSpace(m[1]))
	}

	t.Text = cleanText(body)
	return t
}

// statusFromMarker maps the checkbox character to a Status.
func statusFromMarker(marker string) Status {
	switch marker {
	case "x", "X":
		return StatusDone
	case "-":
		return StatusCancelled
	case "/":
		return StatusInProgress
	default:
		return StatusOpen
	}
}

// priorityFromBody returns the highest-precedence priority signifier
// present in body, or PriorityNone. Highest wins if several appear.
func priorityFromBody(body string) Priority {
	switch {
	case strings.Contains(body, emojiPrioHighest):
		return PriorityHighest
	case strings.Contains(body, emojiPrioHigh):
		return PriorityHigh
	case strings.Contains(body, emojiPrioMedium):
		return PriorityMedium
	case strings.Contains(body, emojiPrioLow):
		return PriorityLow
	case strings.Contains(body, emojiPrioLowest):
		return PriorityLowest
	default:
		return PriorityNone
	}
}

// indentWidth converts leading whitespace to a column count, treating a
// tab as four columns so tab- and space-indented files nest consistently.
func indentWidth(ws string) int {
	n := 0
	for _, r := range ws {
		if r == '\t' {
			n += 4
		} else {
			n++
		}
	}
	return n
}

// decorationTokens lists every token cleanText removes from a task body
// to recover the human-readable text. Dates, tags, and wikilinks are
// handled separately by regex; this covers the bare signifier emojis.
var decorationTokens = []string{
	emojiPrioHighest, emojiPrioHigh, emojiPrioMedium, emojiPrioLow, emojiPrioLowest,
}

// cleanText strips all Obsidian Tasks decorations from a body, leaving
// the human-readable task text with internal whitespace collapsed.
func cleanText(body string) string {
	s := body
	s = dateRe.ReplaceAllString(s, "")
	s = recurRe.ReplaceAllString(s, "")
	s = wikilinkDisplayRe.ReplaceAllString(s, "$1")
	s = tagRe.ReplaceAllString(s, "")
	for _, tok := range decorationTokens {
		s = strings.ReplaceAll(s, tok, "")
	}
	return strings.Join(strings.Fields(s), " ")
}
