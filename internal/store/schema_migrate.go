// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/marcelocantos/sqlift/go/sqlift"
)

// Schema migrations are one transaction.
//
// sqlift's apply() (sqlift.cpp) runs each statement on the connection's
// autocommit and, on failure, only rolls back a rebuild savepoint. It
// does stop at the first error — it does not execute the statements
// after it — but everything before that error is already committed, and
// the schema hash is written only after the whole plan succeeds. Indexes
// are applied in name order, so the index declared first in schema.sql
// (idx_messages_text_z_len) is the last z_len index to run. When that
// CREATE INDEX lost the write lock, the eight indexes before it stayed,
// the hash stayed at the pre-migration value, and every later start
// refused to migrate: "Schema drift detected".
//
// applyMigration wraps the apply in BEGIN IMMEDIATE / COMMIT. An abort
// rolls the schema back to the hash sqlift already stored, so the next
// start retries a clean plan. A store that is already drifted is
// reconciled when the remaining difference is purely additive: the
// missing objects are created, the result is checked against the desired
// schema, and the hash is re-stamped. Anything else stays drifted and
// is reported as such — a restart will not retry it.

// schemaApplyLockRetries is how many times a lock failure is retried.
// Each attempt waits schemaApplyBusyTimeout for the write lock, so a
// migration does not give up after the minute or so a single CREATE
// INDEX spent building while our own ingest held the lock.
const schemaApplyLockRetries = 3

// schemaApplyBusyTimeout is the SQLite busy_timeout on the migration
// connection. It bounds how long BEGIN IMMEDIATE waits for another
// writer, not how long a statement may run.
var schemaApplyBusyTimeout = 5 * time.Minute

// driftHashRe pulls the two hashes out of sqlift's DriftError text.
// The actual hash is sqlift's own digest of the live schema — the value
// the drift check compares against — so re-stamping it is what makes
// the following Apply accept the database.
var driftHashRe = regexp.MustCompile(`Stored hash: ([0-9a-fA-F]{64}), actual hash: ([0-9a-fA-F]{64})`)

// schemaDriftError is a drift sqlift will not repair by retrying. The
// health check keys off the "schema drift:" prefix: that is the case
// where telling the operator to restart and retry is wrong.
type schemaDriftError struct {
	msg string
}

func (e *schemaDriftError) Error() string { return e.msg }

func schemaDriftf(cause error, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	if cause != nil {
		detail += ": " + cause.Error()
	}
	return &schemaDriftError{msg: "schema drift: " + detail}
}

func setSchemaBusyTimeout(sdb *sqlift.Database) error {
	ms := schemaApplyBusyTimeout.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if err := sdb.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", ms)); err != nil {
		return fmt.Errorf("schema upgrade busy_timeout: %w", err)
	}
	return nil
}

// applyMigration applies plan and leaves the database either at desired
// or, on error, at the schema whose hash sqlift had stored. A drifted
// store whose missing objects are a pure addition is repaired in place.
func applyMigration(sdb *sqlift.Database, plan sqlift.MigrationPlan, desired sqlift.Schema) error {
	// sqlift checks the destructive gate before the drift check, so a
	// drifted database whose plan would drop an object comes back as
	// DestructiveError and never mentions the hash. Classify that here,
	// before Apply, so we refuse it as drift instead of attempting the drop.
	if ok, why := planIsAdditiveRepair(plan); !ok {
		drifted, err := schemaHashDrifted(sdb)
		if err != nil {
			return err
		}
		if drifted {
			return schemaDriftf(nil, "not an interrupted additive migration (blocking op %s), so a restart will not retry", why)
		}
	}
	err := applyPlanWithLockRetry(sdb, plan, desired)
	var drift *sqlift.DriftError
	if !errors.As(err, &drift) {
		return err
	}
	slog.Warn("schema hash drifted from the database; reconciling additive objects")
	if rerr := reconcileDriftedSchema(sdb, desired, err); rerr != nil {
		if isSQLiteLock(rerr) {
			return fmt.Errorf("schema upgrade lost the write lock during drift reconcile (rolled back; restart will retry): %w", rerr)
		}
		var already *schemaDriftError
		if errors.As(rerr, &already) {
			return rerr
		}
		return schemaDriftf(rerr, "reconcile failed, so a restart will not retry the migration")
	}
	return nil
}

func applyPlanWithLockRetry(sdb *sqlift.Database, plan sqlift.MigrationPlan, desired sqlift.Schema) error {
	var last error
	for attempt := 1; attempt <= schemaApplyLockRetries; attempt++ {
		err := applyPlanOnce(sdb, plan, desired)
		if err == nil {
			return nil
		}
		last = err
		var drift *sqlift.DriftError
		if errors.As(err, &drift) || !isSQLiteLock(err) {
			return err
		}
		if attempt == schemaApplyLockRetries {
			break
		}
		slog.Warn("schema upgrade waiting on SQLite lock",
			"attempt", attempt, "of", schemaApplyLockRetries, "err", err)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return fmt.Errorf("schema upgrade lost the write lock after %d attempts (rolled back; restart will retry): %w",
		schemaApplyLockRetries, last)
}

// applyPlanOnce runs one attempt. On any error the transaction is rolled
// back before the error is returned, so a failed statement does not leave
// the earlier ones committed.
func applyPlanOnce(sdb *sqlift.Database, plan sqlift.MigrationPlan, desired sqlift.Schema) error {
	if err := sdb.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := sqlift.Apply(sdb, plan, sqlift.ApplyOptions{Allow: sqlift.AllowNone}); err != nil {
		rollbackSchema(sdb)
		return err
	}
	if err := schemaMatches(sdb, desired); err != nil {
		rollbackSchema(sdb)
		return err
	}
	if err := sdb.Exec("COMMIT"); err != nil {
		rollbackSchema(sdb)
		return err
	}
	return nil
}

// reconcileDriftedSchema creates the objects desired has and the live
// database lacks, checks the result matches desired, and re-stamps the
// hash. Plans that would drop or rebuild are refused: those are not an
// interrupted additive migration, and applying them would destroy
// whatever the drift actually is.
func reconcileDriftedSchema(sdb *sqlift.Database, desired sqlift.Schema, drift error) error {
	_, actual, ok := hashesFromDrift(drift)
	if !ok {
		return schemaDriftf(drift, "could not read the live schema hash, so a restart will not retry the migration")
	}
	current, err := sqlift.Extract(sdb)
	if err != nil {
		return err
	}
	plan, err := sqlift.Diff(current, desired)
	if err != nil {
		return schemaDriftf(err, "a restart will not retry the migration")
	}
	if repairable, why := planIsAdditiveRepair(plan); !repairable {
		return schemaDriftf(drift, "not an interrupted additive migration (blocking op %s), so a restart will not retry", why)
	}

	if err := sdb.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	// The hash sqlift stored describes an older schema. Stamp the live
	// schema's own hash so Apply's drift check passes, then let Apply
	// create the missing objects and stamp the finished schema. Both
	// writes are in this transaction: a failure rolls them back together
	// and the store stays exactly as drifted as it was.
	if err := restampSchemaHash(sdb, actual); err != nil {
		rollbackSchema(sdb)
		return err
	}
	if !plan.Empty() {
		if err := sqlift.Apply(sdb, plan, sqlift.ApplyOptions{Allow: sqlift.AllowNone}); err != nil {
			rollbackSchema(sdb)
			return err
		}
	}
	if err := schemaMatches(sdb, desired); err != nil {
		rollbackSchema(sdb)
		return schemaDriftf(err, "reconcile did not reach the expected schema, so a restart will not retry")
	}
	if err := sdb.Exec("COMMIT"); err != nil {
		rollbackSchema(sdb)
		return err
	}
	var created []string
	for _, op := range plan.Operations() {
		created = append(created, op.ObjectName)
	}
	slog.Info("schema drift reconciled and hash re-stamped",
		"objects", strings.Join(created, ", "))
	return nil
}

// planIsAdditiveRepair reports whether plan only creates objects the
// desired schema has and the live database lacks. Empty is repairable:
// the schema already matches and only the hash is stale.
func planIsAdditiveRepair(plan sqlift.MigrationPlan) (bool, string) {
	for _, op := range plan.Operations() {
		if op.Destructive || op.RequiresRebuild || op.DataDependent {
			return false, op.Type.String() + " " + op.ObjectName
		}
		switch op.Type {
		case sqlift.CreateTable, sqlift.AddColumn, sqlift.CreateIndex,
			sqlift.CreateView, sqlift.CreateTrigger, sqlift.CreateVirtualTable:
		default:
			return false, op.Type.String() + " " + op.ObjectName
		}
	}
	return true, ""
}

func schemaMatches(sdb *sqlift.Database, desired sqlift.Schema) error {
	after, err := sqlift.Extract(sdb)
	if err != nil {
		return fmt.Errorf("sqlift extract after apply: %w", err)
	}
	plan, err := sqlift.Diff(after, desired)
	if err != nil {
		return fmt.Errorf("sqlift diff after apply: %w", err)
	}
	if plan.Empty() {
		return nil
	}
	var names []string
	for _, op := range plan.Operations() {
		names = append(names, op.Type.String()+" "+op.ObjectName)
	}
	return fmt.Errorf("schema does not match the expected schema: %s", strings.Join(names, ", "))
}

func restampSchemaHash(sdb *sqlift.Database, hash string) error {
	if !validSchemaHash(hash) {
		return fmt.Errorf("refuse to stamp schema hash %q", hash)
	}
	if err := sdb.Exec(`CREATE TABLE IF NOT EXISTS _sqlift_state (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		return err
	}
	return sdb.Exec(fmt.Sprintf(
		"INSERT OR REPLACE INTO _sqlift_state (key, value) VALUES ('schema_hash', '%s')",
		strings.ToLower(hash)))
}

func validSchemaHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// schemaHashDrifted reports whether _sqlift_state holds a hash that is
// not the live schema's. An absent hash is not drift: a database that
// has never been through sqlift has nothing to compare.
func schemaHashDrifted(sdb *sqlift.Database) (bool, error) {
	stored, err := sdb.QueryText("SELECT value FROM _sqlift_state WHERE key='schema_hash'")
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return false, nil
		}
		return false, err
	}
	if stored == "" {
		return false, nil
	}
	current, err := sqlift.Extract(sdb)
	if err != nil {
		return false, err
	}
	return !strings.EqualFold(stored, current.Hash()), nil
}

func hashesFromDrift(err error) (stored, actual string, ok bool) {
	var drift *sqlift.DriftError
	if !errors.As(err, &drift) || drift == nil {
		return "", "", false
	}
	m := driftHashRe.FindStringSubmatch(drift.Error())
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func rollbackSchema(sdb *sqlift.Database) {
	if err := sdb.Exec("ROLLBACK"); err != nil {
		slog.Warn("schema upgrade rollback failed", "err", err)
	}
}

func isSQLiteLock(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database table is locked") ||
		strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "SQLITE_LOCKED")
}
