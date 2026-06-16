// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package todo

import (
	"fmt"
	"regexp"
	"strings"
)

// This file holds the surgical line-rewriters used by TODO write-back.
// Each function takes the exact original checkbox line and returns a new
// line with one facet changed, leaving everything else — indentation,
// bullet style, surrounding text, unrelated decorations — byte-for-byte
// intact. This is what lets mnemo edit a single task in a human-authored
// file without reformatting the rest of it.

var markerRe = regexp.MustCompile(`^(\s*[-*+]\s+\[)[ xX/\-](\].*)$`)

// markerFor returns the checkbox character for a Status.
func markerFor(s Status) string {
	switch s {
	case StatusDone:
		return "x"
	case StatusCancelled:
		return "-"
	case StatusInProgress:
		return "/"
	default:
		return " "
	}
}

// SetStatus rewrites raw's checkbox to status and reconciles completion
// dates: transitioning to done stamps a ✅ date (and clears ❌);
// cancelled stamps ❌ (and clears ✅); open or in-progress clears both.
// The date argument supplies the stamp for done/cancelled — pass "" to
// change the marker without stamping a date.
func SetStatus(raw string, status Status, date string) string {
	out := markerRe.ReplaceAllString(raw, `${1}`+markerFor(status)+`${2}`)
	switch status {
	case StatusDone:
		out = removeDate(out, emojiCancelled)
		if date != "" {
			out = upsertDate(out, emojiDone, date)
		}
	case StatusCancelled:
		out = removeDate(out, emojiDone)
		if date != "" {
			out = upsertDate(out, emojiCancelled, date)
		}
	default:
		out = removeDate(out, emojiDone)
		out = removeDate(out, emojiCancelled)
	}
	return out
}

// SetDue upserts the 📅 due date on raw. An empty date clears it.
func SetDue(raw, date string) string {
	if date == "" {
		return removeDate(raw, emojiDue)
	}
	return upsertDate(raw, emojiDue, date)
}

var anyPrioRe = regexp.MustCompile(`\s*(?:🔺|⏫|🔼|🔽|⏬)`)

// SetPriority removes any existing priority signifier from raw and, for
// any priority other than none, appends the corresponding emoji. None
// simply clears.
func SetPriority(raw string, p Priority) string {
	out := anyPrioRe.ReplaceAllString(raw, "")
	emoji := ""
	switch p {
	case PriorityHighest:
		emoji = emojiPrioHighest
	case PriorityHigh:
		emoji = emojiPrioHigh
	case PriorityMedium:
		emoji = emojiPrioMedium
	case PriorityLow:
		emoji = emojiPrioLow
	case PriorityLowest:
		emoji = emojiPrioLowest
	}
	if emoji == "" {
		return strings.TrimRight(out, " ")
	}
	return strings.TrimRight(out, " ") + " " + emoji
}

// NewTaskLine builds a fresh checkbox line for an added task at the given
// indent (in columns; rendered as spaces) and status. The text is taken
// verbatim — callers that want decorations should include them.
func NewTaskLine(text string, indent int, status Status) string {
	return strings.Repeat(" ", indent) + "- [" + markerFor(status) + "] " + text
}

// upsertDate replaces an existing `emoji date` pair in raw with the new
// date, or appends `emoji date` at the end when none is present.
func upsertDate(raw, emoji, date string) string {
	re := regexp.MustCompile(regexp.QuoteMeta(emoji) + `\s*\d{4}-\d{2}-\d{2}`)
	repl := emoji + " " + date
	if re.MatchString(raw) {
		return re.ReplaceAllString(raw, repl)
	}
	return strings.TrimRight(raw, " ") + " " + repl
}

// removeDate strips an `emoji date` pair (and the whitespace before it)
// from raw, if present.
func removeDate(raw, emoji string) string {
	re := regexp.MustCompile(`\s*` + regexp.QuoteMeta(emoji) + `\s*\d{4}-\d{2}-\d{2}`)
	return re.ReplaceAllString(raw, "")
}

// ReplaceLine returns content with its 1-based lineNo replaced by
// newLine. It verifies that the current line equals wantOld before
// replacing, returning an error otherwise so a stale edit (the file
// changed under us) fails loudly rather than clobbering the wrong line.
func ReplaceLine(content string, lineNo int, wantOld, newLine string) (string, error) {
	lines := strings.Split(content, "\n")
	if lineNo < 1 || lineNo > len(lines) {
		return "", fmt.Errorf("line %d out of range (file has %d lines)", lineNo, len(lines))
	}
	if lines[lineNo-1] != wantOld {
		return "", fmt.Errorf("line %d changed since indexed: have %q, expected %q",
			lineNo, lines[lineNo-1], wantOld)
	}
	lines[lineNo-1] = newLine
	return strings.Join(lines, "\n"), nil
}
