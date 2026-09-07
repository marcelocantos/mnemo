// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Duplicate-entry garbage collection (🎯T170).
//
// Packing a row sets entries.raw to NULL, which makes its generated uuid
// column NULL and drops the row out of idx_entries_session_uuid — a
// partial index, WHERE uuid IS NOT NULL. From then on the pair is
// guarded only by uuid_m, so any insert that does not bind uuid_m
// conflicts with neither index and lands a second copy; the
// entries_materialise trigger that would have set uuid_m afterwards
// collides and is swallowed by the insert's own OR IGNORE.
//
// The insert path is fixed (writerState now picks its statements by
// schema shape, not codec readiness), but rows already written have to be
// removed by product code: this is the phase-3 GC of the deprecation
// policy, not a schema migration, because deciding which copy to keep
// needs application knowledge that sqlift has no hook for.
//
// Idempotent: a second run finds no groups and removes nothing.
//
// Known residue, deliberately not addressed here: session_summary counters
// are maintained by an insert trigger and are not decremented by these
// deletes, so a session that carried duplicates keeps a slightly inflated
// message tally until it is next recomputed.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// dedupeBatchEntries bounds one delete transaction. Small because each
// victim fans out into its messages and their FTS rows.
const dedupeBatchEntries = 200

// DedupeResult reports what a pass found and, unless it was a dry run,
// what it removed.
type DedupeResult struct {
	DryRun          bool
	DuplicateGroups int64 // (session_id, uuid) keys holding more than one row
	EntriesRemoved  int64 // surplus entries rows (groups counted once each)
	MessagesRemoved int64 // messages belonging to those entries
	KeysRepaired    int64 // survivors given the uuid_m their collision had denied them
}

// victimsSQL selects the surplus rows: every member of a duplicate group
// except the one to keep.
//
// The survivor is the lowest id in the group: the original, with the
// re-ingested copy discarded. Read through entries_v, whose uuid column
// is already COALESCE(uuid_m, uuid) — the effective key, and the only one
// that means anything once packing has NULLed a row's generated uuid.
const victimsCore = `
	SELECT id FROM (
		SELECT id, ROW_NUMBER() OVER (
			PARTITION BY session_id, uuid ORDER BY id
		) AS rn
		FROM entries_v
		WHERE uuid IS NOT NULL
	)
	WHERE rn > 1`

// victimsSQL is victimsCore bounded to one batch.
const victimsSQL = victimsCore + ` LIMIT ?`

// DedupeEntries removes surplus copies of entries that share a
// (session_id, uuid) key, together with the messages hanging off them.
//
// With dryRun set it reports what it would remove and changes nothing —
// run it that way first on a large store, because the scan is a window
// function over every keyed row and the counts are worth reading before
// the deletes.
func (s *Store) DedupeEntries(ctx context.Context, dryRun bool) (DedupeResult, error) {
	res := DedupeResult{DryRun: dryRun}

	if err := s.readDB.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(n - 1), 0) FROM (
			SELECT COUNT(*) AS n FROM entries_v
			WHERE uuid IS NOT NULL
			GROUP BY session_id, uuid
			HAVING n > 1)`).Scan(&res.DuplicateGroups, &res.EntriesRemoved); err != nil {
		return res, fmt.Errorf("survey duplicate entries: %w", err)
	}
	if dryRun {
		// EntriesRemoved already holds what *would* go; add the message
		// fan-out so the operator sees the whole cost before committing.
		if err := s.readDB.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM messages WHERE entry_id IN (`+victimsCore+`)`).
			Scan(&res.MessagesRemoved); err != nil {
			return res, fmt.Errorf("survey duplicate messages: %w", err)
		}
		return res, nil
	}
	if res.EntriesRemoved == 0 {
		return res, nil
	}

	var removedEntries, removedMessages int64
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		victims, err := s.dedupeVictims(ctx, dedupeBatchEntries)
		if err != nil {
			return res, err
		}
		if len(victims) == 0 {
			break
		}
		e, m, err := s.dedupeDeleteBatch(ctx, victims)
		if err != nil {
			return res, err
		}
		removedEntries += e
		removedMessages += m
	}

	// A survivor kept by id order can still be the copy that never got a
	// uuid_m — the trigger's update was swallowed when it collided. Now
	// that its twin is gone the key is free, so fill it: otherwise the row
	// stays outside idx_entries_session_uuid_m and is exactly as
	// duplicable as before.
	repaired, err := s.repairKeylessSurvivors(ctx)
	if err != nil {
		return res, err
	}
	res.KeysRepaired = repaired

	res.EntriesRemoved = removedEntries
	res.MessagesRemoved = removedMessages
	slog.Info("dedupe entries pass complete",
		"groups", res.DuplicateGroups, "entries_removed", removedEntries,
		"messages_removed", removedMessages)
	return res, nil
}

func (s *Store) dedupeVictims(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.readDB.QueryContext(ctx, victimsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("select duplicate entries: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// dedupeDeleteBatch removes one batch of surplus entries and their
// messages in a single transaction.
func (s *Store) dedupeDeleteBatch(ctx context.Context, victims []int64) (entries, messages int64, err error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// messages_fts is an external-content index (content=messages), so a
	// plain DELETE would leave it pointing at rows that no longer exist.
	// The 'delete' command must be given the row's OLD values, and only
	// non-noise rows were ever indexed — the insert trigger filters on
	// is_noise = 0, so deleting an unindexed row would corrupt the index.
	const ftsDelete = `
		INSERT INTO messages_fts(messages_fts, rowid, text, role, project, session_id)
		SELECT 'delete', m.id, m.text, m.role, m.project, m.session_id
		FROM messages_v m WHERE m.entry_id = ? AND m.is_noise = 0`

	for _, id := range victims {
		if _, err = tx.ExecContext(ctx, ftsDelete, id); err != nil {
			return 0, 0, fmt.Errorf("fts delete for entry %d: %w", id, err)
		}
		var r sql.Result
		if r, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE entry_id = ?`, id); err != nil {
			return 0, 0, fmt.Errorf("delete messages for entry %d: %w", id, err)
		}
		n, _ := r.RowsAffected()
		messages += n

		if r, err = tx.ExecContext(ctx, `DELETE FROM entries WHERE id = ?`, id); err != nil {
			return 0, 0, fmt.Errorf("delete entry %d: %w", id, err)
		}
		n, _ = r.RowsAffected()
		entries += n
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return entries, messages, nil
}

// repairKeylessSurvivors fills uuid_m on rows that still hold their raw
// but never got a twin, because the entries_materialise trigger's update
// collided with the copy just deleted and was swallowed by OR IGNORE.
//
// Restricted to rows whose raw is still present: a packed row cannot
// recover a uuid it no longer stores, and there is nothing to recover
// anyway, since packing only ever happened after materialisation.
func (s *Store) repairKeylessSurvivors(ctx context.Context) (int64, error) {
	r, err := s.writeDB.ExecContext(ctx, `
		UPDATE entries SET uuid_m = uuid
		WHERE uuid_m IS NULL AND uuid IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("repair keyless survivors: %w", err)
	}
	n, _ := r.RowsAffected()
	return n, nil
}
