// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package threads

import (
	"fmt"
	"strings"
	"time"
)

// ActivitySummary renders the `list` table's activity column: a file count
// plus a coarse relative time, e.g. "3 files, today" or "1 file, 2 weeks
// ago". A thread with neither files nor activity reads as "empty".
func (t Thread) ActivitySummary(now time.Time) string {
	var parts []string
	if t.FileCount > 0 {
		parts = append(parts, pluralFiles(t.FileCount))
	}
	if t.HasActivity {
		parts = append(parts, RelativeTime(now, t.Activity))
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, ", ")
}

func pluralFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// RelativeTime renders a coarse, human relative time for the CLI: "today",
// "yesterday", "N days ago" (<7), "N weeks ago" (<30), or "N months ago".
// Future timestamps clamp to "today".
func RelativeTime(now, t time.Time) string {
	days := calendarDaysBetween(t, now)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	case days < 7:
		return fmt.Sprintf("%d days ago", days)
	case days < 30:
		w := days / 7
		return fmt.Sprintf("%s ago", plural(w, "week"))
	default:
		mo := days / 30
		return fmt.Sprintf("%s ago", plural(mo, "month"))
	}
}

// CompactAge renders the menu-bar list's compact age column: "now", then
// minutes ("12m"), hours ("3h"), days ("2d"), then weeks ("3w"). Future
// timestamps render as "now".
func CompactAge(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/(24*7)))
	}
}

// calendarDaysBetween returns the number of whole calendar days from t to
// now using local dates (so "yesterday" means the previous calendar day, not
// strictly 24h ago). Negative differences clamp to 0.
func calendarDaysBetween(t, now time.Time) int {
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	tMid := time.Date(ty, tm, td, 0, 0, 0, 0, now.Location())
	nMid := time.Date(ny, nm, nd, 0, 0, 0, 0, now.Location())
	days := int(nMid.Sub(tMid).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
