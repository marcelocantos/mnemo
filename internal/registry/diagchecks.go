// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/upgrade"
)

// BuildDiagRegistry assembles the daemon's self-diagnostics check
// registry (🎯T83), capturing the config, the summariser workdir, and an
// accessor for the default user's store + compaction watcher. daemonStart
// anchors the "backfill ran since startup" check. The returned registry is
// wired into the /health endpoint (via SetDiagRunner), the mnemo_doctor
// tool, and the diag scheduler.
//
// Optional upgrade detector and background lease (🎯T97.2 / 🎯T97.4) are
// read from the Registry when set via SetUpgradeDetector / SetLease.
func (r *Registry) BuildDiagRegistry(defaultUser string, daemonStart time.Time) *diag.Registry {
	reg := diag.NewRegistry()
	workDir := r.summariserWorkDir
	cfg := r.cfg

	// state returns the default user's store + watcher, or nils when that
	// user's workers have not been created yet.
	state := func() (*store.Store, *compact.Watcher) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if e, ok := r.stores[defaultUser]; ok {
			return e.store, e.compactWatcher
		}
		return nil, nil
	}

	reg.Register(
		diag.Check{Name: "compactor.workdir", Tier: diag.Full, Run: func(context.Context) diag.CheckResult {
			if workDir == "" {
				return diag.Failure(
					"no usable summariser working directory — compaction and CLAUDE.md review are disabled",
					"ensure the OS temp dir is writable, then: brew services restart mnemo")
			}
			fi, err := os.Stat(workDir)
			if err != nil || !fi.IsDir() {
				return diag.Failure(
					fmt.Sprintf("summariser workdir %q is missing", workDir),
					"brew services restart mnemo")
			}
			probe := filepath.Join(workDir, ".diagprobe")
			if werr := os.WriteFile(probe, []byte("x"), 0o600); werr != nil {
				return diag.Failure(
					fmt.Sprintf("summariser workdir %q is not writable", workDir),
					"fix permissions on the OS temp dir, then restart the daemon")
			}
			_ = os.Remove(probe)
			return diag.Healthy("summariser workdir present and writable: " + workDir)
		}},

		diag.Check{Name: "claude.path", Tier: diag.Full, Run: func(context.Context) diag.CheckResult {
			p, err := exec.LookPath("claude")
			if err != nil {
				return diag.Failure(
					"the `claude` binary is not on the daemon's PATH — compaction and review cannot run",
					"install Claude Code and put claude on PATH; for brew-services include the install dir (e.g. ~/.claude/local or /opt/homebrew/bin) in the service PATH")
			}
			return diag.Healthy("claude found at " + p)
		}},

		diag.Check{Name: "ingest.roots", Tier: diag.Full, Run: func(context.Context) diag.CheckResult {
			var missingReq, missingOpt []string
			if home, err := os.UserHomeDir(); err == nil {
				proj := filepath.Join(home, ".claude", "projects")
				if _, err := os.Stat(proj); err != nil {
					missingReq = append(missingReq, proj)
				}
			}
			optional := append([]string{}, cfg.ResolvedWorkspaceRoots()...)
			optional = append(optional, cfg.ResolvedSynthesisRoots()...)
			optional = append(optional, cfg.ExtraProjectDirs...)
			for _, d := range optional {
				if d == "" {
					continue
				}
				if _, err := os.Stat(expandTilde(d)); err != nil {
					missingOpt = append(missingOpt, d)
				}
			}
			if len(missingReq) > 0 {
				return diag.Failure(
					"transcript source missing: "+strings.Join(missingReq, ", "),
					"ensure ~/.claude/projects exists and is readable")
			}
			if len(missingOpt) > 0 {
				return diag.Warning(
					"configured roots not found: "+strings.Join(missingOpt, ", "),
					"remove stale roots from ~/.mnemo/config.json, or mount them")
			}
			return diag.Healthy("all configured roots resolve")
		}},

		diag.Check{Name: "compactor.breaker", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			_, w := state()
			if w == nil {
				if workDir == "" {
					return diag.Warning("compactor disabled (no summariser workdir)", "see compactor.workdir")
				}
				return diag.Healthy("compactor not started yet")
			}
			snap := w.BreakerSnapshot()
			if snap.Open {
				return diag.Failure(
					fmt.Sprintf("compaction circuit-breaker tripped after repeated systemic failures: %s", snap.LastError),
					"every compaction is failing for the same reason — check compactor.workdir and claude.path; the watcher retries after a cooldown")
			}
			return diag.Healthy("compaction watcher healthy")
		}},

		diag.Check{Name: "ingest.backfill", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			s, _ := state()
			if s == nil {
				return diag.Healthy("store not started yet")
			}
			if time.Since(daemonStart) < 10*time.Minute {
				return diag.Healthy("startup backfill in progress")
			}
			rows, err := s.Query("SELECT MAX(last_backfill) AS m FROM ingest_status")
			if err != nil || len(rows) == 0 {
				return diag.Healthy("no ingest_status rows yet")
			}
			m, _ := rows[0]["m"].(string)
			if m == "" {
				return diag.Warning("no backfill has completed", "check the daemon log for ingest errors")
			}
			if ts, perr := time.Parse(time.RFC3339, m); perr == nil && ts.Before(daemonStart) {
				return diag.Failure(
					"the indexer has not completed a backfill since the daemon started — ingestion may be stalled",
					"check the daemon log; a common cause is the compactor hammering (see compactor.breaker)")
			}
			return diag.Healthy("indexer has backfilled since startup")
		}},

		diag.Check{Name: "db.readable", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			s, _ := state()
			if s == nil {
				return diag.Healthy("store not started yet")
			}
			if _, err := s.Query("SELECT 1 AS ok"); err != nil {
				return diag.Failure(
					"the database is not responding to queries: "+err.Error(),
					"check ~/.mnemo/mnemo.db permissions and free disk space, then restart the daemon")
			}
			return diag.Healthy("database responsive")
		}},

		diag.Check{Name: "db.wal", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			s, _ := state()
			if s == nil {
				return diag.Healthy("store not started yet")
			}
			fi, err := os.Stat(s.DBPath() + "-wal")
			if err != nil {
				return diag.Healthy("no WAL backlog")
			}
			const warnAt = 256 << 20 // 256 MiB
			if fi.Size() > warnAt {
				return diag.Warning(
					fmt.Sprintf("WAL is large (%d MiB) — a writer may be stuck or checkpoints are overdue", fi.Size()>>20),
					"if it keeps growing, restart the daemon; a wedged worker shows up as compactor.breaker")
			}
			return diag.Healthy("WAL size healthy")
		}},

		// 🎯T97.2: newer release available (warn). Uses the last
		// Detector snapshot so the diag path itself does not force a
		// network call — the detector worker owns polling.
		diag.Check{Name: "upgrade.available", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			d := r.UpgradeDetector()
			if d == nil {
				return diag.Healthy("upgrade detector not configured")
			}
			snap := d.Snapshot()
			if snap.Disabled {
				return diag.Healthy("upgrade check disabled (disable_upgrade_check)")
			}
			if snap.LastError != "" && snap.Latest == "" {
				return diag.Warning(
					"could not query latest release: "+snap.LastError,
					"ensure `gh` is installed and authenticated, or set disable_upgrade_check")
			}
			if snap.UpgradeAvail {
				return diag.Warning(
					fmt.Sprintf("upgrade available: running v%s, latest %s",
						upgrade.NormalizeTag(snap.Current), snap.Latest),
					"brew upgrade mnemo  # or enable auto_upgrade.enabled on Homebrew")
			}
			if snap.Latest == "" {
				return diag.Healthy("upgrade check has not completed yet")
			}
			return diag.Healthy(fmt.Sprintf("up to date (running v%s, latest %s)",
				upgrade.NormalizeTag(snap.Current), snap.Latest))
		}},

		// 🎯T97.4: singleton background lease ownership.
		diag.Check{Name: "background.lease", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			l := r.Lease()
			if l == nil {
				return diag.Healthy("background lease not configured")
			}
			st := l.Status()
			if st.HeldLocally {
				detail := fmt.Sprintf("this process holds the background lease (%s)", st.LocalHolderID)
				if st.RunningBG {
					detail += "; singleton background work running"
				}
				return diag.Healthy(detail)
			}
			if st.FilePresent && st.FileHolder != "" && !st.Expired {
				return diag.Healthy(fmt.Sprintf(
					"background lease held by %s (this process is serve-only)", st.FileHolder))
			}
			return diag.Warning(
				"no live background lease holder — ingest/compaction may be paused",
				"ensure exactly one mnemo backend is running, or check ~/.mnemo/background.lease")
		}},
	)

	// 🎯T102.3 / T102.8: plugin ready checks + signal_sources expand live.
	reg.SetDynamic(func() []diag.Check {
		var out []diag.Check
		if pm := r.PluginManager(); pm != nil {
			out = append(out, pm.DynamicChecks()...)
		}
		if se := r.SignalEvaluator(); se != nil {
			out = append(out, se.DiagChecks()...)
		}
		return out
	})
	return reg
}

// expandTilde resolves a leading ~ to the process home dir.
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
