// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"

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

	// The shipped schema against itself: nothing to do.
	if ok, _ := planIsMetadataOnly(planFor(t, dbPath, schemaSQL)); ok {
		t.Error("an empty plan must not be classified metadata-only (there is nothing to skip)")
	}

	// A view redefinition — exactly the change that cost ~18 minutes of
	// VACUUM INTO on the live index.
	viewChange := strings.Replace(schemaSQL,
		"CREATE VIEW docs_v AS",
		"CREATE VIEW docs_v_extra AS SELECT id FROM docs;\nCREATE VIEW docs_v AS", 1)
	if viewChange == schemaSQL {
		t.Fatal("fixture is stale: docs_v is no longer declared this way")
	}
	ok, why := planIsMetadataOnly(planFor(t, dbPath, viewChange))
	if !ok {
		t.Errorf("a view-only plan must skip the backup, got required because: %s", why)
	}

	// An index addition is metadata too.
	indexChange := schemaSQL + "\nCREATE INDEX idx_docs_kind_t155 ON docs(kind);\n"
	if ok, why := planIsMetadataOnly(planFor(t, dbPath, indexChange)); !ok {
		t.Errorf("an index-only plan must skip the backup, got required because: %s", why)
	}

	// A new column changes the table's own definition: keep the backup.
	// Appended last, which is the only shape the append-only schema policy
	// permits — a column inserted mid-table makes sqlift plan a rebuild
	// instead, which this classifier also (correctly) refuses.
	colChange := strings.Replace(schemaSQL,
		"			content_z BLOB\n		);",
		"			content_z BLOB,\n			t155_probe TEXT\n		);", 1)
	if colChange == schemaSQL {
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
	rebuild := strings.Replace(schemaSQL,
		"			doc_source TEXT NOT NULL DEFAULT ''",
		"			doc_source TEXT NOT NULL DEFAULT '',\n			t155_mid TEXT", 1)
	ok, why = planIsMetadataOnly(planFor(t, dbPath, rebuild))
	if ok {
		t.Error("a plan rebuilding a table must keep the backup")
	}
	if !strings.Contains(why, "rebuild") {
		t.Errorf("a rebuild should be named as such, got %q", why)
	}

	// A new table likewise.
	tableChange := schemaSQL + "\nCREATE TABLE t155_new (id INTEGER PRIMARY KEY);\n"
	if ok, _ := planIsMetadataOnly(planFor(t, dbPath, tableChange)); ok {
		t.Error("a plan creating a table must keep the backup")
	}
}
