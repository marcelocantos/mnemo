// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Compaction is a distilled representation of a session's work, produced
// by a summariser LLM. Payload is structured extraction (targets,
// decisions, files, open_threads, summary) serialised as JSON; Summary
// is the prose abstract held separately for quick reads and FTS later.
type Compaction struct {
	ID           int64
	SessionID    string
	GeneratedAt  time.Time
	Model        string
	PromptTokens int
	OutputTokens int
	CostUSD      float64
	EntryIDFrom  int64
	EntryIDTo    int64
	PayloadJSON  string
	Summary      string
}

// PutCompaction inserts a compaction row and returns the assigned ID.
func (s *Store) PutCompaction(c Compaction) (int64, error) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	generatedAt := c.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	payload := c.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	res, err := s.db.Exec(`
		INSERT INTO compactions
			(session_id, generated_at, model, prompt_tokens, output_tokens,
			 cost_usd, entry_id_from, entry_id_to, payload_json, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		c.SessionID, generatedAt.UTC().Format(time.RFC3339Nano), c.Model,
		c.PromptTokens, c.OutputTokens, c.CostUSD,
		c.EntryIDFrom, c.EntryIDTo, payload, c.Summary,
	)
	if err != nil {
		return 0, fmt.Errorf("insert compaction: %w", err)
	}
	return res.LastInsertId()
}

// LatestCompaction returns the most recent compaction for a session, or
// (nil, nil) if none exist.
func (s *Store) LatestCompaction(sessionID string) (*Compaction, error) {
	row := s.db.QueryRow(`
		SELECT id, session_id, generated_at, model, prompt_tokens, output_tokens,
		       cost_usd, entry_id_from, entry_id_to, payload_json, summary
		FROM compactions
		WHERE session_id = ?
		ORDER BY generated_at DESC, id DESC
		LIMIT 1
	`, sessionID)
	return scanCompaction(row)
}

// ListCompactions returns compactions for a session in chronological order
// (oldest first), up to limit. A limit of 0 or negative returns all rows.
func (s *Store) ListCompactions(sessionID string, limit int) ([]Compaction, error) {
	q := `
		SELECT id, session_id, generated_at, model, prompt_tokens, output_tokens,
		       cost_usd, entry_id_from, entry_id_to, payload_json, summary
		FROM compactions
		WHERE session_id = ?
		ORDER BY generated_at ASC, id ASC`
	args := []any{sessionID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list compactions: %w", err)
	}
	defer rows.Close()
	return scanCompactions(rows)
}

// ChainCompactions walks the session chain from the oldest /clear-bounded
// predecessor to the given session and returns the latest compaction per
// session along that chain, in chronological order (oldest span first).
// Sessions with no compaction are skipped silently.
func (s *Store) ChainCompactions(sessionID string) ([]Compaction, error) {
	chain, err := s.Chain(sessionID)
	if err != nil {
		return nil, fmt.Errorf("chain lookup: %w", err)
	}
	if len(chain) == 0 {
		// No chain recorded — treat the session as standalone.
		if latest, err := s.LatestCompaction(sessionID); err != nil {
			return nil, err
		} else if latest != nil {
			return []Compaction{*latest}, nil
		}
		return nil, nil
	}
	out := make([]Compaction, 0, len(chain))
	for _, link := range chain {
		latest, err := s.LatestCompaction(link.SessionID)
		if err != nil {
			return nil, err
		}
		if latest != nil {
			out = append(out, *latest)
		}
	}
	return out, nil
}

// scannable is anything with a Scan method matching *sql.Row / *sql.Rows.
type scannable interface {
	Scan(dest ...any) error
}

func scanCompaction(r scannable) (*Compaction, error) {
	var c Compaction
	var generatedAt string
	err := r.Scan(&c.ID, &c.SessionID, &generatedAt, &c.Model,
		&c.PromptTokens, &c.OutputTokens, &c.CostUSD,
		&c.EntryIDFrom, &c.EntryIDTo, &c.PayloadJSON, &c.Summary)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan compaction: %w", err)
	}
	if t, err := time.Parse(time.RFC3339Nano, generatedAt); err == nil {
		c.GeneratedAt = t
	} else if t, err := time.Parse("2006-01-02 15:04:05", generatedAt); err == nil {
		c.GeneratedAt = t
	}
	return &c, nil
}

func scanCompactions(rows *sql.Rows) ([]Compaction, error) {
	var out []Compaction
	for rows.Next() {
		c, err := scanCompaction(rows)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, *c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate compactions: %w", err)
	}
	return out, nil
}
