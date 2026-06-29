// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package threads

import "time"

// View is the JSON-friendly, fully-formatted projection of a Thread shared by
// the MCP tools and the /api/thread/* endpoints, so both surfaces present
// identical fields (Integration §0.4).
type View struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	State           string `json:"state"`
	Status          string `json:"status"`
	Focus           string `json:"focus,omitempty"`
	FileCount       int    `json:"file_count"`
	Activity        string `json:"activity,omitempty"` // RFC3339, empty when none
	ActivitySummary string `json:"activity_summary"`
	CompactAge      string `json:"compact_age,omitempty"`
	// Marker is the thread's tag ("" for normal, "important"). MarkerEmoji is
	// its glyph, computed here so the shim need not know the mapping.
	Marker      string `json:"marker,omitempty"`
	MarkerEmoji string `json:"marker_emoji"`
	// ActiveTodos / OverdueTodos are per-thread TODO counts, parsed live from
	// the thread's todo.md/todos.md files with mnemo's todo parser. Omitted
	// when zero.
	ActiveTodos  int `json:"active_todos,omitempty"`
	OverdueTodos int `json:"overdue_todos,omitempty"`
}

// View renders the thread for display as of now.
func (t Thread) View(now time.Time) View {
	v := View{
		Name:            t.Name,
		Path:            t.Path,
		State:           t.State,
		Status:          t.Status,
		Focus:           t.Focus,
		FileCount:       t.FileCount,
		ActivitySummary: t.ActivitySummary(now),
		Marker:          string(t.Marker),
		MarkerEmoji:     t.Marker.Emoji(),
		ActiveTodos:     t.ActiveTodos,
		OverdueTodos:    t.OverdueTodos,
	}
	if t.HasActivity {
		v.Activity = t.Activity.Format(time.RFC3339)
		v.CompactAge = CompactAge(now, t.Activity)
	}
	return v
}

// Views renders a slice of threads as of now.
func Views(ts []Thread, now time.Time) []View {
	out := make([]View, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.View(now))
	}
	return out
}
