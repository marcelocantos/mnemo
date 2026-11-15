// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Attended hours and agent hours per project (🎯T202).
//
// The arithmetic lives in internal/hours; this file decides what counts
// as a human input, materialises inputs and agent activity into small
// tables, and answers a report from those tables alone.
//
// Who typed a message is the hard part. Inside a Claude transcript, the
// owner's prompts and T3 Code's injected messages (delegation briefs,
// Slack inbox pushes, task notifications, messages from other threads)
// are indistinguishable: both arrive as promptSource "sdk". T3's own
// store records the author, so for T3-driven sessions it is the
// authority. Two fields matter, not one: T3 marks every message of an
// IMPORTED transcript createdBy = user, task notifications included, so
// a human input is createdBy = user AND creationSource = web or mobile,
// the two T3 clients a person types into. The 21 non-import rows with
// creationSource = server and createdBy = user on the owner's store are
// agents' questions, not prompts.
//
// Sessions T3 did not drive are classified from transcript provenance:
// isMeta, isCompactSummary, isSidechain, origin.kind, promptSource and
// entrypoint. An SDK entrypoint is a program, never a person, unless
// the entry says origin.kind = human (the desktop app does). Orchestrators
// that type into a terminal session (Jevons, claudia) leave no provenance
// at all — their briefs carry promptSource "typed" and origin "human" —
// so their openers are listed in machineDriverPrefixes and excluded. That
// is wording, and wording drifts; the list is deliberately narrow and
// only applies where no structural signal exists.
//
// Answers to an agent's question (an AskUserQuestion tool result) are
// human input in any session: T3 does not record them as messages.
//
// Codex, Grok, Cursor and Oh My Pi sessions outside T3 have no comparable
// provenance in what mnemo indexes (Codex's session_meta line, which
// names the originator, is not stored), and on the owner's machine most
// of them were orchestrated — 16,561 "user" messages in October alone.
// They contribute agent hours only, and a report says how many there
// were.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/hours"
)

// Sources of a human input, stored in hours_inputs.source.
const (
	HoursSourceT3         = "t3"
	HoursSourceTranscript = "transcript"
	// HoursSourceT3Transcript is a transcript's copy of a prompt in a
	// session T3 drove. Never counted: it includes T3's injected
	// messages. Kept so a report can name the T3-driven work it could
	// not attribute when the T3 store is unavailable.
	HoursSourceT3Transcript = "t3-transcript"
)

// Kinds of human input, stored in hours_inputs.kind.
const (
	HoursKindPrompt    = "prompt"
	HoursKindCommand   = "command"
	HoursKindAnswer    = "answer"
	HoursKindInterrupt = "interrupt"
)

// T3 store status, as reported.
const (
	HoursT3OK          = "ok"
	HoursT3Absent      = "absent"
	HoursT3Unreadable  = "unreadable"
	HoursT3Unexpected  = "unexpected_shape"
	HoursT3NotScanned  = "not_scanned"
	hoursStreamMessage = "messages"
	hoursStreamT3      = "t3"
)

// t3Entrypoint is the Claude transcript entrypoint of a T3-driven turn.
const t3Entrypoint = "t3-code"

// t3HumanCreationSources are the T3 clients a person types into.
var t3HumanCreationSources = map[string]bool{"web": true, "mobile": true}

// machineDriverPrefixes open messages that orchestrators type into
// terminal sessions. Matched after leading whitespace and an optional
// <pasted_content> opener. Measured on the owner's transcripts: every
// opener here is a Jevons or claudia template, and together they were
// most of the "typed, human" prompts in August 2026.
var machineDriverPrefixes = []string{
	"[Who you are — from the fleet registry",
	"[Session handover — you have been restarted",
	"[Jevons fleet standing brief",
	"[event: ",
	"[claudia] ",
	"[overseer] ",
	"[routed to overseer]",
	"[Agent jevons-",
	"[Provider switch",
	"[user] Reply with exactly",
	"Continue the open objective",
	"Write a short brief of in-flight",
	"PO rehydrated.",
	"=== CONVERSATION CONTEXT",
	"=== DELEGATION POLICY",
	"WORK SESSION CONTEXT:", // doit's injected brief, April 2026
}

// harnessOutputPrefixes open user-role text that is the harness reporting
// back, not a person writing.
var harnessOutputPrefixes = []string{
	"<local-command-stdout>",
	"<local-command-stderr>",
	"<local-command-caveat>",
	"<bash-stdout>",
	"<bash-stderr>",
	"<task-notification>",
	"<system-reminder>",
	"Caveat: The messages below were generated",
}

var pastedOpenerRE = regexp.MustCompile(`^<pasted_content[^>]*>\s*`)

// hoursEntry is the provenance of a transcript entry.
type hoursEntry struct {
	UUID             string  `json:"uuid"`
	Entrypoint       string  `json:"entrypoint"`
	PromptSource     string  `json:"promptSource"`
	IsMeta           bool    `json:"isMeta"`
	IsCompactSummary bool    `json:"isCompactSummary"`
	IsSidechain      bool    `json:"isSidechain"`
	Origin           *origin `json:"origin"`
}

type origin struct {
	Kind string `json:"kind"`
}

// classifyTranscriptText decides whether a user-role text message in a
// Claude transcript is a human input. It returns the source and kind to
// store, or ok=false when the message is machine-generated.
func classifyTranscriptText(e hoursEntry, head string) (source, kind string, ok bool) {
	if e.IsSidechain || e.IsMeta || e.IsCompactSummary {
		return "", "", false
	}
	if e.Origin != nil && e.Origin.Kind != "human" {
		return "", "", false
	}
	if e.PromptSource == "system" {
		return "", "", false
	}
	text := strings.TrimSpace(head)
	text = pastedOpenerRE.ReplaceAllString(text, "")
	if text == "" {
		return "", "", false
	}
	for _, p := range harnessOutputPrefixes {
		if strings.HasPrefix(text, p) {
			return "", "", false
		}
	}
	source = HoursSourceTranscript
	switch {
	case e.Entrypoint == t3Entrypoint:
		source = HoursSourceT3Transcript
	case strings.HasPrefix(e.Entrypoint, "sdk") && (e.Origin == nil || e.Origin.Kind != "human"):
		return "", "", false
	}
	for _, p := range machineDriverPrefixes {
		if strings.HasPrefix(text, p) {
			return "", "", false
		}
	}
	switch {
	case strings.HasPrefix(text, "[Request interrupted by user"):
		kind = HoursKindInterrupt
	case strings.HasPrefix(text, "<command-name>"), strings.HasPrefix(text, "<command-message>"):
		kind = HoursKindCommand
	default:
		kind = HoursKindPrompt
	}
	if source == HoursSourceT3Transcript && kind == HoursKindInterrupt {
		// T3 interrupts threads programmatically too; not evidence of a
		// person.
		return "", "", false
	}
	return source, kind, true
}

// hoursBatchSize bounds one materialisation transaction. Rows are small
// (no text is carried except a 160-character head of user text), so the
// batch is large.
const hoursBatchSize = 5000

// hoursYield is the pause between committed batches, so a backfill over
// millions of messages does not pin the writer against live ingest.
const hoursYield = 20 * time.Millisecond

// hoursHeadChars is how much of a user message the classifier sees.
const hoursHeadChars = 160

const hoursMessagesSQL = `
	SELECT m.id, m.session_id, m.role, m.content_type, COALESCE(m.timestamp, ''),
		COALESCE(sm.source, 'claude'),
		CASE WHEN m.role = 'user' AND m.content_type = 'text' AND COALESCE(sm.source, 'claude') = 'claude'
			THEN substr(mnemo_text(m.text, m.text_z), 1, ?) END,
		CASE WHEN m.role = 'user' AND m.content_type = 'text' AND COALESCE(sm.source, 'claude') = 'claude'
			THEN json(e.raw) END,
		CASE WHEN m.role = 'user' AND m.content_type = 'tool_result' AND m.is_error = 0
				AND m.tool_use_id IS NOT NULL AND m.tool_use_id != ''
				AND EXISTS (SELECT 1 FROM messages q WHERE q.tool_use_id = m.tool_use_id
					AND q.role = 'assistant' AND q.tool_name = 'AskUserQuestion')
			THEN COALESCE(e.uuid, '') END
	FROM messages m
	LEFT JOIN session_meta sm ON sm.session_id = m.session_id
	LEFT JOIN entries_v e ON e.id = m.entry_id
	WHERE m.id > ?
	ORDER BY m.id LIMIT ?`

// HoursPassResult reports what one materialisation pass did.
type HoursPassResult struct {
	Messages     int64  // messages examined
	Inputs       int64  // human inputs stored (all sources)
	AgentMinutes int64  // session-minutes touched
	T3Inputs     int64  // of Inputs, from the T3 store
	T3Status     string // HoursT3*
}

// HoursPassArgs configures one pass.
type HoursPassArgs struct {
	// T3Store is the path of T3 Code's state database. Empty means T3
	// is not configured on this machine.
	T3Store string
}

// HoursPass brings the hours tables up to date: every message after the
// cursor, then every T3 message after its cursor. Idempotent and
// resumable; a pass with nothing new is two indexed probes and one
// read-only open of the T3 store.
func (s *Store) HoursPass(ctx context.Context, args *HoursPassArgs) (HoursPassResult, error) {
	var res HoursPassResult
	if !s.Have(CapSchemaCurrent) {
		return res, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n, inputs, minutes, err := s.hoursMessagesBatch(ctx)
		if err != nil {
			return res, fmt.Errorf("hours: messages: %w", err)
		}
		res.Messages += int64(n)
		res.Inputs += inputs
		res.AgentMinutes += minutes
		if n < hoursBatchSize {
			break
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(hoursYield):
		}
	}
	path := ""
	if args != nil {
		path = args.T3Store
	}
	status, n, err := s.hoursT3Pass(ctx, path)
	if err != nil {
		return res, fmt.Errorf("hours: t3: %w", err)
	}
	res.T3Status = status
	res.T3Inputs = n
	res.Inputs += n
	return res, nil
}

// hoursCursor reads a stream's cursor and status.
func (s *Store) hoursCursor(ctx context.Context, stream string) (int64, string, error) {
	var cur int64
	var status string
	err := s.readDB.QueryRowContext(ctx, `SELECT cursor, status FROM hours_progress WHERE stream = ?`, stream).Scan(&cur, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return cur, status, err
}

func setHoursCursor(ctx context.Context, tx *sql.Tx, stream string, cursor int64, status string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO hours_progress (stream, cursor, status, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(stream) DO UPDATE SET cursor = excluded.cursor, status = excluded.status,
			updated_at = excluded.updated_at`,
		stream, cursor, status, time.Now().UTC().Format(time.RFC3339))
	return err
}

type hoursInputRow struct {
	source, ref, kind, session, repoHint string
	atMS                                 int64
}

// hoursMessagesBatch materialises one batch of messages after the cursor.
func (s *Store) hoursMessagesBatch(ctx context.Context) (n int, inputs, minutes int64, err error) {
	cursor, _, err := s.hoursCursor(ctx, hoursStreamMessage)
	if err != nil {
		return 0, 0, 0, err
	}
	rows, err := s.readDB.QueryContext(ctx, hoursMessagesSQL, hoursHeadChars, cursor, hoursBatchSize)
	if err != nil {
		return 0, 0, 0, err
	}
	var ins []hoursInputRow
	type sm struct {
		session string
		minute  int64
	}
	mins := map[sm]int64{}
	last := cursor
	for rows.Next() {
		var (
			id                    int64
			session, role, ct, ts string
			source                string
			head, raw, answerUUID sql.NullString
		)
		if err := rows.Scan(&id, &session, &role, &ct, &ts, &source, &head, &raw, &answerUUID); err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		n++
		last = id
		at, ok := parseHoursTime(ts)
		if !ok {
			continue
		}
		switch {
		case answerUUID.Valid:
			ref := answerUUID.String
			if ref == "" {
				ref = "message:" + strconv.FormatInt(id, 10)
			}
			ins = append(ins, hoursInputRow{source: HoursSourceTranscript, ref: ref, kind: HoursKindAnswer, session: session, atMS: at.UnixMilli()})
		case role == "assistant" || (role == "user" && ct == "tool_result"):
			mins[sm{session, at.Unix() / 60}]++
		case role == "user" && ct == "text" && raw.Valid:
			var e hoursEntry
			if json.Unmarshal([]byte(raw.String), &e) != nil {
				continue
			}
			src, kind, ok := classifyTranscriptText(e, head.String)
			if !ok {
				continue
			}
			ref := e.UUID
			if ref == "" {
				ref = "message:" + strconv.FormatInt(id, 10)
			}
			ins = append(ins, hoursInputRow{source: src, ref: ref, kind: kind, session: session, atMS: at.UnixMilli()})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, 0, err
	}
	rows.Close()
	if n == 0 {
		return 0, 0, 0, nil
	}

	s.schemaMu.Lock()
	s.schemaMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	for _, r := range ins {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO hours_inputs (source, ref, kind, session_id, repo_hint, at_ms)
			VALUES (?, ?, ?, ?, ?, ?)`, r.source, r.ref, r.kind, r.session, r.repoHint, r.atMS)
		if err != nil {
			return 0, 0, 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			inputs++
		}
	}
	for k, c := range mins {
		if _, err = tx.ExecContext(ctx, `INSERT INTO hours_agent_minutes (session_id, minute, n) VALUES (?, ?, ?)
			ON CONFLICT(session_id, minute) DO UPDATE SET n = n + excluded.n`, k.session, k.minute, c); err != nil {
			return 0, 0, 0, err
		}
	}
	if err = setHoursCursor(ctx, tx, hoursStreamMessage, last, ""); err != nil {
		return 0, 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	return n, inputs, int64(len(mins)), nil
}

func parseHoursTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// DefaultT3StorePath is where T3 Code keeps its state on this machine.
func DefaultT3StorePath(home string) string {
	return filepath.Join(home, ".t3", "userdata", "statev2.sqlite")
}

// t3Shape lists the T3 tables and columns the pass reads. Anything
// missing is reported as an unexpected shape rather than read around.
var t3Shape = map[string][]string{
	"orchestration_v2_projection_messages":         {"message_id", "thread_id", "run_id", "role", "created_at", "payload_json"},
	"orchestration_v2_projection_provider_threads": {"thread_id", "payload_json", "first_run_ordinal", "last_run_ordinal", "updated_at"},
	"orchestration_v2_projection_threads":          {"thread_id", "project_id", "payload_json"},
	"projection_projects":                          {"project_id", "workspace_root"},
}

// t3Batch bounds one read of T3 messages.
const t3Batch = 2000

var runOrdinalRE = regexp.MustCompile(`:ordinal:(\d+)$`)

type t3ProviderThread struct {
	native      string
	first, last sql.NullInt64
	updatedAt   string
}

// hoursT3Pass materialises human inputs from T3's store. It returns the
// store's status and the number of inputs stored. A store that cannot
// be read is a status, not an error: the report says so.
func (s *Store) hoursT3Pass(ctx context.Context, path string) (string, int64, error) {
	cursor, _, err := s.hoursCursor(ctx, hoursStreamT3)
	if err != nil {
		return "", 0, err
	}
	record := func(status, note string, c int64) (string, int64, error) {
		tx, err := s.writeDB.BeginTx(ctx, nil)
		if err != nil {
			return "", 0, err
		}
		text := status
		if note != "" {
			text = status + ": " + note
		}
		if err := setHoursCursor(ctx, tx, hoursStreamT3, c, text); err != nil {
			tx.Rollback()
			return "", 0, err
		}
		return status, 0, tx.Commit()
	}
	if path == "" {
		return record(HoursT3Absent, "no T3 store configured", cursor)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return record(HoursT3Absent, path+" does not exist", cursor)
		}
		return record(HoursT3Unreadable, err.Error(), cursor)
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return record(HoursT3Unreadable, err.Error(), cursor)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	for table, cols := range t3Shape {
		have := map[string]bool{}
		rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return record(HoursT3Unreadable, err.Error(), cursor)
		}
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil {
				have[c] = true
			}
		}
		rows.Close()
		var missing []string
		for _, c := range cols {
			if !have[c] {
				missing = append(missing, c)
			}
		}
		if len(have) == 0 {
			return record(HoursT3Unexpected, "table "+table+" is missing", cursor)
		}
		if len(missing) > 0 {
			return record(HoursT3Unexpected, table+" lacks "+strings.Join(missing, ", "), cursor)
		}
	}
	// Authorship must actually be recorded. Sample the newest user
	// messages: a T3 that stopped writing createdBy or creationSource
	// would otherwise read as an owner who stopped typing.
	var sample, withAuthor, withSource int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(json_extract(payload_json, '$.createdBy') IS NOT NULL), 0),
			COALESCE(SUM(json_extract(payload_json, '$.creationSource') IS NOT NULL), 0)
		FROM (SELECT payload_json FROM orchestration_v2_projection_messages
			WHERE role = 'user' ORDER BY rowid DESC LIMIT 200)`).Scan(&sample, &withAuthor, &withSource); err != nil {
		return record(HoursT3Unreadable, err.Error(), cursor)
	}
	if sample > 0 && withAuthor == 0 {
		return record(HoursT3Unexpected, "user messages carry no createdBy", cursor)
	}
	if sample > 0 && withSource == 0 {
		return record(HoursT3Unexpected, "user messages carry no creationSource", cursor)
	}

	// Projects and threads: the repo when the session is not indexed,
	// and which threads are subagents.
	projects := map[string]string{}
	if rows, err := db.QueryContext(ctx, `SELECT project_id, workspace_root FROM projection_projects`); err == nil {
		for rows.Next() {
			var id, root string
			if rows.Scan(&id, &root) == nil {
				projects[id] = root
			}
		}
		rows.Close()
	} else {
		return record(HoursT3Unreadable, err.Error(), cursor)
	}
	threadRepo := map[string]string{}
	subagent := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT thread_id, project_id,
		COALESCE(json_extract(payload_json, '$.lineage.relationshipToParent'), '')
		FROM orchestration_v2_projection_threads`)
	if err != nil {
		return record(HoursT3Unreadable, err.Error(), cursor)
	}
	for rows.Next() {
		var id, project, rel string
		if rows.Scan(&id, &project, &rel) == nil {
			threadRepo[id] = projectLabel(extractRepo(projects[project]), projects[project])
			subagent[id] = rel == "subagent"
		}
	}
	rows.Close()
	providers := map[string][]t3ProviderThread{}
	rows, err = db.QueryContext(ctx, `SELECT COALESCE(thread_id, ''),
		COALESCE(json_extract(payload_json, '$.nativeThreadRef.nativeId'), ''),
		first_run_ordinal, last_run_ordinal, updated_at
		FROM orchestration_v2_projection_provider_threads`)
	if err != nil {
		return record(HoursT3Unreadable, err.Error(), cursor)
	}
	for rows.Next() {
		var thread string
		var p t3ProviderThread
		if rows.Scan(&thread, &p.native, &p.first, &p.last, &p.updatedAt) == nil && thread != "" && p.native != "" {
			providers[thread] = append(providers[thread], p)
		}
	}
	rows.Close()

	// A rebuilt store renumbers its rows; start over (inputs are
	// idempotent on T3's message id).
	var maxRow int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM orchestration_v2_projection_messages`).Scan(&maxRow); err != nil {
		return record(HoursT3Unreadable, err.Error(), cursor)
	}
	if maxRow < cursor {
		cursor = 0
	}

	var stored int64
	for {
		if err := ctx.Err(); err != nil {
			return "", stored, err
		}
		rows, err := db.QueryContext(ctx, `
			SELECT rowid, message_id, thread_id, COALESCE(run_id, ''), created_at,
				COALESCE(json_extract(payload_json, '$.createdBy'), ''),
				COALESCE(json_extract(payload_json, '$.creationSource'), '')
			FROM orchestration_v2_projection_messages
			WHERE rowid > ? AND role = 'user'
			ORDER BY rowid LIMIT ?`, cursor, t3Batch)
		if err != nil {
			return record(HoursT3Unreadable, err.Error(), cursor)
		}
		var ins []hoursInputRow
		n := 0
		last := cursor
		for rows.Next() {
			var rowid int64
			var id, thread, run, created, by, src string
			if err := rows.Scan(&rowid, &id, &thread, &run, &created, &by, &src); err != nil {
				rows.Close()
				return record(HoursT3Unreadable, err.Error(), cursor)
			}
			n++
			last = rowid
			if by != "user" || !t3HumanCreationSources[src] || subagent[thread] {
				continue
			}
			at, ok := parseHoursTime(created)
			if !ok {
				continue
			}
			ins = append(ins, hoursInputRow{
				source: HoursSourceT3, ref: id, kind: HoursKindPrompt,
				session: t3SessionFor(providers[thread], run), repoHint: threadRepo[thread], atMS: at.UnixMilli(),
			})
		}
		rows.Close()
		if n == 0 {
			break
		}
		k, err := s.storeT3Batch(ctx, ins, last, nil, nil)
		if err != nil {
			return "", stored, err
		}
		stored += k
		cursor = last
		if n < t3Batch {
			break
		}
	}
	// Even with no new messages, refresh the session map and the status.
	if _, err := s.storeT3Batch(ctx, nil, cursor, providers, threadRepo); err != nil {
		return "", stored, err
	}
	return HoursT3OK, stored, nil
}

// t3SessionFor picks the provider thread (and so the Claude, Codex or
// Grok session) that handled a run. A thread changes provider thread
// on a handoff or fork; the run's ordinal says which one was live.
func t3SessionFor(ps []t3ProviderThread, run string) string {
	if len(ps) == 0 {
		return ""
	}
	if len(ps) == 1 {
		return ps[0].native
	}
	if m := runOrdinalRE.FindStringSubmatch(run); m != nil {
		ord, _ := strconv.ParseInt(m[1], 10, 64)
		for _, p := range ps {
			if p.first.Valid && p.last.Valid && p.first.Int64 <= ord && ord <= p.last.Int64 {
				return p.native
			}
		}
	}
	best := ps[0]
	for _, p := range ps[1:] {
		if p.updatedAt > best.updatedAt {
			best = p
		}
	}
	return best.native
}

// storeT3Batch stores T3 inputs and advances the cursor; with providers
// set it also refreshes the map of sessions T3 drove.
func (s *Store) storeT3Batch(ctx context.Context, ins []hoursInputRow, cursor int64, providers map[string][]t3ProviderThread, threadRepo map[string]string) (stored int64, err error) {
	s.schemaMu.Lock()
	s.schemaMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	for _, r := range ins {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO hours_inputs (source, ref, kind, session_id, repo_hint, at_ms)
			VALUES (?, ?, ?, ?, ?, ?)`, r.source, r.ref, r.kind, r.session, r.repoHint, r.atMS)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			stored++
		}
	}
	if providers != nil {
		for thread, ps := range providers {
			for _, p := range ps {
				if _, err = tx.ExecContext(ctx, `INSERT INTO hours_t3_sessions (session_id, thread_id, repo_hint) VALUES (?, ?, ?)
					ON CONFLICT(session_id) DO UPDATE SET thread_id = excluded.thread_id, repo_hint = excluded.repo_hint`,
					p.native, thread, threadRepo[thread]); err != nil {
					return 0, err
				}
			}
		}
	}
	if err = setHoursCursor(ctx, tx, hoursStreamT3, cursor, HoursT3OK); err != nil {
		return 0, err
	}
	return stored, tx.Commit()
}

// RunHoursWorker keeps the hours tables current until ctx ends. t3Store
// is consulted on every pass, so a config change takes effect without a
// restart.
func (s *Store) RunHoursWorker(ctx context.Context, interval time.Duration, t3Store func() string) {
	s.Await(CapSchemaCurrent)
	for {
		res, err := s.HoursPass(ctx, &HoursPassArgs{T3Store: t3Store()})
		if err != nil && ctx.Err() == nil {
			slog.Warn("hours: pass failed", "err", err)
		} else if res.Inputs > 0 || res.Messages > hoursBatchSize {
			slog.Info("hours: materialised", "messages", res.Messages, "inputs", res.Inputs,
				"t3_inputs", res.T3Inputs, "agent_minutes", res.AgentMinutes, "t3", res.T3Status)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// HoursParams selects a report.
type HoursParams struct {
	// Days is the window ending now, in whole local days (default 7).
	// Ignored when Since is set.
	Days int
	// Since and Until bound the range: RFC3339, or a local date
	// (YYYY-MM-DD). Until defaults to now.
	Since, Until string
	// Repo filters projects by substring.
	Repo string
	// Timezone names the zone days are cut in (default: the daemon's).
	Timezone string
	// GroupBy "repo_day" adds per-project-per-day rows.
	GroupBy string
	// Model parameters in minutes; zero means the default.
	GapMinutes, LeadMinutes, TailMinutes, AgentGapMinutes float64
}

// HoursReport is the answer to mnemo_hours.
type HoursReport struct {
	Since    string          `json:"since"`
	Until    string          `json:"until"`
	Timezone string          `json:"timezone"`
	Repo     string          `json:"repo_filter,omitempty"`
	Params   HoursParamsUsed `json:"params"`
	*hours.Result
	// InputsBySource counts the human inputs in range by where they came
	// from ("t3", "transcript").
	InputsBySource map[string]int `json:"inputs_by_source"`
	// InputsByKind counts them by kind.
	InputsByKind map[string]int `json:"inputs_by_kind"`
	T3           HoursT3Report  `json:"t3"`
	// AgentOnlySessions counts sessions in range that had agent
	// activity but whose human input cannot be determined: non-Claude
	// sessions T3 did not drive.
	AgentOnlySessions int `json:"agent_only_sessions"`
	// Pending is the number of indexed messages the worker has not yet
	// materialised; a report is complete only when it is zero.
	Pending  int64    `json:"pending_messages"`
	Degraded []string `json:"degraded,omitempty"`
	Notes    []string `json:"notes"`
}

// HoursParamsUsed discloses the model, in minutes.
type HoursParamsUsed struct {
	GapMinutes      float64 `json:"gap_minutes"`
	LeadMinutes     float64 `json:"lead_minutes"`
	TailMinutes     float64 `json:"tail_minutes"`
	AgentGapMinutes float64 `json:"agent_gap_minutes"`
}

// HoursT3Report is the state of the T3 source.
type HoursT3Report struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	// TranscriptPrompts counts prompts in T3-driven sessions seen in the
	// transcripts in range. Not counted as human input (they include
	// T3's own injected messages); when the T3 store is unavailable they
	// are the T3 work this report could not attribute.
	TranscriptPrompts int `json:"transcript_prompts"`
}

func minutesOr(v float64, def time.Duration) time.Duration {
	if v > 0 {
		return time.Duration(v * float64(time.Minute))
	}
	return def
}

// hoursPad is how far beyond the range events are loaded, so blocks and
// agent intervals that cross a boundary are shaped by both sides.
const hoursPad = 2 * time.Hour

// Hours answers a report from the materialised tables.
func (s *Store) Hours(p HoursParams) (*HoursReport, error) {
	loc := time.Local
	if p.Timezone != "" {
		l, err := time.LoadLocation(p.Timezone)
		if err != nil {
			return nil, fmt.Errorf("timezone %q: %w", p.Timezone, err)
		}
		loc = l
	}
	now := time.Now().In(loc)
	parse := func(v string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, nil
		}
		return time.ParseInLocation(time.DateOnly, v, loc)
	}
	until := now
	if p.Until != "" {
		t, err := parse(p.Until)
		if err != nil {
			return nil, fmt.Errorf("until %q: want RFC3339 or YYYY-MM-DD", p.Until)
		}
		until = t
	}
	var since time.Time
	if p.Since != "" {
		t, err := parse(p.Since)
		if err != nil {
			return nil, fmt.Errorf("since %q: want RFC3339 or YYYY-MM-DD", p.Since)
		}
		since = t
	} else {
		days := p.Days
		if days <= 0 {
			days = 7
		}
		u := until.In(loc)
		since = time.Date(u.Year(), u.Month(), u.Day()-days+1, 0, 0, 0, 0, loc)
	}
	if !until.After(since) {
		return nil, fmt.Errorf("empty range: since %s is not before until %s", since.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	params := hours.Params{
		Gap:      minutesOr(p.GapMinutes, hours.DefaultGap),
		Lead:     minutesOr(p.LeadMinutes, hours.DefaultLead),
		Tail:     minutesOr(p.TailMinutes, hours.DefaultTail),
		AgentGap: minutesOr(p.AgentGapMinutes, hours.DefaultAgentGap),
	}
	lo, hi := since.Add(-hoursPad), until.Add(hoursPad)

	rep := &HoursReport{
		Since: since.In(loc).Format(time.RFC3339), Until: until.In(loc).Format(time.RFC3339),
		Timezone: loc.String(), Repo: p.Repo,
		Params: HoursParamsUsed{
			GapMinutes: params.Gap.Minutes(), LeadMinutes: params.Lead.Minutes(),
			TailMinutes: params.Tail.Minutes(), AgentGapMinutes: params.AgentGap.Minutes(),
		},
		InputsBySource: map[string]int{}, InputsByKind: map[string]int{},
	}

	// Human inputs, with the session's repo (or T3's hint).
	var inputs []hours.Input
	rows, err := s.readDB.Query(`
		SELECT i.source, i.kind, i.session_id, i.at_ms,
			COALESCE(NULLIF(sm.repo, ''), NULLIF(i.repo_hint, ''), NULLIF(t.repo_hint, ''), ''),
			COALESCE(sm.cwd, '')
		FROM hours_inputs i
		LEFT JOIN session_meta sm ON sm.session_id = i.session_id
		LEFT JOIN hours_t3_sessions t ON t.session_id = i.session_id
		WHERE i.at_ms >= ? AND i.at_ms < ?`, lo.UnixMilli(), hi.UnixMilli())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src, kind, session, repo, cwd string
		var at int64
		if err := rows.Scan(&src, &kind, &session, &at, &repo, &cwd); err != nil {
			rows.Close()
			return nil, err
		}
		inRange := at >= since.UnixMilli() && at < until.UnixMilli()
		if src == HoursSourceT3Transcript {
			if inRange {
				rep.T3.TranscriptPrompts++
			}
			continue
		}
		repo = projectLabel(repo, cwd)
		inputs = append(inputs, hours.Input{At: time.UnixMilli(at), Session: session, Repo: repo})
		if inRange {
			rep.InputsBySource[src]++
			rep.InputsByKind[kind]++
		}
	}
	rows.Close()

	// Agent minutes. mnemo's own background agents (the compactor, the
	// describers) are not the owner's work and are left out.
	var minutes []hours.AgentMinute
	agentOnly := map[string]bool{}
	rows, err = s.readDB.Query(`
		SELECT a.minute, a.session_id, COALESCE(NULLIF(sm.repo, ''), t.repo_hint, ''), COALESCE(sm.cwd, ''),
			COALESCE(sm.source, 'claude'),
			t.session_id IS NOT NULL
		FROM hours_agent_minutes a
		LEFT JOIN session_meta sm ON sm.session_id = a.session_id
		LEFT JOIN hours_t3_sessions t ON t.session_id = a.session_id
		WHERE a.minute >= ? AND a.minute < ? AND COALESCE(sm.compactor_internal, 0) = 0`,
		lo.Unix()/60, hi.Unix()/60+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m hours.AgentMinute
		var source, cwd string
		var viaT3 bool
		if err := rows.Scan(&m.Minute, &m.Session, &m.Repo, &cwd, &source, &viaT3); err != nil {
			rows.Close()
			return nil, err
		}
		m.Repo = projectLabel(m.Repo, cwd)
		minutes = append(minutes, m)
		if source != "claude" && !viaT3 && m.Minute >= since.Unix()/60 && m.Minute < until.Unix()/60 {
			agentOnly[m.Session] = true
		}
	}
	rows.Close()
	rep.AgentOnlySessions = len(agentOnly)

	res := hours.Compute(params, since, until, loc, inputs, minutes)
	if p.Repo != "" {
		res = filterHours(res, p.Repo)
		rep.Notes = append(rep.Notes, "Filtered: totals and days sum the matching projects; agent time on two matching projects at once counts for each.")
	}
	if p.GroupBy != "repo_day" {
		res.ByRepoDay = nil
	}
	rep.Result = res

	// The T3 source's state.
	_, status, err := s.hoursCursor(context.Background(), hoursStreamT3)
	if err != nil {
		return nil, err
	}
	if status == "" {
		status = HoursT3NotScanned
	}
	rep.T3.Status, rep.T3.Note, _ = strings.Cut(status, ": ")
	if rep.T3.Status != HoursT3OK && rep.T3.TranscriptPrompts > 0 {
		rep.Degraded = append(rep.Degraded, fmt.Sprintf(
			"T3 store %s (%s): %d prompts in T3-driven sessions in this range are not counted as attended time",
			rep.T3.Status, rep.T3.Note, rep.T3.TranscriptPrompts))
	} else if rep.T3.Status != HoursT3OK && rep.T3.Status != HoursT3Absent {
		rep.Degraded = append(rep.Degraded, fmt.Sprintf("T3 store %s (%s)", rep.T3.Status, rep.T3.Note))
	}
	cursor, _, err := s.hoursCursor(context.Background(), hoursStreamMessage)
	if err != nil {
		return nil, err
	}
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id > ?`, cursor).Scan(&rep.Pending); err != nil {
		return nil, err
	}
	if rep.Pending > 0 {
		rep.Degraded = append(rep.Degraded, fmt.Sprintf("%d indexed messages not yet materialised; figures may be low", rep.Pending))
	}
	if rep.AgentOnlySessions > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d non-Claude sessions outside T3 count toward agent hours only: their transcripts do not record who typed a message.",
			rep.AgentOnlySessions))
	}
	return rep, nil
}

// projectLabel names a project: the repo when there is one, else the
// working directory (home shown as ~), so work in a notes directory or
// an orchestrator's home is not lumped into one anonymous bucket.
func projectLabel(repo, dir string) string {
	if repo != "" {
		return repo
	}
	if dir == "" {
		return "(no project)"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
			return "~/" + filepath.ToSlash(rest)
		}
	}
	return filepath.ToSlash(dir)
}

// filterHours keeps the projects whose name contains filter, and sums
// their rows into the totals and days.
func filterHours(res *hours.Result, filter string) *hours.Result {
	f := strings.ToLower(filter)
	match := func(repo string) bool { return strings.Contains(strings.ToLower(repo), f) }
	out := &hours.Result{}
	for _, r := range res.ByRepo {
		if !match(r.Repo) {
			continue
		}
		out.ByRepo = append(out.ByRepo, r)
		out.AttendedHours += r.AttendedHours
		out.AgentHours += r.AgentHours
		out.AgentUnattendedHours += r.AgentUnattendedHours
		out.Inputs += r.Inputs
	}
	days := map[string]*hours.DayRow{}
	for _, r := range res.ByRepoDay {
		if !match(r.Repo) {
			continue
		}
		out.ByRepoDay = append(out.ByRepoDay, r)
		d, ok := days[r.Day]
		if !ok {
			d = &hours.DayRow{Day: r.Day}
			days[r.Day] = d
		}
		d.AttendedHours += r.AttendedHours
		d.AgentHours += r.AgentHours
		d.AgentUnattendedHours += r.AgentUnattendedHours
		d.Inputs += r.Inputs
	}
	for _, d := range days {
		out.ByDay = append(out.ByDay, *d)
	}
	sort.Slice(out.ByDay, func(i, j int) bool { return out.ByDay[i].Day < out.ByDay[j].Day })
	r2 := func(h float64) float64 { return float64(int64(h*100+0.5)) / 100 }
	out.AttendedHours, out.AgentHours, out.AgentUnattendedHours = r2(out.AttendedHours), r2(out.AgentHours), r2(out.AgentUnattendedHours)
	for i := range out.ByDay {
		d := &out.ByDay[i]
		d.AttendedHours, d.AgentHours, d.AgentUnattendedHours = r2(d.AttendedHours), r2(d.AgentHours), r2(d.AgentUnattendedHours)
	}
	return out
}
