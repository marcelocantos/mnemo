# Attended hours and agent hours (🎯T202)

`mnemo_hours` (and `mnemo hours`) answers "how many hours did I put into
this project, and how many did agents?" from data mnemo already holds,
without an agent reading transcripts. The owner runs four or five
threads at once and often leaves agents working while away from the
desk, so neither a per-session sum nor wall-clock between first and last
message is meaningful.

## Model

**Attended time.** Every human input from every thread goes on one
timeline, so five threads worked in parallel count once. Inputs no more
than `gap` apart (default 15 minutes) form a block. A block runs from
`lead` (2 minutes) before its first input to `tail` (5 minutes) after
its last. A lead never reaches back into the previous block.

Within a block, the stretch up to each input goes to that input's
project. Reading one thread's output and writing its next prompt is work
on that thread, so the time before a prompt belongs to the prompt. The
lead goes to the first input and the tail to the last, so a block's
attribution sums exactly to the block.

**Agent time.** A session is busy for each minute in which it produced
output (an assistant message or a tool result). Quiet stretches of up to
`agent_gap` (5 minutes) between busy minutes are bridged, except across
a human input in that session: an agent that finished its turn and then
waited for the owner was waiting, not working. After an input, the
session is busy from the input itself if its next output follows within
`agent_gap`.

A project's agent time is the union over its sessions, so parallel
agents on one project count once; the total is the union over all
projects. `agent_unattended_hours` is agent time while the owner was
attending nothing, anywhere.

mnemo's own background agents (sessions marked `compactor_internal`)
are left out.

## What counts as a human input

Who typed a message is the hard part. Inside a Claude transcript the
owner's prompts and T3 Code's injected messages (delegation briefs,
Slack inbox pushes, task notifications, messages relayed from other
threads) are indistinguishable: both are `promptSource: "sdk"`.

| Session | Source of truth | Counted |
|---|---|---|
| Driven by T3 Code | T3's store, `orchestration_v2_projection_messages` | `createdBy = user` **and** `creationSource` is `web` or `mobile`; not in a subagent thread |
| Claude, not T3 | Transcript provenance | see below |
| Any | AskUserQuestion results | every non-error answer |
| Codex, Grok, Cursor, omp outside T3 | none | agent hours only; counted in `agent_only_sessions` |

`createdBy` alone is not enough. T3 marks every message of an
**imported** transcript `createdBy = user`, task notifications included,
with `creationSource = server`. The 21 non-imported `server`/`user` rows
on the owner's store were agents' questions. The two T3 clients a person
types into are `web` and `mobile`.

For a Claude transcript entry, in order:

1. `isSidechain`, `isMeta` or `isCompactSummary`: not human.
2. `origin.kind` present and not `human`: not human (task notifications,
   peer messages, auto-continuations).
3. `promptSource = system`: not human.
4. Harness output (`<local-command-stdout>`, `<task-notification>`,
   `<bash-stdout>`, …): not human.
5. `entrypoint = t3-code`: stored as `t3-transcript`, never counted.
6. Any other `sdk*` entrypoint without `origin.kind = human`: a program.
7. An opener from an orchestrator that types into a terminal (Jevons,
   claudia; `machineDriverPrefixes` in `internal/store/hours.go`): not
   human. These carry `promptSource: typed` and `origin.kind: human`, so
   wording is the only signal. The list is narrow and applies only where
   no structural signal exists.
8. Otherwise human: a `prompt`, a `command` (slash command), or an
   `interrupt`.

A forked or resumed session copies history into a new transcript under
the same entry UUIDs. Inputs are keyed on the UUID, so the copy does not
count twice.

Codex's `session_meta` line, which names the originator, is not indexed,
and on the owner's machine most non-T3 Codex sessions were orchestrated
(16,561 "user" messages in October 2026). Counting them would be
counting machines.

## Materialisation

A worker (`RunHoursWorker`, every minute) keeps four tables current:

- `hours_inputs`: one row per human input, keyed `(source, ref)`; no
  message text.
- `hours_agent_minutes`: `(session_id, minute)`.
- `hours_t3_sessions`: sessions T3 drove, from its provider threads.
- `hours_progress`: a cursor per stream (message id; T3 rowid) and the
  T3 store's status.

The messages stream reads only metadata, plus a 160-character head and
the entry's JSON for user text. The T3 stream opens the store read-only
and reads authorship, timestamps and thread ids, never text.

A report reads only these tables and answers a week in milliseconds and
a year in about 0.3 seconds (`TestHoursReportIsFastOverAYear`).

## Disclosure

Every report carries:

- **The parameters it used.**
- **`inputs_by_source` and `inputs_by_kind`.**
- **The T3 store's status:** `ok`, `absent`, `unreadable` or
  `unexpected_shape`, with the cause. When the store cannot be read but
  transcripts show T3-driven prompts in range, `degraded` says how many
  went uncounted. A broken T3 store never reads as an owner who stopped
  typing.
- **`pending_messages`:** messages indexed but not yet materialised.
- **`agent_only_sessions`.**

The T3 store path is `hours.t3_store` in `~/.mnemo/config.json`. The
default is `~/.t3/userdata/statev2.sqlite`, under the daemon's home, so
`--home` points a sandbox at a copy.
