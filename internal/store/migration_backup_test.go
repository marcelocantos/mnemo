// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/sqlift/go/sqlift"
)

// planFor diffs a live database against a desired schema and returns the
// migration plan, so these tests classify plans sqlift actually produces
// rather than ones hand-built to match the classifier.
func planFor(t *testing.T, dbPath, desiredSQL string) sqlift.MigrationPlan {
	t.Helper()
	sdb, err := sqlift.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	current, err := sqlift.Extract(sdb)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := sqlift.Parse(desiredSQL)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := sqlift.Diff(current, desired)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// TestMetadataOnlyPlanSkipsTheBackup is the 🎯T155 oracle: a view
// redefinition must not pay for a full pre-migration backup, and
// anything touching a table's own definition or its data still must.
func TestMetadataOnlyPlanSkipsTheBackup(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.AwaitStartup()
	dbPath := s.dbPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// schema.sql is embedded from the checkout, so under Windows autocrlf
	// it arrives with CRLF. Do NOT normalise it: the live schema was
	// created from this exact text, so an LF-normalised "desired" differs
	// textually from what sqlite_master holds and sqlift plans a
	// drop+create of an FTS virtual table — a destructive diff that has
	// nothing to do with the fixture. Detect the line ending instead and
	// build variants in the file's own terms.
	base := schemaSQL
	eol := "\n"
	if strings.Contains(base, "\r\n") {
		eol = "\r\n"
	}

	// The shipped schema against itself: nothing to do. Assert the plan is
	// genuinely empty — otherwise the classifier could be returning false
	// for the wrong reason and every case below would be meaningless.
	selfPlan := planFor(t, dbPath, base)
	if !selfPlan.Empty() {
		var ops []string
		for _, op := range selfPlan.Operations() {
			ops = append(ops, op.Type.String()+" "+op.ObjectName)
		}
		t.Fatalf("the shipped schema diffs against a store it just created: %v", ops)
	}
	if ok, _ := planIsMetadataOnly(selfPlan); ok {
		t.Error("an empty plan must not be classified metadata-only (there is nothing to skip)")
	}

	// A view redefinition — exactly the change that cost ~18 minutes of
	// VACUUM INTO on the live index.
	viewChange := strings.Replace(base,
		"CREATE VIEW docs_v AS",
		"CREATE VIEW docs_v_extra AS SELECT id FROM docs;"+eol+"CREATE VIEW docs_v AS", 1)
	if viewChange == base {
		t.Fatal("fixture is stale: docs_v is no longer declared this way")
	}
	ok, why := planIsMetadataOnly(planFor(t, dbPath, viewChange))
	if !ok {
		t.Errorf("a view-only plan must skip the backup, got required because: %s", why)
	}

	// An index addition is metadata too.
	indexChange := base + eol + "CREATE INDEX idx_docs_kind_t155 ON docs(kind);" + eol
	if ok, why := planIsMetadataOnly(planFor(t, dbPath, indexChange)); !ok {
		t.Errorf("an index-only plan must skip the backup, got required because: %s", why)
	}

	// A new column changes the table's own definition: keep the backup.
	// Appended last, which is the only shape the append-only schema policy
	// permits — a column inserted mid-table makes sqlift plan a rebuild
	// instead, which this classifier also (correctly) refuses.
	colChange := strings.Replace(base,
		"			content_z BLOB"+eol+"		);",
		"			content_z BLOB,"+eol+"			t155_probe TEXT"+eol+"		);", 1)
	if colChange == base {
		t.Fatal("fixture is stale: docs.content_z is no longer the last column")
	}
	ok, why = planIsMetadataOnly(planFor(t, dbPath, colChange))
	if ok {
		t.Error("a plan adding a column must keep the backup")
	}
	if !strings.Contains(why, "docs") {
		t.Errorf("the reason should name the offending object, got %q", why)
	}

	// A mid-table column forces a rebuild; that must keep the backup too,
	// and say so.
	rebuild := strings.Replace(base,
		"			doc_source TEXT NOT NULL DEFAULT ''",
		"			doc_source TEXT NOT NULL DEFAULT '',"+eol+"			t155_mid TEXT", 1)
	ok, why = planIsMetadataOnly(planFor(t, dbPath, rebuild))
	if ok {
		t.Error("a plan rebuilding a table must keep the backup")
	}
	if !strings.Contains(why, "rebuild") {
		t.Errorf("a rebuild should be named as such, got %q", why)
	}

	// A new table likewise.
	tableChange := base + eol + "CREATE TABLE t155_new (id INTEGER PRIMARY KEY);" + eol
	if ok, _ := planIsMetadataOnly(planFor(t, dbPath, tableChange)); ok {
		t.Error("a plan creating a table must keep the backup")
	}
}

// TestUpgradeSkipsBackupForMetadataOnlyPlan is the end-to-end half of
// 🎯T155: not merely that the classifier says "metadata only", but that
// upgradeSchema actually declines to call preMigrationBackup.
//
// The target's third criterion asked for this on a live-sized index. The
// property is size-independent — what size changes is only how much time
// the skipped VACUUM INTO would have cost (~18 minutes on the 18.9 GB
// index that prompted the target) — so it is measured directly here
// rather than by copying tens of gigabytes.
func TestUpgradeSkipsBackupForMetadataOnlyPlan(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	s, err := New(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	var backups int
	orig := preMigrationBackup
	preMigrationBackup = func(src, dest string, args *backup.BackupArgs) (backup.Result, error) {
		backups++
		return orig(src, dest, args)
	}
	t.Cleanup(func() { preMigrationBackup = orig })

	eol := "\n"
	if strings.Contains(schemaSQL, "\r\n") {
		eol = "\r\n"
	}

	// A view-only change: the migration must apply with no snapshot.
	withView := schemaSQL + eol + "CREATE VIEW t155_view AS SELECT id FROM docs;" + eol
	if err := upgradeSchemaWith(dbPath, withView); err != nil {
		t.Fatalf("view-only upgrade: %v", err)
	}
	if backups != 0 {
		t.Errorf("a view-only migration took %d pre-migration backup(s); it can touch no table data", backups)
	}
	// And it really applied.
	db, err := sql.Open(SQLiteDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='view' AND name='t155_view'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if n != 1 {
		t.Fatal("the view-only migration did not apply")
	}

	// A column change must still be insured.
	withCol := strings.Replace(withView,
		"			content_z BLOB"+eol+"		);",
		"			content_z BLOB,"+eol+"			t155_col TEXT"+eol+"		);", 1)
	if withCol == withView {
		t.Fatal("fixture is stale: docs.content_z is no longer the last column")
	}
	if err := upgradeSchemaWith(dbPath, withCol); err != nil {
		t.Fatalf("column upgrade: %v", err)
	}
	if backups != 1 {
		t.Errorf("a column-adding migration took %d backups, want exactly 1", backups)
	}
}

// TestPreMigrationBackupPrunes is the test the retention ratchet's
// exemption promises exists (🎯T158).
//
// The migration path is allowed to call backup.BackupWith directly
// rather than CreateAndRetain, because it needs the destination path up
// front for boot-phase progress reporting. That exemption is only safe
// while it prunes by other means — this asserts it does. Without it, a
// day of three migrations added three snapshots and removed none, and
// with backups disabled or failing, pruning never happened at all.
func TestPreMigrationBackupPrunes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	s, err := New(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Older snapshots that retention should collect. keep resolves from
	// config, which is absent here, so the default (1) applies.
	backupDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"mnemo-daily-20260101T000000Z.db.zst",
		"mnemo-daily-20260102T000000Z.db.gz",
		"mnemo-pre-migration-20260103T000000Z.db.zst",
	} {
		if err := os.WriteFile(filepath.Join(backupDir, n), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	eol := "\n"
	if strings.Contains(schemaSQL, "\r\n") {
		eol = "\r\n"
	}
	// A column addition: table-touching, so it must take a snapshot.
	withCol := strings.Replace(schemaSQL,
		"			content_z BLOB"+eol+"		);",
		"			content_z BLOB,"+eol+"			t158_col TEXT"+eol+"		);", 1)
	if withCol == schemaSQL {
		t.Fatal("fixture is stale: docs.content_z is no longer the last column")
	}
	if err := upgradeSchemaWith(dbPath, withCol); err != nil {
		t.Fatalf("column upgrade: %v", err)
	}

	list, err := backup.List(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		var names []string
		for _, b := range list {
			names = append(names, b.Name)
		}
		t.Fatalf("after a migration the backup dir holds %d snapshots, want 1 "+
			"(the pre-migration path took one and pruned nothing): %v", len(list), names)
	}
	// The survivor must be the insurance just taken, not an older file.
	if list[0].Tag != backup.TagPreMigration {
		t.Errorf("kept a %s snapshot; the pre-migration backup should be newest and survive",
			list[0].Tag)
	}
}
