# Automation liveness plugin (exploration)

**Status:** exploration / contemplating — not committed. Captures an idea that
surfaced while fixing a silent stall in the `ytt` playlist-ingest automation
(June 2026). Owner: Marcelo.

## Motivating failure

The daily YouTube → knowledge-base ingest (`marcelocantos/ytt`,
`scripts/playlist-ingest`, run by a launchd job) **silently stopped producing
output for ~9 days**. A single run's transcript fetch hung on a half-open
socket (left after the laptop slept mid-run); because launchd starts no
concurrent instances, the wedged run suppressed every subsequent daily tick.
Nothing surfaced the stall — it was noticed only because a human eventually
went looking ("latest pull is June 20").

The acute bug was fixed in-script (per-call timeouts + a run-level watchdog,
`ytt` v0.7.0/v0.8.0). But those fixes are **bespoke and in-process** — each
automation must bake in its own liveness logic, and a watchdog inside a run
can't catch the run that never starts.

## The generalization

"An automation silently stopped making progress and nobody noticed" is a
**cross-cutting** concern, not a per-script one. The same risk applies to
yadm auto-sync, the `~/think` auto-sync, future cron jobs, and mnemo's own
background workers (compactor, vault sync). mnemo is well-positioned to own it:
it already aggregates cross-repo/session activity and answers "what happened,
where, when" — extending that to "...and has automation X gone quiet past its
expected cadence?" is a small conceptual step.

## Sketch

A liveness plugin would provide:

- **Registry** — each automation declares a liveness signal. Candidates:
  a heartbeat file, a log's mtime (e.g. `~/think/knowledge/youtube/.ingest.log`),
  a launchd job's `LastExitStatus` + PID, the newest artifact's timestamp, or
  the last commit on a repo.
- **Staleness policy** — a per-automation expected cadence (ytt: daily;
  yadm: 30 min) and a grace multiple. The plugin flags anything past threshold.
- **Surface** — fold health into `mnemo_status` (or a dedicated `mnemo_health`
  tool), and optionally a proactive Slack DM via the existing `RemoteTrigger`
  path so a stall pages a human instead of waiting to be discovered.

## Core design fork: poll vs push

- **Poll** — mnemo periodically inspects known signals (log mtimes, launchd
  state, git heads). Zero work for the automation; mnemo must be told where to
  look. **Would have caught *this* stall with no changes to ytt.**
- **Push** — automations emit heartbeats to mnemo (e.g. `mnemo_heartbeat
  <name>` on each successful cycle). Cleaner, more precise signal; but every
  automation must opt in, and an automation that dies *before* its first
  heartbeat of the day is invisible (the same "run that never starts" gap).

**Lean:** poll-first (it's the strictly more robust default for the failure we
actually hit), with an optional push heartbeat as a precision upgrade per
automation.

## Open questions

- Does this justify a general mnemo **plugin mechanism**, or is it a built-in
  capability? (The watchdog is the first concrete plugin candidate, but a
  plugin API is a larger commitment than a built-in.)
- Where does the poll loop run — inside the mnemo daemon, or a separate
  scheduled agent that calls mnemo?
- Registry storage: a config file, a mnemo table, or discovered from each
  repo's CLAUDE.md (which already declares delivery/cadence-ish metadata)?

See also: the in-script fixes in `marcelocantos/ytt` (per-call timeouts +
`YOUTUBE_INGEST_RUN_TIMEOUT` watchdog) as the bespoke version this would
generalize.
