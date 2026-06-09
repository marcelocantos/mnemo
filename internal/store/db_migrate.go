// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"log/slog"
	"os"
)

// MigrateLegacyDBPath handles the one-time migration of mnemo.db from
// its pre-vault location (legacyPath) to its vault location (vaultPath).
//
//   - Only legacyPath exists → rename it to vaultPath (move, not copy).
//   - Both exist           → remove legacyPath (vaultPath is authoritative).
//   - Only vaultPath exists → nothing to do.
//   - Neither exists       → nothing to do (first run; New will create it).
func MigrateLegacyDBPath(vaultPath, legacyPath string) error {
	_, vaultErr := os.Stat(vaultPath)
	_, legacyErr := os.Stat(legacyPath)

	vaultExists := vaultErr == nil
	legacyExists := legacyErr == nil

	switch {
	case !legacyExists:
		// Nothing to migrate.
	case !vaultExists:
		if err := os.Rename(legacyPath, vaultPath); err != nil {
			return err
		}
		slog.Info("migrated legacy mnemo.db to vault", "from", legacyPath, "to", vaultPath)
	default:
		if err := os.Remove(legacyPath); err != nil {
			slog.Warn("could not remove stale legacy mnemo.db", "path", legacyPath, "err", err)
		} else {
			slog.Info("removed stale legacy mnemo.db", "path", legacyPath)
		}
	}
	return nil
}
