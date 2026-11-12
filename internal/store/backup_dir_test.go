// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/backup"
)

// 🎯T199: the pre-migration backup uses the configured `backup.dir`,
// like every other backup path, and an absent destination skips it
// without writing beside the database.

// schemaWithExtraDocsColumn returns base with one more column on docs: a
// migration that touches table data and so must be insured by a
// pre-migration backup. base is schemaSQL, possibly extended.
func schemaWithExtraDocsColumn(t *testing.T, base, col string) string {
	t.Helper()
	// schema.sql keeps the line ending of the checkout (CRLF under
	// Windows autocrlf); build the variant in the file's own terms.
	eol := "\n"
	if strings.Contains(base, "\r\n") {
		eol = "\r\n"
	}
	tail := "			plain_len INTEGER," + eol + "			z_len INTEGER"
	out := strings.Replace(base, tail+eol+"		);", tail+","+eol+"			"+col+" TEXT"+eol+"		);", 1)
	if out == base {
		t.Fatal("fixture is stale: docs.z_len is no longer the last column")
	}
	return out
}

// homeWithBackupDir makes an isolated mnemo home whose config.json sets
// backup.dir, and returns the path of the database that config governs.
func homeWithBackupDir(t *testing.T, backupDir string) (dbPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(MnemoHomeEnv, home)
	if err := os.MkdirAll(filepath.Join(home, ".mnemo"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"backup": {"dir": ` + jsonString(backupDir) + `}}`
	if err := os.WriteFile(filepath.Join(home, ".mnemo", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".mnemo", "mnemo.db")
}

// jsonString quotes s as a JSON string (Windows paths carry backslashes).
func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// seedStoreAt opens and closes a store at dbPath so the schema exists.
func seedStoreAt(t *testing.T, dbPath string) {
	t.Helper()
	installTestRateCard(t, testRateCard())
	s, err := New(dbPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// config.json governs the database it sits beside, and no other: a
// fixture or a copy opened elsewhere must never write into, or prune,
// the owner's backup directory.
func TestBackupDirFromConfig(t *testing.T) {
	custom := t.TempDir()
	dbPath := homeWithBackupDir(t, custom)
	if got := backupDirFromConfig(dbPath); got != custom {
		t.Errorf("database beside config.json: backup dir = %s, want the configured %s", got, custom)
	}
	other := filepath.Join(t.TempDir(), "t.db")
	if got, want := backupDirFromConfig(other), backup.DefaultDir(other); got != want {
		t.Errorf("unrelated database: backup dir = %s, want its own default %s", got, want)
	}
}

func TestPreMigrationBackupHonoursBackupDir(t *testing.T) {
	custom := t.TempDir()
	dbPath := homeWithBackupDir(t, custom)
	seedStoreAt(t, dbPath)

	if err := upgradeSchemaWith(dbPath, schemaWithExtraDocsColumn(t, schemaSQL, "t199_col")); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	list, err := backup.List(custom)
	if err != nil || len(list) != 1 || list[0].Tag != backup.TagPreMigration {
		t.Fatalf("configured directory holds %v (err=%v), want one pre-migration snapshot", list, err)
	}
	def := backup.DefaultDir(dbPath)
	if stray, _ := backup.List(def); len(stray) != 0 {
		t.Errorf("a snapshot was also written beside the database: %v", stray)
	}
	// The record of the attempt stays beside the database.
	if a, ok, err := backup.LastAttempt(def); err != nil || !ok || !a.OK || a.Tag != backup.TagPreMigration {
		t.Errorf("record beside the database = %+v ok=%v err=%v, want the successful pre-migration attempt", a, ok, err)
	}
	if _, ok, _ := backup.LastAttempt(custom); ok {
		t.Errorf("a record was written into the configured directory")
	}
}

// The destination is away during an upgrade: the migration proceeds, the
// skip is recorded, and no snapshot lands beside the database.
func TestPreMigrationBackupDestinationAway(t *testing.T) {
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.WriteFile(volume, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := homeWithBackupDir(t, filepath.Join(volume, "mnemo", "backups"))
	seedStoreAt(t, dbPath)

	orig := preMigrationBackup
	preMigrationBackup = func(src, dest string, args *backup.BackupArgs) (backup.Result, error) {
		t.Errorf("pre-migration snapshot started with the destination away (dest %s)", dest)
		return orig(src, dest, args)
	}
	t.Cleanup(func() { preMigrationBackup = orig })

	if err := upgradeSchemaWith(dbPath, schemaWithExtraDocsColumn(t, schemaSQL, "t199_col")); err != nil {
		t.Fatalf("upgrade with the destination away: %v", err)
	}
	def := backup.DefaultDir(dbPath)
	a, ok, err := backup.LastAttempt(def)
	if err != nil || !ok || !a.Unavailable || !a.Skipped || a.Tag != backup.TagPreMigration {
		t.Fatalf("record = %+v ok=%v err=%v, want an unavailable pre-migration skip", a, ok, err)
	}
	if stray, _ := backup.List(def); len(stray) != 0 {
		t.Errorf("a snapshot was written beside the database instead: %v", stray)
	}
}
