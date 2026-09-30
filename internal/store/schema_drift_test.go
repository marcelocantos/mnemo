// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/sqlift/go/sqlift"
)

// TestMigrationRollsBackWhenALaterStatementFails is the transactional-DDL
// guard. sqlift applies indexes in name order and used to autocommit each
// statement, so a failure on a later index left the earlier ones committed
// and the schema hash unstamped. The whole plan is one transaction: the
// earlier index is gone, the hash is unchanged, and a retry applies cleanly.
func TestMigrationRollsBackWhenALaterStatementFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	v1 := `
		CREATE TABLE t (
			id INTEGER PRIMARY KEY,
			body TEXT
		);
	`
	if err := upgradeSchemaWith(path, v1); err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	hash1 := mustSchemaHash(t, path)
	if goHash := schemaHashGo(t, path); goHash != hash1 {
		t.Fatalf("Go schema hash does not match the hash sqlift stored\ngo     %s\nstored %s", goHash, hash1)
	}

	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t (id, body) VALUES (1, 'kept')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// idx_aaa sorts before idx_zzz, so the valid index is applied first
	// and the missing-column index is the one that fails.
	v2 := v1 + `
		CREATE INDEX idx_aaa_ok ON t(id);
		CREATE INDEX idx_zzz_bad ON t(no_such_column);
	`
	err = upgradeSchemaWith(path, v2)
	if err == nil {
		t.Fatal("migration with a bad index must fail")
	}
	if !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("failure must be the later CREATE INDEX, not a refusal to plan it: %v", err)
	}
	if strings.Contains(err.Error(), "schema drift:") {
		t.Fatalf("a rolled-back migration must not report drift: %v", err)
	}
	if got := mustSchemaHash(t, path); got != hash1 {
		t.Fatalf("schema hash changed on a failed migration\nbefore %s\nafter  %s", hash1, got)
	}
	if masterHas(t, path, "index", "idx_aaa_ok") {
		t.Fatal("idx_aaa_ok is present: the migration committed statements before the failure")
	}
	if masterHas(t, path, "index", "idx_zzz_bad") {
		t.Fatal("idx_zzz_bad exists after its CREATE INDEX failed")
	}
	if got := scalar(t, path, `SELECT body FROM t WHERE id = 1`); got != "kept" {
		t.Fatalf("row after rollback = %q, want kept", got)
	}

	v3 := v1 + `CREATE INDEX idx_aaa_ok ON t(id);`
	if err := upgradeSchemaWith(path, v3); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if !masterHas(t, path, "index", "idx_aaa_ok") {
		t.Fatal("retry did not create idx_aaa_ok")
	}
	if got := mustSchemaHash(t, path); got == hash1 {
		t.Fatal("successful retry did not re-stamp the schema hash")
	}
}

// TestReconcileRestoresMissingIndexAndRestampHash is the incident shape:
// an earlier migration committed some of its objects, left the hash at
// the previous schema, and a later start refused to create the one index
// still missing. Reconcile creates it, checks the schema matches, and
// re-stamps a hash the next migration will accept.
func TestReconcileRestoresMissingIndexAndRestampHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	v1 := `CREATE TABLE messages (id INTEGER PRIMARY KEY, body TEXT);`
	if err := upgradeSchemaWith(path, v1); err != nil {
		t.Fatalf("v1: %v", err)
	}

	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE messages ADD COLUMN z_len INTEGER`,
		`CREATE INDEX idx_docs_content_z_len ON messages(z_len) WHERE body IS NOT NULL`,
		`INSERT INTO messages (id, body, z_len) VALUES (1, 'kept', 4)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if masterHas(t, path, "index", "idx_messages_text_z_len") {
		t.Fatal("fixture already has the index the reconcile is supposed to create")
	}

	v2 := `
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			body TEXT,
			z_len INTEGER
		);
		CREATE INDEX idx_docs_content_z_len ON messages(z_len) WHERE body IS NOT NULL;
		CREATE INDEX idx_messages_text_z_len ON messages(z_len) WHERE z_len IS NOT NULL;
	`
	if err := upgradeSchemaWith(path, v2); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !masterHas(t, path, "index", "idx_messages_text_z_len") {
		t.Fatal("reconcile did not create idx_messages_text_z_len")
	}
	if !masterHas(t, path, "index", "idx_docs_content_z_len") {
		t.Fatal("reconcile dropped an index that was already there")
	}
	if got := scalar(t, path, `SELECT body FROM messages WHERE id = 1`); got != "kept" {
		t.Fatalf("row after reconcile = %q, want kept", got)
	}

	// A further additive migration drift-checks the re-stamped hash.
	// An empty diff would succeed even with a stale hash, so this has
	// to be a real statement.
	v3 := v2 + `CREATE INDEX idx_probe_schema_drift ON messages(id);`
	if err := upgradeSchemaWith(path, v3); err != nil {
		t.Fatalf("follow-up migration saw a stale hash: %v", err)
	}
	if !masterHas(t, path, "index", "idx_probe_schema_drift") {
		t.Fatal("follow-up index was not created")
	}
}

// TestReconcileRefusesNonAdditiveDrift: an object the desired schema does
// not have is not an interrupted migration. Reconcile must not drop it,
// and the hash must stay put so the refusal is visible on the next start.
func TestReconcileRefusesNonAdditiveDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	v1 := `CREATE TABLE t (id INTEGER PRIMARY KEY);`
	if err := upgradeSchemaWith(path, v1); err != nil {
		t.Fatal(err)
	}
	hash1 := mustSchemaHash(t, path)
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE extra (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	err = upgradeSchemaWith(path, v1)
	if err == nil {
		t.Fatal("drift that requires a drop must not be applied")
	}
	if !strings.Contains(err.Error(), "schema drift:") {
		t.Fatalf("error = %v, want a schema drift refusal", err)
	}
	if !strings.Contains(err.Error(), "will not retry") {
		t.Fatalf("error = %v, want it to say a restart will not retry", err)
	}
	if !masterHas(t, path, "table", "extra") {
		t.Fatal("reconcile dropped the unexpected table")
	}
	if got := mustSchemaHash(t, path); got != hash1 {
		t.Fatalf("refused reconcile re-stamped the hash\nbefore %s\nafter  %s", hash1, got)
	}
}

// TestNewRepairsADroppedIndex is the operator path. A store sqlift has
// stamped, with one index removed by hand, comes back through store.New:
// the missing index is created, the schema matches, and a later migration
// accepts the re-stamped hash. Restarting onto this build is the recovery.
func TestNewRepairsADroppedIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mnemo.db")
	// Apply through sqlift, not the fresh-schema fast path, so the hash
	// is the one a migrated store carries. A fresh store has no hash and
	// would recreate the index without ever noticing drift.
	if err := upgradeSchemaWith(path, schemaSQL); err != nil {
		t.Fatalf("seed full schema: %v", err)
	}
	stamped := mustSchemaHash(t, path)
	if goHash := schemaHashGo(t, path); goHash != stamped {
		t.Fatalf("Go schema hash does not match sqlift on the full schema\ngo     %s\nstored %s", goHash, stamped)
	}

	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_messages_text_z_len`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	if !s.Have(CapSchemaCurrent) {
		t.Fatalf("schema.current unavailable after repair: %+v", s.StartupReport())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !masterHas(t, path, "index", "idx_messages_text_z_len") {
		t.Fatal("store.New did not restore idx_messages_text_z_len")
	}
	// Recreating the dropped index returns the schema to the shape sqlift
	// already stamped, so the hash matches the pre-drop value. The
	// follow-up migration is what proves that stamp is the one Apply checks.
	if got := mustSchemaHash(t, path); got != stamped {
		t.Fatalf("repair stamped a hash other than the restored schema\nbefore %s\nafter  %s", stamped, got)
	}

	eol := "\n"
	if strings.Contains(schemaSQL, "\r\n") {
		eol = "\r\n"
	}
	probe := schemaSQL + eol + "CREATE INDEX idx_probe_schema_drift ON messages(id);" + eol
	if err := upgradeSchemaWith(path, probe); err != nil {
		t.Fatalf("follow-up migration saw a stale hash: %v", err)
	}
	if !masterHas(t, path, "index", "idx_probe_schema_drift") {
		t.Fatal("follow-up index was not created")
	}
}

// TestStartupIngestWaitsForTheSchemaUpgrade: the deferred upgrade used to
// race the startup ingest, and CREATE INDEX lost that race. IngestAll
// does not run while the upgrade is still in the pre-migration backup.
func TestStartupIngestWaitsForTheSchemaUpgrade(t *testing.T) {
	dbPath, started, release := stageDeferredUpgrade(t)
	s, err := New(dbPath, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("pre-migration backup never started")
	}

	got := make(chan error, 1)
	go func() { got <- s.IngestAll() }()
	select {
	case err := <-got:
		close(release)
		t.Fatalf("IngestAll returned while the schema upgrade was still running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("IngestAll after upgrade: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("IngestAll did not resume after the schema upgrade finished")
	}
}

func schemaHashGo(t *testing.T, path string) string {
	t.Helper()
	sdb, err := sqlift.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	current, err := sqlift.Extract(sdb)
	if err != nil {
		t.Fatal(err)
	}
	return current.Hash()
}

func mustSchemaHash(t *testing.T, path string) string {
	t.Helper()
	h := scalar(t, path, `SELECT value FROM _sqlift_state WHERE key = 'schema_hash'`)
	if len(h) != 64 {
		t.Fatalf("schema hash %q is not a sha256 hex digest", h)
	}
	return h
}

func masterHas(t *testing.T, path, kind, name string) bool {
	t.Helper()
	return scalar(t, path,
		`SELECT count(*) FROM sqlite_master WHERE type = '`+kind+`' AND name = '`+name+`'`) == "1"
}

func scalar(t *testing.T, path, query string) string {
	t.Helper()
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}
