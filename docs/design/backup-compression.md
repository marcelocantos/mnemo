# Backup compression (🎯T159)

How mnemo's database snapshots are compressed, what the settings are, and
the measurements that chose them. A later change to level, worker count or
codec should argue against these numbers rather than replace them by
assertion.

## What a backup is

`backup.Backup` produces one file:

1. `VACUUM INTO` a sibling temp file — a fully consistent standalone
   database on the same filesystem, so the eventual rename is atomic.
2. `PRAGMA integrity_check` on that snapshot.
3. Compress it to `destPath.tmp` with the vendored multithreaded libzstd.
4. Verify the compressed artefact reads back.
5. Atomic rename to `mnemo-{tag}-{YYYYMMDDTHHMMSSZ}.db.zst`.

Retention keeps **one** snapshot (🎯T158), plus a second momentarily while
a new one is being written.

## Settings

| Setting | Value | Why |
|---|---|---|
| Codec | libzstd, vendored at `internal/zstdc` | Multithreading is the point; see below |
| Level | 3 (`zstdc.LevelFast`) | zstd's own default, at the knee of the curve |
| Workers | 0 → one per CPU | libzstd stitches parallel jobs into a single frame |
| Checksum | on (`ZSTD_c_checksumFlag`) | Turns silent corruption into a loud read failure |

Level 3 rather than something higher because the snapshot is replaced
tomorrow: higher levels buy a few percent of size for multiples of the
CPU, which is the wrong trade for a file with a one-day life.

## Measurements

End to end on the live index, 2026-08-31, M4 Max (16 cores), 18,157 MB
database, via `backup.BackupWith`:

```
VACUUM INTO + integrity_check + compress + verify   2m15s total
  compress                8.5s   2132 MB/s   ratio 0.548  (18157 → 9953 MB)
  verify (decompress)    11.2s   1631 MB/s   single-threaded
restore (Decompress)     12s
integrity_check on the restored database   ok (1m22s, 3,626,566 messages)
```

The baseline it replaced was **gzip level 1** — `backup.go` had already
chosen speed over size — measured at ~83 MB/s, i.e. roughly 3.8 minutes of
CPU burn on every snapshot, for ratio 0.734. The switch is about that
time, not about size; the size improvement came along for free.

Earlier, on a 1.5 GB slice, comparing the candidates:

```
gzip -1 (what backups used)   ratio 0.734    ~83 MB/s
klauspost, streaming          ratio 0.707    parallel ceiling ~1.4x
klauspost, hand-framed        ratio 0.732    full parallelism, multi-frame
libzstd -T0 (chosen)          ratio 0.689    full parallelism, one frame
```

The pure-Go encoder mnemo already carries (klauspost, which implements the
per-row compression of 🎯T151) pipelines its stages rather than splitting
the input into jobs: it plateaus around 1.4x and does not improve past two
workers, and `EncodeAll` ignores the concurrency setting entirely. Framing
chunks by hand in Go reaches full parallelism but emits a multi-frame
stream about 4% larger. libzstd splits into jobs, compresses them in
parallel and stitches them into a **single** frame, with `overlapLog`
preserving context across job boundaries — which is why `zstd -T0` and
`-T1` produce byte-identical output sizes.

Note the ceiling. 🎯T151/🎯T152 already compress the payload per row, so
generic compression of the file has little left to find: the backup ratio
degraded from 0.33 (28 Jul) to 0.62 (29 Aug) as row compression landed,
and the snapshot briefly *grew* while the database itself halved.

## Verification, and why it is not optional

`integrity_check` in step 2 proves the **database** was sound. Step 4
proves the **file** can be read back: it decompresses the whole artefact,
which validates the frame's embedded XXH64 over the original content, and
checks the byte count against the snapshot.

This matters because retention is one snapshot. The caller deletes the
previous backup once this one returns successfully, so an unreadable
output is not a degraded backup — it is no backup. Verification costs
11.2s against a 228s saving, and it runs on **every** snapshot rather than
once at the format transition, which is the stronger guarantee.

## Reading a backup

`backup.Decompress` expands both `.db.zst` and `.db.gz`, in pure Go —
`internal/zstdc` binds C for compression only, so no restore path needs
cgo, and no restore needs the `zstd` CLI installed. A disaster-recovery
artefact that requires a package manager first is a poor
disaster-recovery artefact; gzip was universally available and zstd is
not yet, so mnemo carries its own reader.

Both extensions are also recognised by `parseFilename`, which is what
retention lists and collects. That is load-bearing in both directions:
retention only manages files it recognises, so teaching it the new suffix
while dropping the old would have made every pre-existing snapshot
invisible to GC — the mechanism by which ~187 GB of unmanaged files once
accumulated beside a correctly functioning retention pool (🎯T158).
`TestRetentionSpansBothFormats` pins it, with the gzip file deliberately
newer than one of the zstd files so ordering has to span formats.

## Vendoring

`internal/zstdc` carries upstream zstd v1.5.7 as a single-file
amalgamation; `UPSTREAM.md` records the version and how to regenerate it,
`LICENSE.zstd` the BSD-3-Clause terms, and the repo `NOTICE` the
attribution.

zstd's multithreading is a **compile-time** option. A library built
without `ZSTD_MULTITHREAD` accepts `nbWorkers` and then silently runs
single-threaded — everything still works, everything is just slower, with
nothing on screen to say so. The bridge therefore reads the granted worker
count back out of the context and reports it, and two tests
(`TestMultithreadingIsReal`, `TestCompressionUsesAllWorkers`) fail rather
than let that regression pass quietly. Both are green on macOS/arm64 and
Windows/arm64.
