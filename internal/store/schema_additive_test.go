package store

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/sqlift/go/sqlift"
)

// TestSchemaUpgradeIsAdditive builds a database on the LAST RELEASED
// schema and plans the migration to the current one, asserting it needs
// none of sqlift's gates.
//
// The append-only policy is not advisory: a plan requiring a rebuild
// cannot be applied to any existing installation, so the feature would
// work nowhere except a fresh install. Nothing else catches this. Every
// test store is created fresh and is therefore already on the current
// schema, so the entire suite stays green while shipping a migration that
// cannot run — which is exactly what happened when 🎯T135 added four
// generated columns to `entries`. sqlift plans a full table REBUILD to add
// a column, and that is forbidden. It surfaced only when the migration was
// planned against a copy of a real database.
//
// Skips when git or the tag is unavailable rather than failing: a missing
// baseline is not evidence of a good migration, and saying so is better
// than a green tick that means nothing.
func TestSchemaUpgradeIsAdditive(t *testing.T) {
	prev := previousSchema(t)

	dbPath := filepath.Join(t.TempDir(), "old.db")
	sdb, err := sqlift.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := sqlift.Parse(prev)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := sqlift.Extract(sdb)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := sqlift.Diff(empty, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlift.Apply(sdb, plan, sqlift.ApplyOptions{}); err != nil {
		t.Fatalf("build previous schema: %v", err)
	}

	current, err := sqlift.Extract(sdb)
	if err != nil {
		t.Fatal(err)
	}

	// CONTROL: diff the previous schema against ITSELF. Any operations
	// here are sqlift round-trip noise, not a consequence of the change
	// under test, and every conclusion below would be measuring the wrong
	// thing.
	if self, err := sqlift.Diff(current, parsed); err != nil {
		t.Fatal(err)
	} else if !self.Empty() {
		for _, op := range self.Operations() {
			t.Logf("CONTROL op: %s %s — %s", op.Type, op.ObjectName, op.Description)
		}
		t.Fatalf("control diff is non-empty (%d ops): the previous schema does "+
			"not round-trip, so this probe cannot attribute anything",
			len(self.Operations()))
	}

	desired, err := sqlift.Parse(schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	up, err := sqlift.Diff(current, desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range up.Operations() {
		t.Logf("op: %+v", op)
	}
	if err := sqlift.Apply(sdb, up, sqlift.ApplyOptions{}); err != nil {
		t.Fatalf("upgrade is NOT additive: %v", err)
	}
}

// previousSchema returns schema.sql as of the most recent release tag —
// the schema an upgrading user actually has on disk.
func previousSchema(t *testing.T) string {
	t.Helper()
	tag, err := exec.Command("git", "describe", "--tags", "--abbrev=0", "--match", "v*").Output()
	if err != nil {
		t.Skipf("no release tag to diff against: %v", err)
	}
	ref := strings.TrimSpace(string(tag)) + ":internal/store/schema.sql"
	out, err := exec.Command("git", "show", ref).Output()
	if err != nil {
		t.Skipf("cannot read %s: %v", ref, err)
	}
	t.Logf("baseline schema: %s", strings.TrimSpace(string(tag)))
	return string(out)
}
