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

	"github.com/marcelocantos/mnemo/internal/boot"
	"github.com/marcelocantos/mnemo/internal/compact"
	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/throttle"
	"github.com/marcelocantos/mnemo/internal/upgrade"
)

// BuildDiagRegistry assembles the daemon's self-diagnostics check
// registry (🎯T83), capturing the config, the summariser workdir, and an
// accessor for the default user's store + compaction watcher. daemonStart
// anchors the "backfill ran since startup" check. The returned registry is
// wired into the /health endpoint (via SetDiagRunner), the mnemo_ops op=doctor
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
		// Process boot phase: listen-first + deferred schema upgrade
		// (🎯T114 / 🎯T114.1). PhaseReady means tools can run; Upgrade
		// overlay means backup/apply still running concurrently.
		diag.Check{Name: "startup.ready", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			st := boot.Get()
			switch st.Phase {
			case boot.PhaseReady:
				if st.Upgrade != "" {
					return diag.Warning(
						"default-user store serving; schema upgrade in progress: "+st.Upgrade,
						"tools and /health stay up — pre-migration VACUUM INTO + compression run concurrently with the live SQLite pools; wait for apply to finish for new-schema features")
				}
				return diag.Healthy("default-user store ready")
			case boot.PhaseFailed:
				return diag.Failure(
					"startup failed: "+st.Detail,
					"check the daemon log; fix the underlying open/migration error and restart mnemo")
			case boot.PhasePreMigrationBackup:
				return diag.Warning(
					boot.Summary(),
					"pre-migration backup (VACUUM INTO + compression) can take several minutes on multi-GB DBs; HTTP stays up — wait, or check the daemon log")
			case boot.PhaseApplyingSchema:
				return diag.Warning(
					boot.Summary(),
					"schema migration in progress; wait for apply + ANALYZE to finish")
			default:
				return diag.Warning(
					boot.Summary(),
					"daemon is still bringing up the default-user store; /health stays available")
			}
		}},

		// Budget projection (🎯T135). Full tier: it runs several usage
		// aggregations, and a budget does not change on a three-minute
		// timescale.
		//
		// Deliberately warn-not-fail even when the cap is already
		// breached. A fail transition fires an OS notification, and mnemo
		// cannot stop the spend that caused this — notifying about an
		// unstoppable condition on every pass is how an alert becomes
		// something to dismiss reflexively.
		diag.Check{Name: "budget.projection", Tier: diag.Full, Run: func(context.Context) diag.CheckResult {
			st, _ := state()
			if st == nil {
				return diag.Healthy("no default-user store yet")
			}
			bcfg := cfg.Budget
			if bcfg.MonthlyCapUSD <= 0 {
				return diag.Healthy("no budget cap configured; spend is reported but not watched")
			}
			b, err := st.BudgetStatusNow(bcfg, time.Now())
			if err != nil {
				return diag.Warning("budget status failed: "+err.Error(),
					"check mnemo_budget for the underlying query error")
			}
			if !b.Priced {
				// The one case that must not read as healthy spending:
				// $0.00 here means nothing could be priced, not that
				// nothing was spent.
				return diag.Warning(b.Headline,
					`set {"pricing": {"enabled": true}} in ~/.mnemo/config.json so mnemo can fetch the model rate card`)
			}
			switch b.Severity {
			case "over", "warn":
				// State the governed fraction. mnemo can throttle only the
				// agents it invokes itself (🎯T136); a rising total
				// alongside an active throttle reads as a broken throttle
				// unless the report separates governed from observed.
				return diag.Warning(
					fmt.Sprintf("%s Of that, $%.2f (%.0f%%) is mnemo's own background agents.",
						b.Headline, b.GovernedUSD, b.GovernedPct),
					"run mnemo_budget for the sessions driving it, each with its repo, "+
						"working directory and live pid where it is still running. Only "+
						"mnemo's own agents can be throttled automatically; Claude Code "+
						"sessions and their sub-agents are observed but not gateable")
			default:
				return diag.Healthy(b.Headline)
			}
		}},

		// Throttle state (🎯T136). Steady severity is warn (intentional
		// control, not "daemon dying"). Interruptive loudness on engage/
		// lift is PushAlert from EvaluateThrottle (🎯T140) — not Observe —
		// so the default Fail notify threshold does not silence transitions
		// and does not drag budget.projection into banners.
		diag.Check{Name: "budget.throttle", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			detail, remediation := r.governor.Describe()
			if r.governor.State().Level == throttle.Full {
				return diag.Healthy(detail)
			}
			return diag.Warning(detail, remediation)
		}},

		diag.Check{Name: "schema.upgrade", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			st := boot.Get()
			if st.Upgrade == "" {
				return diag.Healthy("no schema upgrade in progress")
			}
			return diag.Warning(
				st.Upgrade,
				"store is serving on the current schema; backup then apply run in the background (SQLite concurrent VACUUM INTO)")
		}},

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

		// 🎯T142: FD-bounded tree watching — backend, open FDs, cap hit.
		diag.Check{Name: "watch.fds", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			s, _ := state()
			if s == nil {
				return storeNotReadyResult("watch.fds")
			}
			tel := s.WatchTelemetrySnapshot()
			sev, detail, rem := store.EvaluateWatchHealth(tel)
			switch sev {
			case "fail":
				return diag.Failure(detail, rem)
			case "warn":
				return diag.Warning(detail, rem)
			default:
				return diag.Healthy(detail)
			}
		}},

		// 🎯T145: stream reconciler overdue / hung pass visibility.
		// Startup capability graph (🎯T154). startup.ready reports the
		// coarse boot phase; this reports the declared capabilities, so a
		// phase that failed or never resolved is visible here rather than
		// only as a downstream symptom (a silently skipped backfill, an
		// empty query result, a hung test).
		diag.Check{Name: "startup.capabilities", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			st, _ := state()
			if st == nil {
				return storeNotReadyResult("startup.capabilities")
			}
			var pending, unavailable []string
			for _, c := range st.StartupReport() {
				switch c.State {
				case "pending":
					pending = append(pending, c.Name)
				case "unavailable":
					unavailable = append(unavailable, c.Name+" ("+c.Reason+")")
				}
			}
			switch {
			case len(unavailable) > 0:
				return diag.Warning(
					"startup capabilities unavailable: "+strings.Join(unavailable, "; "),
					"the store is serving degraded: work requiring these is skipped with a logged reason rather than erroring. A failed schema upgrade is the usual cause — check the daemon log and restart to retry")
			case len(pending) > 0:
				return diag.Warning(
					"startup capabilities still resolving: "+strings.Join(pending, ", "),
					"normal during an upgrade boot (pre-migration backup, then the entries materialisation pass); if it persists past those, check the daemon log for a stuck phase")
			}
			return diag.Healthy("all startup capabilities available")
		}},
		diag.Check{Name: "streams.overdue", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			s, _ := state()
			if s == nil {
				return storeNotReadyResult("streams.overdue")
			}
			reports := s.StreamHealthReports(s.StreamReconcilers(), time.Now())
			var overdue []string
			for _, r := range reports {
				if r.Overdue {
					overdue = append(overdue, r.Name+": "+r.OverdueDetail)
				}
			}
			if len(overdue) == 0 {
				return diag.Healthy("all registered streams completed within " +
					"3× their interval (or daemon uptime is still within budget)")
			}
			return diag.Warning(
				"overdue stream pass(es): "+strings.Join(overdue, "; "),
				"inspect logs for reconcile failed / pass timeout; a hung stream no longer blocks others but needs attention")
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
				return storeNotReadyResult("ingest.backfill")
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
				return storeNotReadyResult("db.readable")
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
				return storeNotReadyResult("db.wal")
			}
			fi, err := os.Stat(s.DBPath() + "-wal")
			if err != nil {
				return diag.Healthy("no WAL backlog")
			}
			// Size alone cannot tell a fault from a high-water mark:
			// SQLite reuses the -wal from offset zero rather than
			// shrinking it, so a big file usually means "was busy once",
			// not "writes are not landing". Report it, but only call it a
			// problem when it is still GROWING between checks — that is
			// the shape of a reader pinning the WAL or a wedged writer.
			const warnAt = 256 << 20 // 256 MiB
			size := fi.Size()
			grew := s.NoteWALSize(size)
			if size > warnAt && grew {
				return diag.Warning(
					fmt.Sprintf("WAL is %d MiB and still growing — a long-running reader is "+
						"blocking checkpoints, or a writer is stuck", size>>20),
					"long readers (backup VACUUM INTO, image backfill) pin the WAL until they "+
						"finish; if it keeps climbing with none running, restart the daemon — "+
						"a wedged worker also shows up as compactor.breaker")
			}
			if size > warnAt {
				return diag.Healthy(fmt.Sprintf(
					"WAL is %d MiB but stable (space is reused, not leaked); "+
						"maintenance truncates it when writes go quiet", size>>20))
			}
			return diag.Healthy("WAL size healthy")
		}},

		// 🎯T121: whether the image embedder ran or was skipped, and why,
		// without reading the daemon log. Never fails — an absent embedder
		// is a normal deployment, not a fault.
		diag.Check{Name: "images.embedder", Tier: diag.Fast, Run: func(context.Context) diag.CheckResult {
			s, _ := state()
			if s == nil {
				return diag.Healthy("store not started yet")
			}
			es := s.EmbedderStatus()
			if !es.Enabled {
				return diag.Healthy(fmt.Sprintf(
					"image embeddings skipped (%s): %s", es.Reason, es.Detail))
			}
			detail := fmt.Sprintf("%s; %d embedded, %d failed, %d pending",
				es.Detail, es.Embedded, es.Failed, es.Pending)
			if es.Failed > 0 {
				return diag.Warning(detail+"; last error: "+es.LastError,
					"failed images are not retried past their attempt budget; a model-weight "+
						"download failure clears on its own once the network allows it, or set "+
						`{"image_embeddings":{"enabled":false}} in ~/.mnemo/config.json to stop trying`)
			}
			return diag.Healthy(detail)
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

// storeNotReadyResult reports boot progress when a check needs the
// default-user store but ForUser has not finished yet.
func storeNotReadyResult(check string) diag.CheckResult {
	st := boot.Get()
	switch st.Phase {
	case boot.PhaseFailed:
		return diag.Failure("store unavailable: "+st.Detail, "see startup.ready")
	case boot.PhaseReady:
		// Race: boot marked ready but registry map not visible yet.
		return diag.Warning("store not visible yet", "retry /health shortly")
	case boot.PhaseStarting:
		return diag.Healthy(check + ": store not started yet")
	default:
		return diag.Warning(boot.Summary(), "see startup.ready")
	}
}
