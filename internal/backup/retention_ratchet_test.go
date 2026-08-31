// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retentionExemptions lists the files allowed to call Backup/BackupWith
// directly instead of CreateAndRetain, with the reason each is safe.
//
// An exemption is a promise that the file prunes by other means. Adding
// one without doing so recreates the defect this ratchet exists to
// prevent, so the reason is required reading, not decoration.
var retentionExemptions = map[string]string{
	// The migration path cannot use CreateAndRetain: it needs the
	// destination path up front for the boot-phase progress reporting,
	// and it goes through the preMigrationBackup test seam. It calls
	// GCOldest explicitly right after a successful snapshot — verified
	// by TestPreMigrationBackupPrunes in the store package.
	"internal/store/store.go": "prunes explicitly via backup.GCOldest after the snapshot",
}

// TestEverySnapshotPathRetains is the structural half of 🎯T158
// acceptance 3.
//
// Two of the three snapshot-creating paths pruned nothing, and fixing
// those two would have fixed the bug of the day while leaving the fourth
// call site free to reintroduce it. Taking a backup and forgetting to
// prune has to stop being an available mistake, so this fails the build
// when a new caller reaches for Backup/BackupWith instead of
// CreateAndRetain.
//
// The cost of getting it wrong is asymmetric and grows as retention
// tightens: at keep=1 a path that does not prune doubles the backup
// directory every single time it runs.
func TestEverySnapshotPathRetains(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		// The backup package itself defines and composes these.
		if strings.HasPrefix(rel, "internal/backup/") {
			return nil
		}
		if _, exempt := retentionExemptions[rel]; exempt {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, call := range []string{"backup.Backup(", "backup.BackupWith("} {
			if strings.Contains(string(src), call) {
				offenders = append(offenders, rel+" calls "+call+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, o := range offenders {
		t.Errorf("%s — use backup.CreateAndRetain instead, so the snapshot is "+
			"pruned to the configured retention. If this path genuinely cannot "+
			"(it needs the destination path up front, say), add it to "+
			"retentionExemptions with the reason it prunes by other means, and a "+
			"test proving it does.", o)
	}
}

// TestExemptionsAreReal keeps the allowlist honest: an entry naming a
// file that no longer exists, or that no longer calls the raw API, is a
// stale promise that quietly widens the hole.
func TestExemptionsAreReal(t *testing.T) {
	root := filepath.Join("..", "..")
	for rel, reason := range retentionExemptions {
		if reason == "" {
			t.Errorf("%s is exempt with no stated reason", rel)
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("exempt file %s cannot be read (renamed or deleted?): %v", rel, err)
			continue
		}
		body := string(src)
		if !strings.Contains(body, "backup.Backup(") && !strings.Contains(body, "backup.BackupWith(") {
			t.Errorf("%s no longer calls Backup/BackupWith — drop the exemption", rel)
		}
		// The promise the exemption makes.
		if !strings.Contains(body, "GCOldest") {
			t.Errorf("%s is exempt on the promise that it prunes by other means, "+
				"but it does not mention GCOldest", rel)
		}
	}
}
