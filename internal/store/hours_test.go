// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for 🎯T202: attended and agent hours from indexed data.

func TestClassifyTranscriptText(t *testing.T) {
	human := &origin{Kind: "human"}
	notif := &origin{Kind: "task-notification"}
	cases := []struct {
		name   string
		e      hoursEntry
		head   string
		source string
		kind   string
	}{
		{"cli prompt", hoursEntry{Entrypoint: "cli", PromptSource: "typed", Origin: human}, "fix the build", HoursSourceTranscript, HoursKindPrompt},
		{"old cli prompt without provenance", hoursEntry{}, "yes", HoursSourceTranscript, HoursKindPrompt},
		{"desktop prompt", hoursEntry{Entrypoint: "claude-desktop", PromptSource: "sdk", Origin: human}, "why android?", HoursSourceTranscript, HoursKindPrompt},
		{"slash command", hoursEntry{Entrypoint: "cli"}, "<command-name>/cv</command-name>", HoursSourceTranscript, HoursKindCommand},
		{"interrupt", hoursEntry{Entrypoint: "cli"}, "[Request interrupted by user]", HoursSourceTranscript, HoursKindInterrupt},
		{"pasted prompt", hoursEntry{Entrypoint: "cli", Origin: human}, `<pasted_content id="1"> a log I found`, HoursSourceTranscript, HoursKindPrompt},
		{"t3-driven prompt", hoursEntry{Entrypoint: "t3-code", PromptSource: "sdk"}, "go", HoursSourceT3Transcript, HoursKindPrompt},

		{"task notification", hoursEntry{Entrypoint: "t3-code", PromptSource: "system", Origin: notif}, "<task-notification>", "", ""},
		{"notification in cli", hoursEntry{Entrypoint: "cli", Origin: notif}, "<task-notification> <task-id>", "", ""},
		{"peer message", hoursEntry{Entrypoint: "claude-desktop", PromptSource: "system", IsMeta: true, Origin: &origin{Kind: "peer"}}, "Another Claude session sent", "", ""},
		{"meta", hoursEntry{Entrypoint: "cli", IsMeta: true}, "Base directory for this skill", "", ""},
		{"compaction continuation", hoursEntry{Entrypoint: "t3-code", IsCompactSummary: true}, "This session is being continued", "", ""},
		{"sidechain", hoursEntry{Entrypoint: "cli", IsSidechain: true}, "do the thing", "", ""},
		{"sdk program", hoursEntry{Entrypoint: "sdk-cli", PromptSource: "sdk"}, "[mnemo:compactor:v1] You are a session compactor", "", ""},
		{"sdk title generation", hoursEntry{Entrypoint: "sdk-ts", PromptSource: "sdk"}, "Produce name and description", "", ""},
		{"command output", hoursEntry{Entrypoint: "cli"}, "<local-command-stdout>ok</local-command-stdout>", "", ""},
		{"jevons brief", hoursEntry{Entrypoint: "cli", PromptSource: "typed", Origin: human}, "[event: idle-nudge] NUDGE — you are phase=idle", "", ""},
		{"pasted jevons brief", hoursEntry{Entrypoint: "cli", PromptSource: "typed", Origin: human}, `<pasted_content id="e6e7"> [Jevons fleet standing brief — apply`, "", ""},
		{"claudia restart note", hoursEntry{Entrypoint: "cli", Origin: human}, "[claudia] The host restarted at 2026-09-30", "", ""},
		{"doit brief", hoursEntry{Entrypoint: "cli"}, "WORK SESSION CONTEXT:   Scope: doit smoke test", "", ""},
		{"t3 interrupt", hoursEntry{Entrypoint: "t3-code"}, "[Request interrupted by user for tool use]", "", ""},
		{"empty", hoursEntry{Entrypoint: "cli"}, "   ", "", ""},
	}
	for _, c := range cases {
		src, kind, ok := classifyTranscriptText(c.e, c.head)
		if c.source == "" {
			if ok {
				t.Errorf("%s: counted as %s/%s, want excluded", c.name, src, kind)
			}
			continue
		}
		if !ok || src != c.source || kind != c.kind {
			t.Errorf("%s: got %s/%s ok=%v, want %s/%s", c.name, src, kind, ok, c.source, c.kind)
		}
	}
}

// hoursFixtureTime is a fixed morning, well inside one local day in any
// zone the tests run in once Timezone is UTC.
var hoursFixtureTime = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func hts(min float64) string {
	return hoursFixtureTime.Add(time.Duration(min * float64(time.Minute))).Format("2006-01-02T15:04:05.000Z")
}

func hoursUser(uuid, text string, min float64, extra map[string]any) map[string]any {
	e := map[string]any{"type": "user", "uuid": uuid, "timestamp": hts(min), "message": map[string]any{"role": "user", "content": text}}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func hoursAssistant(uuid string, min float64, content []any) map[string]any {
	if content == nil {
		content = []any{map[string]any{"type": "text", "text": "working on it"}}
	}
	return map[string]any{"type": "assistant", "uuid": uuid, "timestamp": hts(min),
		"message": map[string]any{"role": "assistant", "model": "claude-opus-5", "content": content}}
}

func withCwd(e map[string]any, repo string) map[string]any {
	e["cwd"] = "/Users/someone/work/github.com/acme/" + repo
	return e
}

// t3Fixture builds a T3 state store shaped like statev2.sqlite.
type t3Fixture struct {
	t    *testing.T
	db   *sql.DB
	path string
}

func newT3Fixture(t *testing.T) *t3Fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "statev2.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE orchestration_v2_projection_messages (message_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL,
			run_id TEXT, node_id TEXT, role TEXT NOT NULL, streaming INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT '', payload_json TEXT NOT NULL)`,
		`CREATE TABLE orchestration_v2_projection_provider_threads (provider_thread_id TEXT PRIMARY KEY, thread_id TEXT,
			provider TEXT NOT NULL DEFAULT 'claudeAgent', status TEXT NOT NULL DEFAULT 'idle',
			first_run_ordinal INTEGER, last_run_ordinal INTEGER, updated_at TEXT NOT NULL DEFAULT '', payload_json TEXT NOT NULL)`,
		`CREATE TABLE orchestration_v2_projection_threads (thread_id TEXT PRIMARY KEY, project_id TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '', payload_json TEXT NOT NULL)`,
		`CREATE TABLE projection_projects (project_id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', workspace_root TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return &t3Fixture{t: t, db: db, path: path}
}

func (f *t3Fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *t3Fixture) thread(id, project, root, session, relationship string) {
	f.exec(`INSERT OR IGNORE INTO projection_projects (project_id, workspace_root) VALUES (?, ?)`, project, root)
	lineage, _ := json.Marshal(map[string]any{"lineage": map[string]any{"relationshipToParent": relationship}})
	if relationship == "" {
		lineage = []byte(`{"lineage":{"relationshipToParent":null}}`)
	}
	f.exec(`INSERT INTO orchestration_v2_projection_threads (thread_id, project_id, payload_json) VALUES (?, ?, ?)`, id, project, string(lineage))
	if session != "" {
		pt, _ := json.Marshal(map[string]any{"nativeThreadRef": map[string]any{"nativeId": session}})
		f.exec(`INSERT INTO orchestration_v2_projection_provider_threads (provider_thread_id, thread_id, payload_json) VALUES (?, ?, ?)`,
			"pt:"+id+":"+session, id, string(pt))
	}
}

func (f *t3Fixture) message(id, thread, createdBy, creationSource string, min float64) {
	payload, _ := json.Marshal(map[string]any{"createdBy": createdBy, "creationSource": creationSource, "text": "secret words " + id})
	f.exec(`INSERT INTO orchestration_v2_projection_messages (message_id, thread_id, role, created_at, payload_json)
		VALUES (?, ?, 'user', ?, ?)`, id, thread, hts(min), string(payload))
}

func hoursReport(t *testing.T, s *Store, p HoursParams) *HoursReport {
	t.Helper()
	if p.Since == "" && p.Days == 0 {
		p.Since = hoursFixtureTime.Add(-time.Hour).Format(time.RFC3339)
		p.Until = hoursFixtureTime.Add(10 * time.Hour).Format(time.RFC3339)
	}
	p.Timezone = "UTC"
	rep, err := s.Hours(p)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func repoRow(rep *HoursReport, repo string) (attended, agent float64, inputs int) {
	for _, r := range rep.ByRepo {
		if r.Repo == repo {
			return r.AttendedHours, r.AgentHours, r.Inputs
		}
	}
	return 0, 0, 0
}

// The whole path: transcripts and a T3 store in, a report out. T3's
// agent-authored and imported messages are excluded, transcript
// notifications and meta are excluded, an answer to an agent's question
// counts, and two threads worked at once count once.
func TestHoursEndToEnd(t *testing.T) {
	projectDir := t.TempDir()
	// A CLI session in alpha: two prompts, a notification, a meta line,
	// an AskUserQuestion and its answer, and agent output throughout.
	writeJSONL(t, projectDir, "-alpha", "sess-cli", []map[string]any{
		withCwd(hoursUser("u1", "start the migration", 0, map[string]any{"entrypoint": "cli", "promptSource": "typed", "origin": map[string]any{"kind": "human"}}), "alpha"),
		withCwd(hoursAssistant("a1", 0.5, nil), "alpha"),
		withCwd(hoursAssistant("a2", 1.5, []any{map[string]any{"type": "tool_use", "id": "toolu_q1", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{}}}}), "alpha"),
		withCwd(map[string]any{"type": "user", "uuid": "u-answer", "timestamp": hts(3), "entrypoint": "cli",
			"message":       map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_q1", "content": "The user answered: yes"}}},
			"toolUseResult": map[string]any{"answers": map[string]any{"q": "yes"}}}, "alpha"),
		withCwd(hoursAssistant("a3", 3.5, nil), "alpha"),
		withCwd(hoursUser("u-notif", "<task-notification> <task-id>b1</task-id>", 4, map[string]any{"entrypoint": "cli", "promptSource": "system", "origin": map[string]any{"kind": "task-notification"}}), "alpha"),
		withCwd(hoursUser("u-meta", "Base directory for this skill", 4.2, map[string]any{"entrypoint": "cli", "isMeta": true}), "alpha"),
		withCwd(hoursUser("u2", "now the tests", 6, map[string]any{"entrypoint": "cli", "promptSource": "typed", "origin": map[string]any{"kind": "human"}}), "alpha"),
		withCwd(hoursAssistant("a4", 6.5, nil), "alpha"),
	})
	// A T3-driven session in beta, interleaved with alpha.
	writeJSONL(t, projectDir, "-beta", "sess-t3", []map[string]any{
		withCwd(hoursUser("t1", "beta prompt", 2, map[string]any{"entrypoint": "t3-code", "promptSource": "sdk"}), "beta"),
		withCwd(hoursAssistant("b1", 2.5, nil), "beta"),
		withCwd(hoursUser("t2", "Delegated task brief", 5, map[string]any{"entrypoint": "t3-code", "promptSource": "sdk"}), "beta"),
		withCwd(hoursAssistant("b2", 5.5, nil), "beta"),
	})
	notes := hoursUser("n1", "jot this down", 40, map[string]any{"entrypoint": "cli"})
	notes["cwd"] = "/scratch/notes"
	writeJSONL(t, projectDir, "-notes", "sess-notes", []map[string]any{notes})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	t3 := newT3Fixture(t)
	t3.thread("th-beta", "p-beta", "/Users/someone/work/github.com/acme/beta", "sess-t3", "")
	t3.thread("th-import", "p-beta", "/Users/someone/work/github.com/acme/beta", "", "")
	t3.thread("th-sub", "p-beta", "/Users/someone/work/github.com/acme/beta", "", "subagent")
	t3.message("m-human", "th-beta", "user", "web", 2)         // counted
	t3.message("m-agent", "th-beta", "agent", "server", 5)     // delegation brief: excluded
	t3.message("m-import", "th-import", "user", "server", 4)   // imported notification: excluded
	t3.message("m-question", "th-beta", "user", "server", 4.5) // an agent's question: excluded
	t3.message("m-subagent", "th-sub", "user", "web", 4.7)     // subagent thread: excluded
	t3.message("m-mobile", "th-beta", "user", "mobile", 7)     // counted
	t3.message("m-unindexed", "th-import", "user", "web", 30)  // counted, repo from the project

	res, err := s.HoursPass(context.Background(), &HoursPassArgs{T3Store: t3.path})
	if err != nil {
		t.Fatal(err)
	}
	if res.T3Status != HoursT3OK || res.T3Inputs != 3 {
		t.Fatalf("t3 pass: status %s, inputs %d; want ok and 3", res.T3Status, res.T3Inputs)
	}

	rep := hoursReport(t, s, HoursParams{})
	if rep.InputsBySource[HoursSourceT3] != 3 || rep.InputsBySource[HoursSourceTranscript] != 4 {
		t.Errorf("inputs by source = %v, want t3=3 transcript=4", rep.InputsBySource)
	}
	if rep.InputsByKind[HoursKindAnswer] != 1 {
		t.Errorf("inputs by kind = %v, want one answer", rep.InputsByKind)
	}
	if rep.T3.TranscriptPrompts != 2 {
		t.Errorf("t3 transcript prompts = %d, want 2 (not counted, disclosed)", rep.T3.TranscriptPrompts)
	}
	// One block 0..7 (+2 lead, +5 tail) = 14 min, and one from 30 to 40
	// (28..45) = 17 min: alpha and beta were worked at once and count once.
	if got, want := rep.AttendedHours, round2(31.0/60); got != want {
		t.Errorf("attended = %.2f h, want %.2f h", got, want)
	}
	aAtt, aAgent, aIn := repoRow(rep, "acme/alpha")
	_, _, bIn := repoRow(rep, "acme/beta")
	if aIn != 3 || bIn != 3 {
		t.Errorf("inputs alpha=%d beta=%d, want 3 and 3", aIn, bIn)
	}
	var sum float64
	for _, r := range rep.ByRepo {
		sum += r.AttendedHours
	}
	if d := sum - rep.AttendedHours; d > 0.02 || d < -0.02 {
		t.Errorf("projects sum to %.2f h, report %.2f h", sum, rep.AttendedHours)
	}
	if aAgent <= 0 {
		t.Errorf("alpha agent hours = %v, want > 0", aAgent)
	}
	if len(rep.Degraded) != 0 {
		t.Errorf("degraded = %v, want none", rep.Degraded)
	}

	// No message text reaches the tables.
	var leaked int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM hours_inputs WHERE ref LIKE '%secret%' OR kind LIKE '%secret%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("T3 text in hours_inputs: %d rows (err %v)", leaked, err)
	}

	// A second pass changes nothing.
	again, err := s.HoursPass(context.Background(), &HoursPassArgs{T3Store: t3.path})
	if err != nil || again.Inputs != 0 {
		t.Errorf("second pass stored %d inputs (err %v), want 0", again.Inputs, err)
	}
	rep2 := hoursReport(t, s, HoursParams{})
	if rep2.AttendedHours != rep.AttendedHours || rep2.Inputs != rep.Inputs {
		t.Errorf("report changed on an idle pass: %+v vs %+v", rep2.Totals, rep.Totals)
	}

	// A session outside any repo is labelled by its directory.
	if _, _, n := repoRow(rep, "/scratch/notes"); n != 1 {
		t.Errorf("inputs under /scratch/notes = %d, want 1; rows %+v", n, rep.ByRepo)
	}

	// A repo filter keeps only that project.
	only := hoursReport(t, s, HoursParams{Repo: "alpha"})
	if len(only.ByRepo) != 1 || only.ByRepo[0].Repo != "acme/alpha" || only.AttendedHours != aAtt {
		t.Errorf("filtered = %+v, want alpha only with %.2f h", only.ByRepo, aAtt)
	}
}

func round2(h float64) float64 { return float64(int64(h*100+0.5)) / 100 }

// A T3 store whose tables are not what the pass expects is reported as
// degraded, by name, and the T3-driven prompts the transcripts saw are
// disclosed rather than silently reported as no work.
func TestHoursT3UnexpectedShapeIsDegraded(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "-beta", "sess-t3", []map[string]any{
		withCwd(hoursUser("t1", "beta prompt", 2, map[string]any{"entrypoint": "t3-code", "promptSource": "sdk"}), "beta"),
		withCwd(hoursAssistant("b1", 2.5, nil), "beta"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	t3 := newT3Fixture(t)
	t3.exec(`ALTER TABLE orchestration_v2_projection_messages RENAME COLUMN payload_json TO payload`)

	res, err := s.HoursPass(context.Background(), &HoursPassArgs{T3Store: t3.path})
	if err != nil {
		t.Fatal(err)
	}
	if res.T3Status != HoursT3Unexpected {
		t.Fatalf("status = %s, want %s", res.T3Status, HoursT3Unexpected)
	}
	rep := hoursReport(t, s, HoursParams{})
	if len(rep.Degraded) == 0 || !strings.Contains(rep.Degraded[0], "payload_json") || !strings.Contains(rep.Degraded[0], "1 prompts in T3-driven sessions") {
		t.Errorf("degraded = %q, want the missing column and the uncounted prompt named", rep.Degraded)
	}

	// T3 writing no authorship at all is also a shape problem.
	t3b := newT3Fixture(t)
	t3b.thread("th", "p", "/x", "sess-t3", "")
	t3b.exec(`INSERT INTO orchestration_v2_projection_messages (message_id, thread_id, role, created_at, payload_json)
		VALUES ('m1', 'th', 'user', ?, '{"text":"hi"}')`, hts(2))
	res, err = s.HoursPass(context.Background(), &HoursPassArgs{T3Store: t3b.path})
	if err != nil {
		t.Fatal(err)
	}
	if res.T3Status != HoursT3Unexpected {
		t.Errorf("status without createdBy = %s, want %s", res.T3Status, HoursT3Unexpected)
	}
}

// An absent T3 store with no T3-driven work in range is not degraded;
// with T3-driven work it is.
func TestHoursT3Absent(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "-alpha", "sess-cli", []map[string]any{
		withCwd(hoursUser("u1", "hello", 0, map[string]any{"entrypoint": "cli"}), "alpha"),
		withCwd(hoursAssistant("a1", 0.5, nil), "alpha"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "nope.sqlite")
	if _, err := s.HoursPass(context.Background(), &HoursPassArgs{T3Store: missing}); err != nil {
		t.Fatal(err)
	}
	rep := hoursReport(t, s, HoursParams{})
	if rep.T3.Status != HoursT3Absent || len(rep.Degraded) != 0 || rep.Inputs != 1 {
		t.Errorf("t3=%+v degraded=%v inputs=%d; want absent, none, 1", rep.T3, rep.Degraded, rep.Inputs)
	}

	appendJSONL(t, projectDir, "-alpha", "sess-cli", []map[string]any{
		withCwd(hoursUser("t1", "from t3", 3, map[string]any{"entrypoint": "t3-code", "promptSource": "sdk"}), "alpha"),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoursPass(context.Background(), &HoursPassArgs{T3Store: missing}); err != nil {
		t.Fatal(err)
	}
	rep = hoursReport(t, s, HoursParams{})
	if len(rep.Degraded) != 1 || !strings.Contains(rep.Degraded[0], "absent") {
		t.Errorf("degraded = %v, want the absent store named with its uncounted prompt", rep.Degraded)
	}
}

// A fork copies history into a new session; the copy's prompts are the
// same inputs and count once.
func TestHoursForkCountsOnce(t *testing.T) {
	projectDir := t.TempDir()
	lines := []map[string]any{
		withCwd(hoursUser("u1", "same prompt", 0, map[string]any{"entrypoint": "cli"}), "alpha"),
		withCwd(hoursAssistant("a1", 0.5, nil), "alpha"),
	}
	writeJSONL(t, projectDir, "-alpha", "sess-a", lines)
	writeJSONL(t, projectDir, "-alpha", "sess-b", lines)
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoursPass(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	rep := hoursReport(t, s, HoursParams{})
	if rep.Inputs != 1 {
		t.Errorf("inputs = %d, want 1", rep.Inputs)
	}
	if want := round2(7.0 / 60); rep.AttendedHours != want {
		t.Errorf("attended = %.2f, want %.2f", rep.AttendedHours, want)
	}
	if rep.Pending != 0 {
		t.Errorf("pending = %d after a pass, want 0", rep.Pending)
	}
}

// The report answers from the materialised tables in bounded time even
// with a year of activity: no transcript text is read.
func TestHoursReportIsFastOverAYear(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ctx := context.Background()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := hoursFixtureTime.AddDate(-1, 0, 0)
	// ~8 working hours a day of agent minutes over 3 sessions, and a
	// prompt every 4 minutes: about half a million minute rows.
	minStmt, err := tx.Prepare(`INSERT INTO hours_agent_minutes (session_id, minute, n) VALUES (?, ?, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	inStmt, err := tx.Prepare(`INSERT INTO hours_inputs (source, ref, kind, session_id, at_ms) VALUES ('t3', ?, 'prompt', ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for d := range 365 {
		day := start.AddDate(0, 0, d)
		for m := range 480 {
			at := day.Add(time.Duration(m) * time.Minute)
			for k := range 3 {
				if _, err := minStmt.Exec(fmt.Sprintf("s%d", k), at.Unix()/60); err != nil {
					t.Fatal(err)
				}
			}
			if m%4 == 0 {
				if _, err := inStmt.Exec(fmt.Sprintf("%d-%d", d, m), fmt.Sprintf("s%d", m%3), at.UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	rep := hoursReport(t, s, HoursParams{Since: hoursFixtureTime.AddDate(0, 0, -7).Format(time.RFC3339), Until: hoursFixtureTime.Format(time.RFC3339)})
	week := time.Since(t0)
	t0 = time.Now()
	rep = hoursReport(t, s, HoursParams{Since: start.Format(time.RFC3339), Until: hoursFixtureTime.Format(time.RFC3339)})
	year := time.Since(t0)
	t.Logf("week %v, year %v (%.0f attended h, %.0f agent h)", week, year, rep.AttendedHours, rep.AgentHours)
	if week > 500*time.Millisecond {
		t.Errorf("a week took %v, want well under a second", week)
	}
}
