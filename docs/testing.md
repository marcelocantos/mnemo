# Testing mnemo

Reference for the project's three-tier test architecture (🎯T73).

## TL;DR

| Tier | Where it lives | When to write one | Run with |
|------|----------------|-------------------|----------|
| 1 — synthetic, in-tree | `*_test.go` files alongside the code they test | Most regressions. Pure unit logic, small fixtures. | `make test` |
| 2 — programmatic mid-scale | Tests that use `internal/storetest.Generator` to synthesise hundreds of sessions | When the bug only surfaces at row counts a tiny fixture won't reach (lock contention, query-plan flips, ingest scaling) | `make test` |
| 3 — real-data scale | Files gated by `//go:build scale`, live alongside Tier 1/2 tests | When the assertion only makes sense against a production-shape corpus (compactor convergence on actual token distributions, federation under real per-peer sizes) | `make test-scale` after `make snapshot` |

Drive the daemon through the MCP transport via the `internal/e2e`
harness whenever you're testing more than a single package's
internals. Direct Go-package API tests miss MCP serialisation bugs,
per-user routing bugs, and resolver wiring bugs.

## Tier 1 — synthetic, in-tree

The bread and butter. `internal/storetest` has primitives —
`WriteJSONL`, `MetaMsg`, `Msg`, `NewStore` — that build a small in-
memory transcript and a populated store. Tests assert exact values
because the inputs are fully specified.

```go
func TestX(t *testing.T) {
    projDir := t.TempDir()
    storetest.WriteJSONL(t, projDir, "-Users-alice-dev-app", "sess-01", []map[string]any{
        storetest.MetaMsg("user", "hello", "2026-05-10T10:00:00Z",
            "/Users/alice/dev/app", "main"),
        storetest.Msg("assistant", "world", "2026-05-10T10:00:01Z"),
    })
    s := storetest.NewStore(t, projDir)
    if err := s.IngestAll(); err != nil { t.Fatal(err) }
    // … exact-value assertions against s …
}
```

Use Tier 1 for: ingest correctness, FTS5 query shapes, single-table
invariants, fence/markdown parsing, schema-version checks.

## Tier 2 — programmatic mid-scale

`internal/storetest.Generator` writes a synthetic `~/.claude/projects/`
tree with controllable distributions of session count, messages per
session, token volume, repo mix, and tool-use density. Deterministic
given a seed — same seed, byte-identical output.

```go
gen := storetest.Generator{
    Seed:        42,
    Sessions:    500,
    Repos:       []string{"acme/foo", "acme/bar"},
    MsgsDist:    storetest.Distribution{Min: 5, Max: 200, Mean: 50},
    TokensDist:  storetest.Distribution{Min: 50, Max: 5000, Mean: 800},
    ToolUseRate: 0.3,
}
projDir := t.TempDir()
if err := gen.Write(projDir); err != nil { t.Fatal(err) }
// Pass projDir as MNEMO_HOME's .claude/projects to an e2e.Daemon,
// or ingest directly via storetest.NewStore.
```

Use Tier 2 for: lock contention with N writers, query-plan
correctness as row counts grow, ingest performance regressions,
migration backfill behaviour, compactor candidate selection at
non-trivial corpus sizes.

## Tier 3 — real-data scale

Tier 3 tests operate on an **isolated copy** of the real data —
never the live `~/.mnemo` or `~/.claude/projects`.

### Safety design

- **`MNEMO_HOME` controls every daemon-state path.** The daemon
  reads its database, config, state.json, peers/, and the
  default-user's `.claude/projects` tree from `$MNEMO_HOME`
  exclusively. Tests launched against a tempdir or snapshot path are
  structurally incapable of touching the real `$HOME`.
- **Build tag gate.** Tier 3 tests live behind `//go:build scale`.
  `go test ./...` never sees them. Only `go test -tags 'sqlite_fts5
  scale' ./...` (or `make test-scale`) compiles them.
- **Env-var gate.** Tier 3 tests skip with a clear message when
  `MNEMO_TEST_SNAPSHOT` is unset. CI never sets it. A casual
  `make test-scale` on a contributor's machine without a snapshot
  prints skips, not failures.
- **APFS clone for snapshots.** `cmd/mnemo-test-snapshot` uses
  `cp -c` to clone the source tree by reference on macOS —
  effectively free, O(1). Falls back to plain recursive copy on
  other platforms.
- **No git-tracked destination.** The snapshot helper refuses to
  write into a directory inside a git working tree. The conventional
  location (`~/.mnemo-test-snapshots/`) is also `.gitignore`d as
  belt-and-braces.

### Workflow

```sh
make snapshot                              # clones ~/.mnemo and ~/.claude/projects
# Output ends with: MNEMO_HOME=/Users/you/.mnemo-test-snapshots/snapshot-...
export MNEMO_TEST_SNAPSHOT=$(make snapshot 2>/dev/null | tail -n1 | cut -d= -f2)
make test-scale
```

### Writing Tier 3 tests

Tier 3 assertions are **invariants**, not exact values — the
snapshot changes every time you take one, so a test claiming
"exactly 500 compactions" flakes. Reach for shapes like:

- **Convergence**: "backlog ≤ N within K scans"
- **Cursor monotonicity**: "every compaction's `entry_id_from`
  equals its predecessor's `entry_id_to + 1`"
- **No-double-process**: "no session_id appears more than once in
  a single scan's candidate list"
- **Bounded resource use**: "MCP tool returns within X seconds at
  scale"

The current worked examples live in `internal/e2e/scale_test.go`.

## The e2e harness (driving the daemon via MCP)

`internal/e2e.Start(t, ...)` spawns `bin/mnemo` as a subprocess
under `MNEMO_HOME=<tempdir or snapshot>`, waits for the HTTP
transport to become ready, and returns an MCP client handle. Tests
drive scenarios via real tool calls.

```go
func TestVaultViaMCP(t *testing.T) {
    d := e2e.Start(t)  // tempdir-isolated daemon
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    out, err := d.Call(ctx, "mnemo_vault_status", nil)
    if err != nil { t.Fatal(err) }
    // … assertions on `out` …
}
```

Failure modes that ONLY surface end-to-end:

- MCP request → tool handler → resolver wiring → backend response
  serialisation.
- Per-user routing and the `?user=` query-param fallback.
- Hot-reload paths (`mnemo_config(op=write, ...)`) and their
  in-process adoption.
- The streamable-HTTP transport's session handling under real load.

The harness uses [`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go)'s
client for the transport layer, so test assertions read the same
JSON-RPC responses any real MCP client (Claude Code, etc.) sees.

## What `make bullseye` runs

The repo's "standing invariants" check (run on every PR / referenced
by the `bullseye_convergence` tool). It gates Tier 1 + Tier 2 only —
`make test`, not `make test-scale`. Tier 3 is local-only.

## File map

| Path | Purpose |
|------|---------|
| `internal/storetest/storetest.go` | Tier 1 primitives — JSONL fixtures, NewStore helper |
| `internal/storetest/generator.go` | Tier 2 deterministic generator (🎯T73) |
| `internal/e2e/e2e.go` | Subprocess + MCP client harness (🎯T73) |
| `internal/e2e/scale_test.go` | Tier 3 invariant examples (🎯T73) |
| `cmd/mnemo-test-snapshot/` | Snapshot helper (🎯T73) |
| `internal/store/homedir.go` | `EffectiveHome()` + `MNEMO_HOME` plumbing (🎯T73) |
