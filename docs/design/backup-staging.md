# Backups on a volume that comes and goes (🎯T200, 🎯T201)

The database (`~/.mnemo/mnemo.db`, ~47 GiB) stays on the internal disk.
Snapshots (~28 GB each, zstd) go to `backup.dir` on an external SSD that
is unplugged now and then and that macOS can withhold from a newly
signed binary until a removable-volume prompt is answered. Two things
must hold: the daemon never stalls on that volume, and its absence never
costs a backup.

## Bounded waits (🎯T200)

On 2026-11-13 v0.116.0's worker and then v0.117.0's store bring-up sat
in `open()` on `/Volumes/Storage/mnemo/backups` and never returned.
`internal/backup/answer.go` puts every file-system call on a backup
directory behind a bounded wait:

- The call runs on its own goroutine. After `AnswerTimeout` (5s for
  metadata; `DataTimeout`, 60s, for a 64 MiB chunk) the caller gets
  `ErrDestinationUnavailable` with a reason naming the call and the two
  likely causes (a pending permission decision, an unresponsive volume).
- While the abandoned call is still in the kernel, its directory is
  marked as not answering and further calls are refused immediately.
  Without this, a health check polled every few seconds would strand one
  OS thread per poll.
- `probe` opens the directory and reads one name, because `open()` is
  the call that was held; a `stat` can be answered from cached metadata.
- The startup orphan sweep runs on a goroutine of its own.

A hang in the middle of a long write — the VACUUM INTO or the
compressor, minutes into a snapshot — is not bounded: it stalls that
snapshot's goroutine (and its read transaction) but not the daemon,
tool calls or health. A pulled USB drive fails those writes with EIO in
practice; the hang seen on 2026-11-13 was at `open()`.

## Staging (🎯T201)

`backup.Place(dest, staging)` returns `dest` when it answers, otherwise
the staging directory (`backup.staging_dir`, default
`~/.mnemo/backups/staging`). Staging is off when `backup.dir` is the
default, which is already on the database's volume. A staged snapshot:

- is recorded with `staged: true` and `staged_because`, as a success —
  it counts for the nightly window and for `last_success`;
- replaces any older staged snapshot of the same tag
  (`backup.RetainPlaced`), so a long absence costs one snapshot per tag;
- passes the same free-space precheck; a refusal is a skip naming the
  cause, and with the destination also away it is `unavailable`, so the
  worker still owes the destination a snapshot.

`<staging>/.staging.json` records `waiting_since`: when staging last
went from empty to non-empty. A newer snapshot replacing an older one
does not reset it, because what it measures is how long the destination
has gone without the newest backup.

## Replication

`backup.Replicator` polls every minute. When the destination answers it
copies each staged snapshot, oldest first:

1. Write `<dest>/<name>.part` in 64 MiB chunks. After each chunk:
   `fsync`, then append the chunk's SHA-256 to `<staged>.progress.json`
   (atomic rename). The progress file never claims more than is durably
   on the destination.
2. On resume (another pass, a restart, a reboot): if the progress
   matches the staged file (name, size, mtime, chunk size, destination),
   read the last recorded chunk back from the `.part` and compare its
   hash. Match: truncate there and continue. Mismatch: start again.
3. At the end, read the whole `.part` back and compare every chunk's
   hash and the size with the staged file's. Then `fsync`, rename to the
   final name, `fsync` the directory, and only then delete the staged
   copy and its progress record.
4. Apply retention on the destination. Replicated snapshots keep their
   original filenames, so they are ordered by when they were taken.

A staged snapshot that retention would discard on arrival (the
destination already holds `keep` newer snapshots of its pool) is
deleted without copying. A `.part` whose staged source has gone is
removed. `.part` files are reported separately by `backup.Usage`, not
as strays.

## States reported

`backup_status` and `backup.recent`:

| State | Health |
|---|---|
| Nothing staged; snapshots go to the destination | as before |
| Staged, waiting for the destination | healthy, with the staged snapshot and replication progress |
| Staged for longer than `StagedMaxAge` (7 days) | warns: the newest backup is still on the same disk as the database |

## Why the replicator is not a separate helper

macOS ties a removable-volume grant to the binary's code identity, and
an ad-hoc signed release (v0.117.0 was one) is a new identity on every
upgrade. A helper with a stable identity would keep its grant across
upgrades, but:

- It would be built and signed by the same release process, and Go
  embeds build and version information, so an ad-hoc helper's identity
  would change every release too unless it were frozen and built
  reproducibly across toolchain upgrades.
- It would cover only replication. The daemon writes snapshots straight
  to `backup.dir` when it answers, so it needs the grant anyway.

The fix for the grant is a stable signing identity for mnemo (🎯T101).
v0.118.0 was the first release signed with a Developer ID identity
(`scripts/release.sh` uses one whenever it is in the keychain), and it
read and wrote `/Volumes/Storage` straight after the upgrade with no
prompt. On a release host without the identity, a missing grant after
an upgrade costs a prompt, and the snapshots taken before it is answered
are staged, not lost.

## Tests

- `internal/backup/hanging_destination_test.go`: a stuck call is bounded
  and later calls are refused at once; sweep, `Away`, `List`, `Usage`,
  `EnsureDir` and worker housekeeping return against a hanging
  destination; a snapshot is skipped (no staging) or staged.
- `internal/registry/backup_hang_darwin_test.go`
  (`TestStartupSurvivesHangingBackupDir`): libc `open()` interposed with
  `DYLD_INSERT_LIBRARIES` so the directory really blocks; store bring-up,
  `backup_status`, `backup_now` (skip recorded) and `backup.recent` all
  answer. Fails against v0.117.0.
- `internal/backup/staging_test.go`: direct write, staged, one per tag,
  space precheck, replication when the destination returns, resume after
  a detach, restart on a mismatched partial, superseded snapshots,
  retention by original timestamp, the 7-day threshold, the nightly
  worker staging.
