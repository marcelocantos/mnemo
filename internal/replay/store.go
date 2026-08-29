// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"database/sql"
	"fmt"
	"github.com/marcelocantos/mnemo/internal/store"
	"strings"
	"time"
)

// Scope selects which tool_use rows to replay.
type Scope struct {
	SessionID string
	Since     *time.Time
	Until     *time.Time
	Repo      string
	Source    string
}

// DB reads replay candidates from a read-only SQLite handle.
type DB struct {
	db           *sql.DB
	textZ        bool
	textZChecked bool
}

func NewDB(db *sql.DB) *DB {
	return &DB{db: db}
}

// CollectOps queries tool_use rows matching scope and maps them to normalised ops.
func (d *DB) CollectOps(scope Scope) ([]Op, []string, error) {
	if scope.SessionID != "" {
		resolved, err := d.resolveSessionID(scope.SessionID)
		if err != nil {
			return nil, nil, err
		}
		scope.SessionID = resolved
	}
	rows, err := d.queryToolRows(scope)
	if err != nil {
		return nil, nil, err
	}
	var ops []Op
	var warns []string
	missingResultSessions := make(map[string]struct{})

	for _, row := range rows {
		if row.ResultError == nil {
			if _, ok := missingResultSessions[row.SessionID]; !ok {
				missingResultSessions[row.SessionID] = struct{}{}
				warns = append(warns, ReasonToolResultMissing+": session="+row.SessionID)
			}
		}
		expanded, skipReason := OpFromToolRow(row)
		if skipReason != "" {
			continue
		}
		ops = append(ops, expanded...)
	}
	SortOps(ops)
	return ops, warns, nil
}

func (d *DB) queryToolRows(scope Scope) ([]ToolRow, error) {
	names := ReplayToolNames()
	placeholders := strings.Repeat("?,", len(names))
	placeholders = placeholders[:len(placeholders)-1]

	var args []any
	var where []string

	where = append(where, "m.content_type = 'tool_use'")
	where = append(where, "m.is_noise = 0")
	where = append(where, fmt.Sprintf("m.tool_name IN (%s)", placeholders))
	for _, n := range names {
		args = append(args, n)
	}

	if scope.SessionID != "" {
		where = append(where, "m.session_id = ?")
		args = append(args, scope.SessionID)
	}
	if scope.Since != nil {
		where = append(where, "m.timestamp >= ?")
		args = append(args, scope.Since.UTC().Format(time.RFC3339Nano))
	}
	if scope.Until != nil {
		where = append(where, "m.timestamp <= ?")
		args = append(args, scope.Until.UTC().Format(time.RFC3339Nano))
	}
	if scope.Repo != "" {
		where = append(where, "sm.repo = ?")
		args = append(args, scope.Repo)
	}
	if scope.Source != "" {
		where = append(where, "sm.source = ?")
		args = append(args, scope.Source)
	}

	textExpr := "COALESCE(m.text, '')"
	if d.hasTextZ() {
		textExpr = "COALESCE(mnemo_text(m.text, m.text_z), '')"
	}

	q := fmt.Sprintf(`
SELECT m.timestamp, m.session_id, COALESCE(sm.source,''), m.tool_name, m.tool_use_id,
       m.tool_input, %s, COALESCE(m.tool_file_path, ''),
       COALESCE(m.tool_content, ''), COALESCE(m.tool_old_string, ''), COALESCE(m.tool_new_string, ''),
       COALESCE(sm.cwd, ''), COALESCE(sm.repo, ''),
       tr.is_error
FROM messages m
JOIN session_meta sm ON sm.session_id = m.session_id
LEFT JOIN messages tr ON tr.session_id = m.session_id
  AND tr.content_type = 'tool_result' AND tr.tool_use_id = m.tool_use_id
WHERE %s
ORDER BY m.timestamp, m.session_id, sm.source, m.tool_use_id`, textExpr, strings.Join(where, " AND "))

	rs, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var out []ToolRow
	for rs.Next() {
		var row ToolRow
		var ts string
		var toolInput []byte
		var isErr sql.NullInt64
		if err := rs.Scan(&ts, &row.SessionID, &row.Source, &row.ToolName, &row.ToolUseID,
			&toolInput, &row.Text, &row.FilePath,
			&row.ToolContent, &row.OldString, &row.NewString,
			&row.CWD, &row.Repo, &isErr); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			row.Timestamp = t
		} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
			row.Timestamp = t
		}
		if len(toolInput) > 0 {
			row.ToolInput = toolInput
		}
		if isErr.Valid {
			v := isErr.Int64 != 0
			row.ResultError = &v
		}
		out = append(out, row)
	}
	return out, rs.Err()
}

func (d *DB) hasTextZ() bool {
	if d.textZChecked {
		return d.textZ
	}
	d.textZChecked = true
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'text_z'`).Scan(&n)
	d.textZ = err == nil && n > 0
	return d.textZ
}

// OpenReadOnly opens mnemo.db read-only for replay CLI use.
func OpenReadOnly(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", dbPath)
	return sql.Open(store.SQLiteDriverName, dsn)
}

func (d *DB) resolveSessionID(id string) (string, error) {
	var exists int
	err := d.db.QueryRow("SELECT 1 FROM session_summary WHERE session_id = ?", id).Scan(&exists)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	rows, err := d.db.Query("SELECT session_id FROM session_summary WHERE session_id LIKE ? LIMIT 2", id+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var sid string
		_ = rows.Scan(&sid)
		matches = append(matches, sid)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("session not found: %s", id)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous session prefix %q (%d matches)", id, len(matches))
	}
}
