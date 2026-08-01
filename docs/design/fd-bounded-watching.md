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

## Steady-state bound

Watch-related open FDs must stay **well under 5k** for a full corpus (target
class: O(roots) on Darwin). Historical files are covered by the tree
subscription or the safety poll — never as individual permanent watches.

`MaxDirWatches` (4096) caps fsnotify dir expansion; exceeding it **fails soft**
(log + continue; safety poll covers the rest).

## Safety poll

All platforms re-stat only a **hot set**: live sessions, recently evented
paths, and previously incomplete (`size > offset`) paths — not the full cold
archive every tick. Recovers open/write/close writers (e.g. Grok
`updates.jsonl`).

## Path filters

Allowlisted: Claude/Codex session `*.jsonl`, Grok `updates.jsonl`, memory /
skills / CLAUDE.md / audit-log / targets / todos / `.planning` markdown.

Denied: Grok `terminal/`, non-ingested Grok sidecars (`events.jsonl`,
`chat_history.jsonl`, …).

## Regression oracle

`internal/store/fswatch.TestFDBoundOracle` starts the **shipped** `fswatch.New`
API over a synthetic multi-thousand-file tree and asserts process open-FD delta
is far below file count (not ≥ half the files) and under 5k.
