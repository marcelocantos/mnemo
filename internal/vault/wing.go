// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// mnemoWingDir is the library-wing namespace inside the vault root.
const mnemoWingDir = "_mnemo"

// mnemoWingFiles lists the static-ish notes T64.2 writes into the
// wing. Each is fence-aware (writeNote preserves below-fence
// annotations). MIGRATION.md is NOT in this list — it has its own
// write-once contract (see writeMigrationDoc).
var mnemoWingFiles = []struct {
	name   string
	render func() string
}{
	{"index.md", renderMnemoIndex},
	{"README.md", renderMnemoReadme},
}

// syncMnemoWing performs the T64.2 library-wing work on each Sync:
//
//  1. Load state.json sidecar; default to a fresh one if missing.
//  2. Stamp the first observation of the active layout (idempotent).
//  3. When layout ∈ {both, v2}: ensure <vault>/_mnemo/ exists and
//     write the index.md + README.md notes via the fence-preserving
//     writeNote so user annotations below the fence persist.
//  4. When layout == "both" AND a v1 layout is detectable AND we have
//     not written MIGRATION.md before: write _mnemo/MIGRATION.md once.
//     User-deleted MIGRATION.md is treated as "read; move on" and is
//     not regenerated.
//  5. Emit the weekly "opt into v2" structured warning if the vault
//     has been in "both" past the soak window and the last warning
//     was ≥ 7 days ago. Stamps last_soak_warn_at on emit.
//  6. Persist state.json (no-op if loaded ReadOnly because the file's
//     version is newer than this binary understands).
//
// Failures in any sub-step are logged at warn level rather than
// returning an error: the wing is auxiliary to the rest of Sync and
// must not block v1 writers when (e.g.) state.json is unwritable.
func (e *Exporter) syncMnemoWing(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	layout := e.effectiveLayout()

	statePath, err := e.effectiveStatePath()
	if err != nil {
		slog.Warn("vault: resolve state.json path failed", "err", err)
		return
	}
	st, err := store.LoadState(statePath)
	if err != nil {
		slog.Warn("vault: load state.json failed", "path", statePath, "err", err)
		return
	}

	// vault_path tracking: a real path change resets the first-seen
	// counters per design ("vault_path change semantics"). A fresh
	// state.json with VaultPath=="" is NOT a path change — the
	// stamps below populate first_seen for the first time. Resetting
	// in that case would wipe state seeded by tests or migrations.
	if st.VaultPath != "" && st.VaultPath != e.path {
		st.ResetVaultLayoutFirstSeen()
	}
	st.VaultPath = e.path

	st.StampLayoutFirstSeen(layout, now)

	if layout != store.VaultLayoutV1 {
		wingDir := filepath.Join(e.path, mnemoWingDir)
		if err := os.MkdirAll(wingDir, 0o755); err != nil {
			slog.Warn("vault: create _mnemo dir failed", "path", wingDir, "err", err)
		} else {
			for _, f := range mnemoWingFiles {
				absPath := filepath.Join(wingDir, f.name)
				content := f.render()
				if err := writeNote(absPath, content, ""); err != nil {
					slog.Warn("vault: write wing note failed", "path", absPath, "err", err)
					continue
				}
				e.recordOutput(filepath.Join(mnemoWingDir, f.name), "mnemo_wing", "wing", content)
			}
		}

		if layout == store.VaultLayoutBoth && st.MigrationDocWrittenAt.IsZero() {
			if err := writeMigrationDoc(e.path, renderMnemoMigration(e.path)); err != nil {
				slog.Warn("vault: write MIGRATION.md failed", "err", err)
			} else {
				st.MigrationDocWrittenAt = now.UTC()
			}
		}

		// Bridges (🎯T64.6): reconcile configured bridges against anchor
		// files + the written-bridge record. Runs even when no bridges
		// are configured so a removed bridge's block is stripped. Mutates
		// st (WrittenBridges + BridgeErrors); persisted by st.Write below.
		e.syncBridges(st)
	}

	soak := e.effectiveSoakWarn()
	if e.maybeSoakWarn(st, layout, now, soak) {
		st.LastSoakWarnAt = now.UTC()
	}

	if err := st.Write(statePath); err != nil {
		slog.Warn("vault: write state.json failed", "path", statePath, "err", err)
	}
}

// maybeSoakWarn emits the "opt into v2" structured warning if the
// vault is in "both" layout and has been past the soak window for
// ≥ 7 days since the last warning (or never warned). Returns true
// when a warning was emitted so the caller can stamp
// last_soak_warn_at.
func (e *Exporter) maybeSoakWarn(st *store.State, layout string, now time.Time, soak time.Duration) bool {
	if layout != store.VaultLayoutBoth {
		return false
	}
	first := st.LayoutFirstSeen(store.VaultLayoutBoth)
	if first.IsZero() {
		return false
	}
	elapsed := now.Sub(first)
	if elapsed < soak {
		return false
	}
	// Weekly cadence: never two warnings within 7 days. Zero
	// LastSoakWarnAt means "never warned" → fire immediately.
	const weeklyCadence = 7 * 24 * time.Hour
	if !st.LastSoakWarnAt.IsZero() && now.Sub(st.LastSoakWarnAt) < weeklyCadence {
		return false
	}
	days := int(elapsed.Hours() / 24)
	slog.Warn(
		"vault: soak window exceeded — opt into v2 or run mnemo_vault_gc_legacy",
		"vault_layout", store.VaultLayoutBoth,
		"days_in_both", days,
		"soak_warn_after_hours", int(soak.Hours()),
	)
	return true
}

// writeMigrationDoc writes _mnemo/MIGRATION.md exactly once. The
// file has NO fence: from mnemo's point of view it is user-owned
// content from the moment it lands on disk. The caller checks
// State.MigrationDocWrittenAt to enforce write-once; this function
// also skips silently if the file already exists, so a deletion
// reset on state.json never causes a re-write.
func writeMigrationDoc(vaultPath, content string) error {
	dst := filepath.Join(vaultPath, mnemoWingDir, "MIGRATION.md")
	if _, err := os.Stat(dst); err == nil {
		return nil // file already exists; do not overwrite or re-touch
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat MIGRATION.md: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir wing: %w", err)
	}
	return os.WriteFile(dst, []byte(content), 0o644)
}

// RegenerateMigrationDoc writes _mnemo/MIGRATION.md unconditionally,
// overwriting any existing file. The mnemo_vault_migration_doc MCP
// tool routes through here when called with write=true. Intended as
// the only legitimate way to bring MIGRATION.md back after a user
// deletion: the write-once contract holds in the sync path; this is
// the explicit opt-in escape hatch.
//
// MigrationDocSnapshot returns the same content without touching the
// filesystem so the MCP tool can preview / return the snapshot in
// write=false mode.
func (e *Exporter) RegenerateMigrationDoc() (string, error) {
	content := renderMnemoMigration(e.path)
	dst := filepath.Join(e.path, mnemoWingDir, "MIGRATION.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("mkdir wing: %w", err)
	}
	if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write MIGRATION.md: %w", err)
	}
	return content, nil
}

// MigrationDocSnapshot returns the MIGRATION.md content mnemo would
// write for this vault right now, without touching the filesystem.
// Used by mnemo_vault_migration_doc(write=false).
func (e *Exporter) MigrationDocSnapshot() string {
	return renderMnemoMigration(e.path)
}

// effectiveLayout returns the active layout, defaulting to "v2" so a
// zero-Options Exporter (used by tests that pre-date T64.2) keeps
// working without surfacing v1 paths it never wrote in the first
// place.
func (e *Exporter) effectiveLayout() string {
	switch e.layout {
	case store.VaultLayoutV1, store.VaultLayoutBoth, store.VaultLayoutV2:
		return e.layout
	}
	return store.VaultLayoutV2
}

// effectiveSoakWarn returns the configured soak window, defaulting
// to 720h when unset.
func (e *Exporter) effectiveSoakWarn() time.Duration {
	if e.soakWarn > 0 {
		return e.soakWarn
	}
	return 720 * time.Hour
}

// effectiveStatePath returns the absolute state.json path, falling
// back to ~/.mnemo/state.json when no override was supplied.
func (e *Exporter) effectiveStatePath() (string, error) {
	if e.statePath != "" {
		return e.statePath, nil
	}
	return store.DefaultStatePath()
}

// Layout exposes the active vault_layout for the mnemo_vault_status
// MCP tool to surface.
func (e *Exporter) Layout() string { return e.effectiveLayout() }

// SoakWarnAfter exposes the configured soak window so the status
// tool can render `soak_warn_after_hours` alongside the layout
// block.
func (e *Exporter) SoakWarnAfter() time.Duration { return e.effectiveSoakWarn() }

// StatePath exposes the state.json location so the status tool can
// read it for the vault_layout block.
func (e *Exporter) StatePath() (string, error) { return e.effectiveStatePath() }

// renderMnemoIndex builds _mnemo/index.md content. The note is
// intentionally short and stable: it points the user at the README,
// names the library-wing contract, and is fence-aware so users can
// add their own annotations below the line.
func renderMnemoIndex() string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("tags: [mnemo, mnemo/index]\n")
	b.WriteString("---\n\n")
	b.WriteString("# mnemo library wing\n\n")
	b.WriteString("This directory is mnemo's library wing. Everything inside\n")
	b.WriteString("`_mnemo/` is generated and/or maintained by mnemo. The wing\n")
	b.WriteString("groups second-order knowledge (themes, patterns, decisions,\n")
	b.WriteString("memories, lessons) extracted from your Claude Code session\n")
	b.WriteString("history so it sits cleanly alongside — but does not mix\n")
	b.WriteString("into — your own vault notes.\n\n")
	b.WriteString("- Read [[README]] for the contract and what each subdirectory holds.\n")
	b.WriteString("- Use the `-tag:mnemo` filter to exclude this wing from your search results.\n")
	b.WriteString("- Anything below the generated fence on any mnemo-owned page is\n")
	b.WriteString("  preserved across re-syncs.\n")
	return b.String()
}

// renderMnemoReadme builds _mnemo/README.md content — the contract
// page. Names the fence convention, the tag namespace, and the
// rules a user can rely on across syncs.
func renderMnemoReadme() string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("tags: [mnemo, mnemo/readme]\n")
	b.WriteString("---\n\n")
	b.WriteString("# Reading the mnemo wing\n\n")
	b.WriteString("## What this directory is\n\n")
	b.WriteString("`_mnemo/` is the **library wing** of your vault: a fenced\n")
	b.WriteString("namespace where mnemo writes its derived knowledge so it\n")
	b.WriteString("never collides with your own notes. Future syncs only touch\n")
	b.WriteString("files inside this directory.\n\n")
	b.WriteString("## The fence contract\n\n")
	b.WriteString("On every mnemo-owned page (except `MIGRATION.md`, which is\n")
	b.WriteString("write-once — see below), content above the line:\n\n")
	b.WriteString("```\n")
	b.WriteString("<!-- mnemo:generated -->\n")
	b.WriteString("```\n\n")
	b.WriteString("is regenerated on every sync. Anything you add **below** the\n")
	b.WriteString("line is preserved indefinitely. Move your annotations there\n")
	b.WriteString("and they survive every future write.\n\n")
	b.WriteString("## Tag namespace\n\n")
	b.WriteString("Every mnemo-generated note carries `mnemo` plus one of:\n\n")
	b.WriteString("- `mnemo/theme`\n")
	b.WriteString("- `mnemo/pattern`\n")
	b.WriteString("- `mnemo/cross-repo`\n")
	b.WriteString("- `mnemo/lesson`\n")
	b.WriteString("- `mnemo/decision`\n")
	b.WriteString("- `mnemo/memory`\n\n")
	b.WriteString("Use `-tag:mnemo` to exclude the wing from search; use\n")
	b.WriteString("`tag:mnemo/<type>` to scope a query.\n\n")
	b.WriteString("## MIGRATION.md\n\n")
	b.WriteString("If this vault held a v1-shape mnemo export (root-level\n")
	b.WriteString("`sessions/`, `decisions/`, `memories/`, etc.), a one-time\n")
	b.WriteString("`MIGRATION.md` was written here. Read it, then delete it —\n")
	b.WriteString("mnemo treats deletion as acknowledgement and will not\n")
	b.WriteString("recreate the file. If you need it back, call the\n")
	b.WriteString("`mnemo_vault_migration_doc(write: true)` MCP tool.\n\n")
	b.WriteString("## Want to leave?\n\n")
	b.WriteString("Set `vault_path: \"\"` in `~/.mnemo/config.json` and mnemo\n")
	b.WriteString("stops writing. The wing's contents remain on disk —\n")
	b.WriteString("delete them yourself when you no longer need them.\n")
	return b.String()
}

// renderMnemoMigration builds _mnemo/MIGRATION.md content. The note
// explains the v1 → v2 transition for THIS specific vault: where the
// old data lives, what the new wing means, and how to finish the
// migration. The vaultPath argument is interpolated as a reminder.
//
// This is written once on v1-detection and treated as user-owned
// from that moment forward. Do not include a `mnemo:generated`
// fence: the contract is "say something once, then leave it alone."
func renderMnemoMigration(vaultPath string) string {
	var b strings.Builder
	b.WriteString("# Migrating from mnemo v1 to v2\n\n")
	b.WriteString("> This file was written **once** when mnemo first detected a v1-shape\n")
	b.WriteString("> export in this vault. mnemo will not regenerate it. Once you have\n")
	b.WriteString("> read this, delete the file — your deletion is mnemo's signal\n")
	b.WriteString("> that you have understood. To bring this document back, call\n")
	b.WriteString("> `mnemo_vault_migration_doc(write: true)`.\n\n")
	b.WriteString("## What changed\n\n")
	b.WriteString("Earlier mnemo versions wrote sessions, decisions, memories,\n")
	b.WriteString("plans, targets, CI runs, PRs, repos, and configs as flat\n")
	b.WriteString("subdirectories at the vault root. Every Markdown file under\n")
	b.WriteString("`" + vaultPath + "` was mnemo's content, mixed in with your\n")
	b.WriteString("own notes.\n\n")
	b.WriteString("In v2 the data has moved into a single, clearly-fenced wing:\n")
	b.WriteString("`_mnemo/`. The v1 root-level directories are still readable;\n")
	b.WriteString("mnemo's writers continue to update them while the layout is\n")
	b.WriteString("`\"both\"` so you do not lose history during the transition.\n\n")
	b.WriteString("## What's happening right now\n\n")
	b.WriteString("Your `vault_layout` is `\"both\"`. mnemo dual-writes to:\n\n")
	b.WriteString("- The v1 root-level directories (kept as-is for continuity).\n")
	b.WriteString("- The new `_mnemo/` wing (see `_mnemo/README.md` for the contract).\n\n")
	b.WriteString("`mnemo_vault_status` reports `vault_layout.days_in_both` so\n")
	b.WriteString("you can see how long the transition has been running.\n\n")
	b.WriteString("## When you are ready to commit to v2\n\n")
	b.WriteString("1. Read the v1 content you care about. Move anything you want\n")
	b.WriteString("   to keep into your own vault — files outside `_mnemo/` and\n")
	b.WriteString("   outside the v1 directories will not be touched.\n")
	b.WriteString("2. Set `\"vault_layout\": \"v2\"` in `~/.mnemo/config.json`.\n")
	b.WriteString("   mnemo will stop writing to the v1 directories.\n")
	b.WriteString("3. Run `mnemo_vault_gc_legacy` (when available) to delete the\n")
	b.WriteString("   v1 root-level directories. Until then, you can remove them\n")
	b.WriteString("   manually — mnemo will not recreate them under `\"v2\"`.\n\n")
	b.WriteString("If you are not ready, do nothing. mnemo will warn weekly that\n")
	b.WriteString("dual-write is past the soak window but will never auto-narrow\n")
	b.WriteString("the layout. The timing is yours.\n")
	return b.String()
}
