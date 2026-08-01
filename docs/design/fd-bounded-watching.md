# FD-bounded tree watching (🎯T142)

## Incident (2026-08-01)

macOS kernel panic: `initproc exited` / SIGBUS (namespace 2 subcode 0xa) after
the system vnode table hit `kern.maxvnodes` (263168/263168). The brew service
`homebrew.mxcl.mnemo` (v0.80.0, `mnemo.db` ~24–25GB) held ~72k open FDs
(~65k REG + ~7k DIR) under `~/.claude/projects/**` and `~/.grok/sessions/**`.
Stopping mnemo dropped system open files ~88k→~8.8k. Service left stopped as
emergency mitigation.

## Root cause

Not a classic unclosed-handle leak. `Store.Watch` recursively
`filepath.Walk`ed transcript roots and `fsnotify.Add`ed every directory. On
Darwin, fsnotify uses **kqueue**, which opens an FD for every watched path and,
on `Add(dir)`, opens **every file in that directory** (`watchDirectoryFiles`)
to emulate inotify child writes. Live corpus math matched the incident
(~4.5k dirs + ~61k files ≈ ~65k FDs). The vault watcher used the same pattern.

fsnotify has no FSEvents backend ([fsnotify#11](https://github.com/fsnotify/fsnotify/issues/11)).

## Platform backends (shipped)

| GOOS | Backend | Cost model |
|------|---------|------------|
| **darwin** | **FSEvents** via `github.com/fsnotify/fsevents` on configured roots | O(roots) streams |
| **linux** | **inotify** via fsnotify directory watches (recursive, capped) | O(min(dirs, MaxDirWatches)) |
| **windows** | **ReadDirectoryChangesW** via fsnotify (recursive, capped) | O(min(dirs, MaxDirWatches)) |

Shared package: `internal/store/fswatch`. Production call sites:
`Store.Watch`, vault watcher in `internal/registry`.

### Linux: `fs.inotify.max_user_watches`

Each watched directory consumes one inotify watch. The kernel limits total
watches per user via `fs.inotify.max_user_watches` (often 8192–65536; check
`sysctl fs.inotify.max_user_watches` or
`/proc/sys/fs/inotify/max_user_watches`).

mnemo also enforces `fswatch.MaxDirWatches` (4096) in-process: when the cap is
hit, further `Add`s stop (**fail soft**), a warning is logged, and the safety
poll continues to cover known transcript paths. If the **kernel** limit is
lower than the tree size, `watcher.Add` fails per directory (also logged);
raise the sysctl only if you intentionally need more simultaneous dir watches:

```bash
# temporary
sudo sysctl -w fs.inotify.max_user_watches=524288
# persistent (distro-dependent)
echo fs.inotify.max_user_watches=524288 | sudo tee /etc/sysctl.d/99-mnemo-inotify.conf
```

Prefer fewer roots / filters over unbounded watch counts; the product bound is
still “well under 5k” open FDs / watches for mnemo’s own process.

## Steady-state bound

Watch-related open FDs must stay **well under 5k** for a full corpus (target
class: O(roots) on Darwin). Historical files are covered by the tree
subscription or the safety poll — never as individual permanent watches.

**Watch roots** (production): Claude/Codex/Grok trees + skills + configured
`workspace_roots` (default `~/work`). Individual known git checkouts are **not**
each registered as separate FSEvents paths — that previously produced hundreds of
roots and thousands of DIR FDs. Nested repo files under a workspace root still
receive events via the tree stream; repos outside workspace roots still get
boot-time `Ingest*` catch-up.

Live measurement on the author's full corpus (2026-08-01, ~25GB `mnemo.db`,
~60k session files): **backend=fsevents, roots=6, process open FDs ≈ 80**,
only ~3 FDs under transcript corpus paths (was ~72k with kqueue Walk+Add).

`MaxDirWatches` (4096) caps fsnotify dir expansion; exceeding it **fails soft**
(log + continue; safety poll covers the rest).

## Safety poll

Each tick states at most `DefaultPollMaxPerTick` (500) paths, in order:

1. **Live** sessions (existing liveness / open-jsonl signals)
2. **Recent** tree-event paths (`NoteEvent`)
3. **Incomplete** (`size > offset` observed earlier)
4. **Round-robin sample** of remaining keys in `ingest_state` / offsets
   (jsonl preferred)

So a silent open/write/close append with **no** FSEvents/inotify delivery is
still recovered within `ceil(N / maxPerTick)` ticks for N known paths — without
walking the filesystem tree or statting every cold file every interval.
Priority hot paths are always checked first.

## Path filters

Allowlisted: Claude/Codex session `*.jsonl`, Grok `updates.jsonl`, memory /
skills / CLAUDE.md / audit-log / targets / todos / `.planning` markdown.

Denied: Grok `terminal/`, non-ingested Grok sidecars (`events.jsonl`,
`chat_history.jsonl`, …).

## Regression oracle

`internal/store/fswatch.TestFDBoundOracle` starts the **shipped** `fswatch.New`
API over a synthetic multi-thousand-file tree and asserts process open-FD delta
is far below file count (not ≥ half the files) and under 5k.

## Telemetry surfaces

| Surface | Field / check |
|---------|----------------|
| `mnemo_status` → `diagnostics.watch` | `WatchTelemetry` JSON (backend, roots, dir_watches, cap_hit, process_open_fds, events, poll_*) |
| `mnemo_stats` → `watch` | same snapshot |
| `mnemo_doctor` / `GET /health` | check **`watch.fds`** (fast): ok / warn (≥3k FDs or cap hit / not running) / fail (≥8k FDs) |
| daemon log | `watching for changes` includes `backend`, `open_fds`, `cap_hit` |

`Store.WatchTelemetrySnapshot()` is the single source; poll ticks refresh
`process_open_fds` via `fswatch.OpenFDCount()`.
