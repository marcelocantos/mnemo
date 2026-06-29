// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package threads

import "strings"

// extractSection returns the first meaningful line of the `## <heading>`
// section of a CLAUDE.md body. "Meaningful" means non-blank and not an
// italic-only placeholder line (e.g. `_like this_`), so an unfilled template
// section reads as empty rather than echoing its placeholder. Extraction
// stops at the next level-1 or level-2 heading. Heading matching is
// case-insensitive on the trimmed heading text.
func extractSection(body, heading string) string {
	want := strings.ToLower(strings.TrimSpace(heading))
	lines := strings.Split(body, "\n")
	inSection := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		if h, ok := headingText(trimmed); ok {
			if inSection {
				// A new heading ends the section we were reading.
				return ""
			}
			if strings.ToLower(h) == want {
				inSection = true
			}
			continue
		}
		if !inSection {
			continue
		}
		if trimmed == "" || isItalicPlaceholder(trimmed) {
			continue
		}
		return trimmed
	}
	return ""
}

// headingText returns the text of a level-1 or level-2 ATX heading
// ("# Foo" / "## Foo"), and whether the line was such a heading. Deeper
// headings (### and below) are not treated as section boundaries here,
// matching the "next ## heading" stop rule.
func headingText(trimmed string) (string, bool) {
	switch {
	case strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### "):
		return strings.TrimSpace(trimmed[3:]), true
	case strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## "):
		return strings.TrimSpace(trimmed[2:]), true
	}
	return "", false
}

// isItalicPlaceholder reports whether the whole line is a single italic
// emphasis span — `_text_` or `*text*` — as used for template placeholders.
// Bold spans (`__text__`, `**text**`) and partially-emphasised lines are not
// placeholders.
func isItalicPlaceholder(s string) bool {
	for _, c := range []byte{'_', '*'} {
		if len(s) >= 2 && s[0] == c && s[len(s)-1] == c {
			// Reject the doubled (bold) marker on either end.
			if s[1] == c || s[len(s)-2] == c {
				continue
			}
			inner := s[1 : len(s)-1]
			if inner != "" && !strings.ContainsRune(inner, rune(c)) {
				return true
			}
		}
	}
	return false
}

// firstWordState reduces a status string to its compact state token: the
// first whitespace-delimited word, lowercased, with surrounding markdown
// emphasis and punctuation stripped. "**Blocked** on db" → "blocked".
func firstWordState(status string) string {
	fields := strings.Fields(status)
	if len(fields) == 0 {
		return ""
	}
	w := strings.ToLower(fields[0])
	w = strings.Trim(w, "_*`~.,:;!?()[]\"'")
	return w
}
