// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package todo

import (
	"reflect"
	"testing"
)

func TestParseBasicStatuses(t *testing.T) {
	content := `# Project

- [ ] open task
- [x] done task
- [X] done upper
- [-] cancelled task
- [/] in progress
not a task
* [ ] star bullet
+ [ ] plus bullet
`
	got := Parse(content)
	want := []struct {
		line   int
		status Status
		text   string
	}{
		{3, StatusOpen, "open task"},
		{4, StatusDone, "done task"},
		{5, StatusDone, "done upper"},
		{6, StatusCancelled, "cancelled task"},
		{7, StatusInProgress, "in progress"},
		{9, StatusOpen, "star bullet"},
		{10, StatusOpen, "plus bullet"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Line != w.line || got[i].Status != w.status || got[i].Text != w.text {
			t.Errorf("task %d: got {line:%d status:%s text:%q}, want {line:%d status:%s text:%q}",
				i, got[i].Line, got[i].Status, got[i].Text, w.line, w.status, w.text)
		}
	}
}

func TestParseSections(t *testing.T) {
	content := `# Top

- [ ] under top

## Subsection

- [ ] under sub
### Deep
- [ ] under deep
`
	got := Parse(content)
	wantSections := []string{"Top", "Subsection", "Deep"}
	if len(got) != 3 {
		t.Fatalf("got %d tasks, want 3", len(got))
	}
	for i, ws := range wantSections {
		if got[i].Section != ws {
			t.Errorf("task %d: section %q, want %q", i, got[i].Section, ws)
		}
	}
}

func TestParseIndent(t *testing.T) {
	content := "- [ ] root\n  - [ ] two spaces\n    - [ ] four spaces\n\t- [ ] one tab\n"
	got := Parse(content)
	want := []int{0, 2, 4, 4}
	if len(got) != 4 {
		t.Fatalf("got %d tasks, want 4", len(got))
	}
	for i, w := range want {
		if got[i].Indent != w {
			t.Errorf("task %d: indent %d, want %d", i, got[i].Indent, w)
		}
	}
}

func TestParseDecorations(t *testing.T) {
	content := "- [ ] write report 📅 2026-06-20 ⏳ 2026-06-18 🛫 2026-06-15 ➕ 2026-06-10 🔼 🔁 every week #work #docs/spec [[Project X]] [[Notes|see here]]\n"
	got := Parse(content)
	if len(got) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got))
	}
	task := got[0]
	if task.Due != "2026-06-20" {
		t.Errorf("due: %q", task.Due)
	}
	if task.Scheduled != "2026-06-18" {
		t.Errorf("scheduled: %q", task.Scheduled)
	}
	if task.Start != "2026-06-15" {
		t.Errorf("start: %q", task.Start)
	}
	if task.Created != "2026-06-10" {
		t.Errorf("created: %q", task.Created)
	}
	if task.Priority != PriorityMedium {
		t.Errorf("priority: %v, want medium", task.Priority)
	}
	if task.Recurrence != "every week" {
		t.Errorf("recurrence: %q", task.Recurrence)
	}
	if !reflect.DeepEqual(task.Tags, []string{"work", "docs/spec"}) {
		t.Errorf("tags: %v", task.Tags)
	}
	if !reflect.DeepEqual(task.Links, []string{"Project X", "Notes"}) {
		t.Errorf("links: %v", task.Links)
	}
	// Wikilink display text is kept inline (alias when present); dates,
	// priority, recurrence, and tags are stripped.
	if want := "write report Project X see here"; task.Text != want {
		t.Errorf("text: %q, want %q", task.Text, want)
	}
}

func TestParseDoneCancelledDates(t *testing.T) {
	content := "- [x] shipped ✅ 2026-06-12\n- [-] dropped ❌ 2026-06-13\n"
	got := Parse(content)
	if got[0].Done != "2026-06-12" {
		t.Errorf("done date: %q", got[0].Done)
	}
	if got[1].Cancelled != "2026-06-13" {
		t.Errorf("cancelled date: %q", got[1].Cancelled)
	}
}

func TestPriorityOrdering(t *testing.T) {
	// Obsidian order: Highest > High > Medium > None > Low > Lowest.
	order := []Priority{
		PriorityHighest, PriorityHigh, PriorityMedium,
		PriorityNone, PriorityLow, PriorityLowest,
	}
	for i := 1; i < len(order); i++ {
		if !(order[i-1] > order[i]) {
			t.Errorf("priority ordering broken at %d: %v !> %v", i, order[i-1], order[i])
		}
	}
	for _, p := range order {
		if PriorityFromString(p.String()) != p {
			t.Errorf("round-trip %v -> %q -> %v", p, p.String(), PriorityFromString(p.String()))
		}
	}
}

func TestPriorityHighestWins(t *testing.T) {
	// Multiple signifiers: highest precedence wins.
	got := Parse("- [ ] task 🔺 🔽\n")
	if got[0].Priority != PriorityHighest {
		t.Errorf("priority: %v, want highest", got[0].Priority)
	}
}

func TestSetStatusToDone(t *testing.T) {
	raw := "  - [ ] finish docs 📅 2026-06-20 #work"
	got := SetStatus(raw, StatusDone, "2026-06-16")
	// Marker flips, ✅ stamped, indentation + due + tag preserved.
	if want := "  - [x] finish docs 📅 2026-06-20 #work ✅ 2026-06-16"; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	// Re-parse must reflect the change.
	tasks := Parse(got)
	if tasks[0].Status != StatusDone || tasks[0].Done != "2026-06-16" {
		t.Errorf("re-parse: status=%s done=%s", tasks[0].Status, tasks[0].Done)
	}
}

func TestSetStatusReopenClearsDates(t *testing.T) {
	raw := "- [x] thing ✅ 2026-06-12"
	got := SetStatus(raw, StatusOpen, "")
	if want := "- [ ] thing"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetStatusCancelledSwapsDates(t *testing.T) {
	raw := "- [x] thing ✅ 2026-06-12"
	got := SetStatus(raw, StatusCancelled, "2026-06-16")
	if want := "- [-] thing ❌ 2026-06-16"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetDue(t *testing.T) {
	if got := SetDue("- [ ] thing", "2026-07-01"); got != "- [ ] thing 📅 2026-07-01" {
		t.Errorf("append due: %q", got)
	}
	if got := SetDue("- [ ] thing 📅 2026-06-20", "2026-07-01"); got != "- [ ] thing 📅 2026-07-01" {
		t.Errorf("replace due: %q", got)
	}
	if got := SetDue("- [ ] thing 📅 2026-06-20 #tag", ""); got != "- [ ] thing #tag" {
		t.Errorf("clear due: %q", got)
	}
}

func TestSetPriority(t *testing.T) {
	if got := SetPriority("- [ ] thing", PriorityHigh); got != "- [ ] thing ⏫" {
		t.Errorf("add priority: %q", got)
	}
	if got := SetPriority("- [ ] thing ⏫", PriorityLow); got != "- [ ] thing 🔽" {
		t.Errorf("change priority: %q", got)
	}
	if got := SetPriority("- [ ] thing ⏫", PriorityNone); got != "- [ ] thing" {
		t.Errorf("clear priority: %q", got)
	}
}

func TestSetText(t *testing.T) {
	// Trailing emoji-metadata is preserved; prose is replaced.
	if got := SetText("- [ ] old text 📅 2026-06-20 ⏫", "new text"); got != "- [ ] new text 📅 2026-06-20 ⏫" {
		t.Errorf("preserve metadata: %q", got)
	}
	// No metadata → whole body replaced (caller supplies tags).
	if got := SetText("  - [x] fix bug #old", "fix the bug properly #new"); got != "  - [x] fix the bug properly #new" {
		t.Errorf("no metadata: %q", got)
	}
	// Indentation and checkbox state preserved.
	if got := SetText("    - [/] a 🔼", "b"); got != "    - [/] b 🔼" {
		t.Errorf("indent/state: %q", got)
	}
}

func TestNewTaskLine(t *testing.T) {
	if got := NewTaskLine("buy milk 📅 2026-06-20", 2, StatusOpen); got != "  - [ ] buy milk 📅 2026-06-20" {
		t.Errorf("new task line: %q", got)
	}
}

func TestReplaceLine(t *testing.T) {
	content := "line1\nline2\nline3"
	got, err := ReplaceLine(content, 2, "line2", "LINE2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "line1\nLINE2\nline3" {
		t.Errorf("got %q", got)
	}
	// Stale guard fires when the current line differs.
	if _, err := ReplaceLine(content, 2, "stale", "x"); err == nil {
		t.Error("expected stale-line error")
	}
	if _, err := ReplaceLine(content, 99, "x", "y"); err == nil {
		t.Error("expected out-of-range error")
	}
}

func TestRoundTripFidelity(t *testing.T) {
	// Parsing then re-emitting an unchanged line via RawLine is identity.
	content := "  - [ ] task with 📅 2026-06-20 #tag [[link]] preserved exactly"
	tasks := Parse(content)
	if tasks[0].RawLine != content {
		t.Errorf("RawLine not preserved: %q", tasks[0].RawLine)
	}
}
