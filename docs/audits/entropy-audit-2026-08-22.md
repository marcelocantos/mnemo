# Entropy audit — mnemo (2026-08-22)

Full-mode audit (entropy + explicit hygiene validation). Production
code was not modified. Hygiene was not initialized.

## Executive summary

- **Snapshot:** `/Users/marcelo/work/github.com/marcelocantos/mnemo`
  - Branch: `master` (tracks `origin/master`)
  - HEAD: `e189aa574b19415d2f3b509fbc23265a8575c46b`
    (`Reliability: query deadlines, reconciler isolation, budget/throttle surfaces (#190)`)
  - Working tree: **clean** (`git status --porcelain=v1 -b` showed
    `## master...origin/master` and no other lines)
  - Date: 2026-08-22
  - Version in `main.go`: `0.86.0`
- **Scope:** Go daemon (`main.go` + `internal/`), MCP tool surface,
  HTTP/dashboard/REST, SQLite schema, CI/release, docs that declare
  architecture. Adjacent surfaces named but not line-audited: Swift
  menubar (`shim/`), ObjC Vision OCR (`ocr_darwin.m`), Windows
  installer ISS, Python CLIP embedder.
- **Exclusions:** `bin/` binaries; `shim/dist/Mnemo.app`;
  `internal/store/testdata/images/` fixtures; sibling
  `../sqldeep` (not in this repo). No vendored tree in-repo.
- **Headline mechanism:** The MCP *tool* surface was ratcheted to 18
  names, but almost every corpus still lives behind one 70-method
  `store.Backend` and an 8 180-line `store.go`. Meanwhile the CGO
  sqldeep dependency — required for `Store.Status` — is pinned in four
  disagreeing places, so the PR / Windows-VM merge gates do not test
  the sqldeep that release builds.
- **Highest-consequence findings:** ENT-001 (Store/Backend hub),
  ENT-002 (sqldeep pin disagreement).
- **Unverified residue:** full `go test -tags sqlite_fts5 ./...` was
  not re-run here (last green on this commit is assumed from merge);
  whether sqldeep v0.22.0 actually breaks the named-param Status query
  was not reproduced; dashboard pixels were not rendered; goja plugin
  sandbox was not fuzzed.

P0: 0. P1: 2. P2: 6. P3: 4.

## Dimension vector

| Dimension | State | Evidence summary | Change from baseline |
|---|---|---|---|
| Architecture topology | concern | 29 internal packages, **0 import cycles**, but `internal/store` fan-in 14 and `store.go` 8 180 lines / 70-method `Backend` | no prior entropy report in `docs/audits/` |
| Redundancy / sources of truth | concern | sqldeep pinned four ways; README “27 tools” vs ratchet 18; todo indexer config/schema remain after tools+ingest were removed | — |
| Change amplification | concern | 6-month churn: `main.go` 120, `store.go` 108, `tools.go` 86, `iface.go` 47; fakeBackend is 75 methods | — |
| Local code quality | concern | 7 committed files fail `gofmt -l`; leftover `WhoRan` / template APIs; `extractMetaFromFile` still on `bufio.Scanner` | — |
| Correctness / verification | concern | Strong oracles for schema-additive, query-only, ingest oversize, tool-surface; e2e env-gated out of PR CI; dashboard is HTML greps; no macOS CI job | — |
| Security / dependencies | concern | Query authorizer + ATTACH deny are real (🎯T103/T106); no dependabot, no secret/vuln scanner in CI; opt-in egress posture is sound | — |
| Build / release / operations | concern | `go.mod replace` to sibling sqldeep; `ci.yml` and `win-validate.sh` still clone v0.22.0 while release/e2e use v0.23.0 | — |
| Documentation / governance | concern | README empty tool tables; CLAUDE.md omits two live tools; `hygiene.yaml` absent; no CODEOWNERS | — |

These states are not aggregated.

## Scope and exclusions

Audited: module `github.com/marcelocantos/mnemo`, Go 1.26, CGO SQLite
FTS5 daemon. Declared architecture from `CLAUDE.md`, `agents-guide.md`,
`STABILITY.md`, `docs/testing.md`, `docs/design/*`, and the July
Fable-5 audit (`docs/audit/fable-2026-07.md`).

Named exclusions (not silent omissions):

| Path | Role |
|---|---|
| `bin/` | built binaries |
| `shim/dist/Mnemo.app/` | packaged macOS app |
| `internal/store/testdata/images/` | OCR/embed fixtures |
| `../sqldeep` | sibling CGO library consumed via `replace` |
| Swift sources under `shim/Sources/` | native head; topology only |
| `ocr_darwin.m` / `.h` | Vision OCR; topology only |
| `tools/embed-clip/embed.py` | opt-in CLIP path |

## Commands run

All from repo root. Auxiliary unless noted.

| Command | Version / notes | Exit | Relevant output | Path |
|---|---|---|---|---|
| `git rev-parse --abbrev-ref HEAD`; `git rev-parse HEAD`; `git status --porcelain=v1 -b`; `git log -1 --oneline` | git | 0 | `master`; `e189aa574b19415d2f3b509fbc23265a8575c46b`; clean tracking `origin/master` | provenance |
| `go version` | go 1.26.4 darwin/arm64 | 0 | — | aux |
| `find . -name '*.go' \| wc -l` and per-package / per-file loc | — | 0 | 364 Go files, 98 372 lines; `store.go` 8 180; `store_test.go` 2 754; `tools.go` 1 963; `config.go` 1 856; `main.go` 1 804 | aux |
| Python Tarjan SCC over internal imports | — | 0 | 29 internal packages; **0 cycles**; store fan-in 14; main fan-out 15; registry fan-out 14 | aux |
| `git log --pretty=format: --name-only --since='6 months ago'` counts | — | 0 | `main.go` 120, `store.go` 108, `tools.go` 86, `iface.go` 47, `registry.go` 33 | aux |
| `gofmt -l .` | gofmt from go 1.26.4 | 0 (list non-empty) | 7 files (see ENT-006). Makefile `fmt-check` would fail (`test -z`) | aux; would fail shipped `make fmt-check` |
| `GOWORK=off go vet -tags sqlite_fts5 ./...` | go vet | 0 | clean. Bare `go vet` first failed: `directory prefix . does not contain modules listed in go.work` even though **no `go.work` in this repo** — environment `GOWORK` pointing at a parent workspace | aux |
| `~/.claude/skills/hygiene/hygiene_check.py` | hygiene skill validator | 1 | `FileNotFoundError: .../mnemo/hygiene.yaml` | hygiene |
| Schema / interface counts | rg | 0 | `CREATE TABLE` 52, FTS virtual tables 23, `schema.sql` 1 575 lines, `Backend` methods 70, `fakeBackend` methods 75 | aux |
| Full `go test -tags sqlite_fts5 ./...` | not run | — | residue (Windows store package is the known long pole) | shipped path **not re-executed** |

Limitations: clone detection (jscpd) and `govulncheck` were not
installed. Metrics locate evidence; they are not verdicts.

## Observed architecture

Single-process HTTP MCP daemon. `main.go` dispatches subcommands then
`runServe`: `registry.Registry` (one `store.Store` per `?user=`),
streamable-HTTP MCP at `/mcp`, REST+embedded dashboard on the same mux,
optional federated mTLS listener (`:19420`), optional edge proxy,
upgrade lease, plugin manager, diagnostics scheduler.

```
clients (Claude/Grok/Codex MCP, browser, Mnemo.app)
        |  HTTP :19419  /  mTLS :19420
        v
 main.go  -->  tools.Handler  -->  store.Backend
          -->  api.Handler    -->  store.Backend
          -->  registry       -->  store.Store (+ workers)
                                  compact / streamseg / reviewer
                                  vault.Exporter
                                  backup / throttle / plugin
                                  fswatch
```

**Declared and observed that agree**

- One SQLite schema file applied by sqlift `AllowNone`
  (`store.go:913`, `applySchema` comment at 939–940).
- Tool surface is a ledger (`internal/tools/surface.go`) with a
  bidirectional ratchet (`surface_test.go:35–77`, baseline 18).
- Read path for `mnemo_query`: sqldeep transpile + `PRAGMA query_only=1`
  + per-connection authorizer denying ATTACH and `query_only` reset
  (`rodriver.go:13–65`, `store.go:754–762`, `5964–5996`).
- Ingest of Claude/Codex/Grok JSONL shares `parsedFile` /
  `writeParsedFile`; parsers selected by path (`store.go:3308–3314`).
- Opt-in egress (pricing, CLIP, Admin API, vault embeddings/LLM,
  federation) matches `CLAUDE.md`.
- Federated endpoint is a closed allowlist (`federated.go:14–34`).
- No internal package import cycle.

**Observed, inferred from code**

- `store.Backend` is the integration seam for MCP, REST, vault, and
  tests — not a small query port. Vault still calls the per-corpus
  `Search*` methods (`vault.go:358` and siblings) even after those MCP
  tools were deleted.
- Three topic-span producers share `topic_segments` with an explicit
  rank `llm > compaction > structural` (`docs/design/streaming-segmentation.md:12–27`).
  Streaming segmentation is config-gated off by default
  (`config.go:90–95`). `DefaultSegmentExpand` remains `"none"`
  (`segments.go:23–24`).
- `internal/todo` is still live for **thread views** (parse
  `todo.md` on disk — `threads/view.go:25–29`), not for the removed
  MCP todo tools.
- Registry exists as its own package specifically to break
  store→compact→store (`registry.go:11–14`).

**Contradictions**

- README headline “27 tools” (`README.md:9`) vs ratchet 18 vs
  STABILITY.md “70 → 18”.
- `go.mod` requires sqldeep v0.23.0 and `replace`s to a sibling
  checkout; `ci.yml` and `win-validate.sh` still pin **v0.22.0**.
- CLAUDE.md MCP list omits `mnemo_session_structure` and
  `mnemo_locate_uuid`, which the ledger registers.
- `TodoGlobs` is still loaded into the store (`registry.go:408`) but
  `IngestTodos` no longer exists.

**Unknown intent**

- Whether `Backend` should split into query / vault-export / control
  ports, or stay one type until 1.0.
- Whether the `go.mod replace` to `../sqldeep` is a standing local-dev
  rule or leftover from T94.

## Findings

### ENT-001: `store.Backend` / `store.go` remain the change hub after the tool-surface ratchet

- **Priority:** P1
- **Dimensions:** Architecture topology; Change amplification; Local code quality
- **Status:** observed fact
- **Evidence:**
  - `internal/store/store.go` is 8 180 lines (longest file; next
    production files are `tools.go` 1 963, `config.go` 1 856, `main.go`
    1 804).
  - `internal/store/iface.go:12–93` is a **70-method** `Backend`.
  - Import graph: `internal/store` fan-in **14**; `main` imports 15
    internal packages; `registry` imports 14.
  - Six-month co-change: `store.go` 108 files-touched commits,
    `iface.go` 47, `tools.go` 86, `main.go` 120.
  - `internal/api/api_test.go:20–32` plus 75 `fakeBackend` methods
    (lines 33–128 shown for `WhoRan` / templates) must be edited
    whenever the interface grows, including panic stubs for methods
    the HTTP layer never calls.
  - T143 deleted MCP tools (`CLAUDE.md:86–94`, `surface.go:6–20`) but
    left `SearchMemories`, `GetMemory`, `DefineTemplate`,
    `EvaluateTemplate`, `ListTemplates`, `SearchImages*`, `ToolResult`,
    `Permissions` on `Backend` (`iface.go:30–40, 57–61`). Vault still
    needs some of those (`vault.go:358`); templates and `GetMemory`
    have **no non-test callers** outside `store.go` itself
    (`store.go:1444`, `7098–7155`).
- **Mechanism:** A new corpus, search shape, or control operation
  still means: schema.sql + store method + iface method + fakeBackend
  stub + often a tools.go handler. The tool *names* can no longer
  grow silently (ENT healthy structure), but the Go integration
  surface can, and does. The next plausible change — another ingest
  source or a vault-only query — repeats that shotgun.
- **Blast radius:** Every MCP handler, the dashboard REST layer, vault
  export, and every test fake. Compile-time: any iface addition
  breaks `fakeBackend` until a panic stub is added.
- **Counterevidence checked:** Package split already happened for
  compact, streamseg, vault, plugin, fswatch, backup; registry exists
  to avoid a cycle. Unified search (`corpora.go:1–17`) absorbed
  *tool* proliferation, not the Go interface. Some `Search*` methods
  are still the vault full-dump path (deliberate, not dead).
- **Smallest coherent remediation:** Split `Backend` along actual
  consumers (`QueryPort` for MCP/REST, `VaultCorpus` for full dumps,
  `Control` for notes/cluster/backup) so `fakeBackend` only stubs
  what the HTTP tests call. Move leftover template/`GetMemory`/`WhoRan`
  off the interface. Do not rewrite `store.go` in one sweep; peel
  write-side ingest files that already exist (`codex.go`, `grok.go`,
  `docs.go`) and stop adding methods to the 8k file.
- **Verification:** a compile test that `internal/api`’s fake
  implements a *narrow* interface whose method count is ratcheted;
  `TestToolSurfaceSize` already ratchets MCP names.
- **Ratchet candidate:** `len(Backend methods)` architecture test, or
  three small interfaces with a one-line composition in `*Store`.

### ENT-002: sqldeep is pinned in four places and the merge gates use the older one

- **Priority:** P1
- **Dimensions:** Redundancy / sources of truth; Build / release / operations; Correctness / verification
- **Status:** observed fact (disagreement); inference (that v0.22.0
  misbinds Status) — not reproduced in this audit
- **Evidence:**
  - `go.mod:50` requires `sqldeep v0.23.0`.
  - `go.mod:56` `replace … => ../sqldeep/go/sqldeep`. Local sibling
    described itself as `v0.23.0-1-gbd9257d` on `master` (one commit
    past the tag).
  - `.github/workflows/ci.yml:34–38` checks out sqldeep **`ref: v0.22.0`**.
  - `.github/workflows/release.yml:85–87` and
    `e2e-nightly.yml:29–31` check out **`v0.23.0`**.
  - `scripts/win-validate.sh:11–15` documents and defaults
    `WINCI_SQLDEEP_REF` to **`v0.22.0`**. That script is the
    windows-vm-validated pre-merge gate (`CLAUDE.md` Gates).
  - `Store.Status` is explicitly a sqldeep nested projection that
    needs v0.23.0 named-param behaviour (`store.go:5601–5611`).
  - Bullseye T94 already recorded the workflow-ref lag (“currently
    v0.22.0”) while claiming go.mod + release + e2e at v0.23.0 —
    `ci.yml` / win-validate were the ones not listed as bumped.
- **Mechanism:** CI and the required Windows VM gate compile mnemo
  against a different sqldeep than local `replace` builds and than
  GitHub Releases. A Status/sqldeep regression can be green on PR CI
  and red on release, or the reverse. The `replace` also means a
  checkout of *only* this repo cannot `go build` without a sibling
  tree at a floating git SHA.
- **Blast radius:** every test that opens a Store (almost the whole
  suite, via CGO); `mnemo_status`; release artifacts; Windows merge
  gate.
- **Counterevidence checked:** e2e-nightly and release *were* bumped.
  Named parameters may make Status safe even on 0.22.0 (not verified).
  The replace is the documented local-dev way to consume unpublished
  sqldeep fixes.
- **Smallest coherent remediation:** one pin. Recommend: drop
  `replace` from committed `go.mod` (or keep it only in a
  gitignored overlay); set `ci.yml`, `win-validate.sh` default, and
  release/e2e to the same tag as `go.mod`.
- **Verification:** a workflow grep / tiny test that every
  `sqldeep` `ref:` and `WINCI_SQLDEEP_REF` default equals
  `go.mod`’s require. A machine without `../sqldeep` must
  `go build -tags sqlite_fts5`.
- **Ratchet candidate:** CI step `test -z` comparing those strings;
  or generate workflow refs from `go.mod`.

### ENT-003: Published tool-count and tool lists disagree with the ledger

- **Priority:** P2
- **Dimensions:** Documentation / governance; Redundancy / sources of truth
- **Status:** observed fact
- **Evidence:**
  - Ratchet: `surface_test.go:71` `const baseline = 18`;
    `surface.go:44–89` lists 18 names.
  - `STABILITY.md:15–16` “70 tools to 18”.
  - `README.md:9` “deliberately small set of MCP tools — **27**”.
  - `README.md:347–384` three MCP tool tables are **empty**
    (Cross-project knowledge, Cross-session messaging, External
    source indexing) after T143 removed the rows and not the
    headings.
  - `CLAUDE.md:59–84` documents 16 tools and omits
    `mnemo_session_structure` and `mnemo_locate_uuid`, both in the
    ledger (`surface.go:53–54`) and README’s remaining table
    (`README.md:343–344`).
- **Mechanism:** Agents and humans read README/CLAUDE.md as the
  product surface. The Go ratchet cannot see markdown, so the
  headline number and the empty tables will drift again the next
  time a tool is folded.
- **Blast radius:** agent setup (`agents-guide.md` is closer to
  correct), owner-facing README, in-repo CLAUDE.md.
- **Counterevidence checked:** `agents-guide.md` still has sections
  for the two omitted tools. STABILITY.md matches the ratchet.
- **Smallest coherent remediation:** set README to 18; fill or delete
  empty tables; add the two missing bullets to CLAUDE.md.
- **Verification:** a test or hygiene `command:` that counts
  `mnemo_` names in README’s tables against `len(toolConsumers)`.
- **Ratchet candidate:** hygiene item `docs.mcp-surface` with a
  small `command:` comparing those sets.

### ENT-004: Todo indexer was removed at the tool and ingest layers but not at config, watch, or schema

- **Priority:** P2
- **Dimensions:** Redundancy / sources of truth; Change amplification
- **Status:** observed fact
- **Evidence:**
  - Tools gone: `CLAUDE.md:86–94` (`mnemo_todos` / `_add` / `_set`);
    `surface_test.go:169` still names them as *forbidden* re-adds.
  - `IngestTodos` has no implementation; only comments and
    `SetTodoGlobs` remain (`store.go:150–154`, `346–357`).
  - Registry still applies config: `registry.go:408`
    `s.SetTodoGlobs(cfg.TodoGlobs)` and reload at 1265.
  - Config still documents the indexer (`config.go:97–101`).
  - Watcher still treats `TODO.md` / `todos.md` as indexable
    (`fswatch/filter.go:66`).
  - Schema still has `todo_files` / `todos` / `todos_fts` and
    triggers (`schema.sql:704–713` and later FTS/triggers). Append-only
    policy forbids dropping them.
  - `internal/todo` **is** still used by thread views
    (`threads/view.go:25–29`) — disk parse, not the SQLite index.
- **Mechanism:** A user setting `todo_globs` believes mnemo indexes
  those files into FTS. Nothing writes the tables. Watch events for
  TODO.md have no ingest handler. Two todo authorities (SQLite
  tables vs live parse in threads) can already disagree, and only
  one is updated.
- **Blast radius:** config UX; any remaining rows in `todos` (stale
  forever); thread UI is unaffected (it does not read the table).
- **Counterevidence checked:** schema policy *requires* leaving the
  tables (phase-1/2/3 GC, `CLAUDE.md` Schema policy). Thread parser
  is a separate, valid use of `internal/todo`.
- **Smallest coherent remediation:** stop advertising `todo_globs`;
  stop calling `SetTodoGlobs`; drop TODO.md from `fswatch` filter
  unless a new writer is restored. Leave tables in place until a
  product GC (phase 3).
- **Verification:** grep ratchet that `SetTodoGlobs` / `todo_globs`
  have no production callers, or a doctor check “todo index is
  retired”.
- **Ratchet candidate:** `rg` in CI that `IngestTodos` is not
  reintroduced without restoring tools, or the inverse.

### ENT-005: `mnemo_search` still has two backends

- **Priority:** P2
- **Dimensions:** Redundancy / sources of truth; Change amplification
- **Status:** observed fact
- **Evidence:**
  - Default path: `tools.go:576–591` → `unifiedSearch` →
    `UnifiedSearchOpts` (`search_unified.go:21–45`).
  - Non-default `expand` (not `"none"`) still calls `h.mem.Search`
    then `AttachSegmentExpand` (`tools.go:594–599`).
  - `DefaultSegmentExpand` is `"none"` until a quality bar
    (`segments.go:23–24`; design doc `streaming-segmentation.md:23–27`).
  - `Search` remains on `Backend` (`iface.go:13`) and is widely used
    in tests and vault annotation tests.
- **Mechanism:** A ranking/context/repo-filter bug fix on unified
  search does not automatically apply to the expand path, and the
  reverse. Expand is off by default so production agents mostly hit
  one path — until someone passes `expand=segment` and silently
  leaves the calibrated multi-corpus world.
- **Blast radius:** the tool that is 55% of agent calls
  (`surface.go:45–47`); any future default-on expand.
- **Counterevidence checked:** the split is documented as temporary
  until segment expansion has its own output shape (`tools.go:586–587`).
  Message-only `Search` is still the right primitive for expand.
- **Smallest coherent remediation:** implement expand as a post-pass
  on unified message hits (the `Message *SearchResult` field already
  exists — `search_unified.go:46–50`) and stop calling `Search` from
  the tool handler.
- **Verification:** a test that `expand=segment` still returns
  calibrated cross-corpus hits plus a span, and fails if the handler
  calls `Search` again.
- **Ratchet candidate:** forbid `callHandler.search` → `mem.Search`.

### ENT-006: `gofmt` is declared locally and already failing on `master`

- **Priority:** P2
- **Dimensions:** Local code quality; Build / release / operations
- **Status:** observed fact
- **Evidence:**
  - `Makefile:45–46` `fmt-check` fails if `gofmt -l` is non-empty.
  - `gofmt -l .` on this snapshot listed:
    `budget_cli_test.go`, `internal/api/api.go`,
    `internal/compact/claudia.go`,
    `internal/registry/breaker_stream.go`,
    `internal/registry/registry.go`,
    `internal/store/reconciler_drive.go`,
    `internal/tools/consolidated_tools.go`.
  - `.github/workflows/ci.yml` Test job runs only
    `go test -tags sqlite_fts5` (`ci.yml:108–115`). No `gofmt`, no
    `go vet`.
- **Mechanism:** format drift is invisible until someone runs
  `make fmt-check` / `make bullseye`. CI will not catch it. Seven
  files on master already would fail that local gate.
- **Blast radius:** review noise; `make bullseye` (`Makefile:8–15`)
  is unusable on a clean master without first reformatting unrelated
  files.
- **Counterevidence checked:** `gofmt -l` exit code is 0 even with
  output (by design); the Makefile’s `test -z` is the real gate.
  `go vet` is clean under `GOWORK=off`.
- **Smallest coherent remediation:** run `gofmt -w` on those seven
  (separate from this audit) and add `gofmt -l` + `go vet` steps to
  `ci.yml`.
- **Verification:** `make fmt-check` empty; CI job fails if not.
- **Ratchet candidate:** CI step matching `Makefile` `fmt-check` /
  `vet`.

### ENT-007: Merge-gate matrix does not include the primary runtime (macOS) and e2e is opt-in

- **Priority:** P2
- **Dimensions:** Correctness / verification; Build / release / operations
- **Status:** observed fact
- **Evidence:**
  - `ci.yml:9–25` matrix: `ubuntu-latest`, `windows-latest` only.
    Cloud Windows is documented as non-required (`CLAUDE.md` Gates);
    the required Windows path is `scripts/win-validate.sh` (ENT-002).
  - No macOS GitHub Actions test job. Production macOS-only code:
    `internal/store/fswatch/watch_darwin.go`, `ocr_darwin.go` / `.m`,
    `internal/iterm`, `shim/` menubar.
  - `internal/e2e/main_test.go:11–21` exits 0 unless `MNEMO_E2E=1`.
    Comment says ci.yml and release.yml therefore never run the
    daemon-subprocess suite. Nightly does (`e2e-nightly.yml:1–7,62–69`).
  - Dashboard is owner-visible (`README.md:10–12`, `main.go:79–80`
    embed) and 1 291 lines of HTML. Layout oracle is a substring
    grep (`api/budget_test.go:87–105`), which
    `~/.claude/web-development.md` forbids as the sole layout gate.
- **Mechanism:** FSEvents watch caps, Vision OCR, and the menubar
  head can regress without a red PR. MCP serialisation / per-user
  routing bugs the e2e harness was built to catch (`e2e.go:4–12`)
  only fail on a non-blocking nightly. Dashboard geometry can be
  wrong with a green grep.
- **Blast radius:** every macOS Homebrew user (the documented
  default install in `agents-guide.md`); dashboard users; MCP
  transport bugs.
- **Counterevidence checked:** darwin-tagged unit tests exist
  (`fswatch/structural_darwin_test.go`, `iterm_test.go`) but they
  do not run on ubuntu/windows CI. Nightly e2e is an honest
  non-blocking net. `docs/testing.md` Tier 3 is correctly isolated
  behind `scale` + snapshot env.
- **Smallest coherent remediation:** add `macos-latest` to the CI
  test matrix (native, cheap relative to Windows CGO). Keep e2e
  nightly, but add **one** thin daemon-MCP journey to PR CI (status
  + search round-trip) or name an explicit exception. Dashboard:
  Playwright bounding-box on one fixture **or** an explicit
  journey exception.
- **Verification:** CI matrix includes darwin; a PR that breaks
  `/mcp` initialize fails in PR CI.
- **Ratchet candidate:** hygiene `ci_job` for macos + a
  `make test-e2e-smoke` job.

### ENT-008: Query-template and `WhoRan` APIs survived the tool deletion

- **Priority:** P3
- **Dimensions:** Local code quality; Change amplification
- **Status:** observed fact
- **Evidence:**
  - `DefineTemplate` / `EvaluateTemplate` / `ListTemplates` still on
    `Backend` (`iface.go:38–40`) and implemented (`store.go:7098–7155`).
    No non-test callers.
  - `WhoRan` is implemented (`store.go:2536–2545`) and stubbed on
    `fakeBackend` (`api_test.go:110–111`) but **is not on `Backend`**.
  - `GetMemory` is on `Backend` (`iface.go:31`) with no non-test
    caller besides the method itself (`store.go:1444`). Vault uses
    `SearchMemories`, not `GetMemory`.
- **Mechanism:** Dead methods still force fake updates and suggest
  that `mnemo_define` / `mnemo_who_ran` still exist (they do not;
  STABILITY.md lists them as removed).
- **Blast radius:** test fakes; grepping developers.
- **Counterevidence checked:** schema `query_templates` cannot be
  dropped (append-only). Leaving *functions* is not required by
  that policy.
- **Smallest coherent remediation:** unexport or delete the Go
  methods; keep tables. Remove `WhoRan` from `fakeBackend`.
- **Verification:** `rg WhoRan` / `DefineTemplate` confined to
  `store.go` historical comments or gone.
- **Ratchet candidate:** deadcode / `staticcheck` U1000 on
  `store.WhoRan`.

### ENT-009: `extractMetaFromFile` still uses the 1 MiB `bufio.Scanner` that T104 removed from ingest

- **Priority:** P3
- **Dimensions:** Local code quality; Correctness / verification
- **Status:** observed fact
- **Evidence:**
  - Ingest path uses uncapped `ReadBytes` (`store.go:3729–3751`;
    same comment in `codex.go` / `grok.go`); regression
    `audit_fable5_test.go:21–57`.
  - Session-meta backfill still does `bufio.NewScanner` with
    `Buffer(..., 1<<20)` and **does not check `scanner.Err()`**
    (`store.go:6132–6135`).
- **Mechanism:** a session whose first meta-bearing line is >1 MiB
  (inline image in the first user message) yields empty cwd/repo
  for that session. Offset of ingest is not affected; this is
  classification, not data loss. Same class of bug T104 closed on
  the write path.
- **Blast radius:** `session_meta.repo` / topic used by filters,
  vault paths, and `mnemo_recent_activity`.
- **Counterevidence checked:** not on the ingest offset path; loop
  returns when cwd+branch+topic filled (`store.go:6160–6162`).
- **Smallest coherent remediation:** reuse the `ReadBytes` helper
  already used by `parseFile`.
- **Verification:** extend T104-style fixture through
  `extractMetaFromFile` / backfill.
- **Ratchet candidate:** forbid `bufio.NewScanner` under
  `internal/store/` except tests.

### ENT-010: No declared dependency or secret scanning in CI

- **Priority:** P3
- **Dimensions:** Security / dependencies
- **Status:** observed fact
- **Evidence:** no `dependabot.yml`, no gitleaks/govulncheck/CodeQL
  job under `.github/workflows/`. `go.mod` pulls AWS SDK (indirect,
  via claudia), goja, websocket, sqlite3 CGO.
- **Mechanism:** CVEs and accidental secrets will not fail the merge
  gate. This is a hygiene gap more than a demonstrated defect.
- **Blast radius:** supply chain; `.mnemo` does not belong in git
  (`.gitignore` covers `*.db` only).
- **Counterevidence checked:** query authorizer, opt-in egress,
  localhost-only dashboard with no CORS (`api.go:66–69`) are real
  product-level controls. LICENSE is Apache-2.0.
- **Smallest coherent remediation:** `govulncheck` + dependabot for
  gomods; optional gitleaks. Do not treat absence as a vuln.
- **Verification:** CI job exists and fails on a planted
  `gitleaks` / high govulncheck hit.
- **Ratchet candidate:** hygiene `security.*` items once
  `hygiene.yaml` exists.

### ENT-011: Plugin in-process host evaluates operator JS in-process

- **Priority:** P3
- **Dimensions:** Security / dependencies
- **Status:** inference
- **Evidence:** `internal/plugin/inprocess.go:23–59` loads a
  `.js` file into `goja.New()` and serves it on 127.0.0.1. Plugins
  are config-opt-in (`CLAUDE.md` mnemo_config / 🎯T102.2).
- **Mechanism:** a malicious or confused plugin script runs in the
  daemon process. goja is not an OS sandbox. Blast is limited by
  “operator-configured, localhost” but is still process-equivalent
  to the daemon’s filesystem and SQLite handles if the VM is given
  host functions.
- **Blast radius:** the daemon process, the transcript DB.
- **Counterevidence checked:** launch/connect transports exist;
  in-process is documented (`plugin-system.md`); enablement is
  explicit in config. Not reviewed instruction-by-instruction.
- **Smallest coherent remediation:** keep the JS host on the
  documented contract; do not add filesystem/exec host functions
  without a second process. Treat this as accepted risk unless
  plugins grow.
- **Verification:** a test that the VM cannot `os.Open` the DB
  path unless a host func is added.
- **Ratchet candidate:** architecture test that goja `Set`
  host funcs stay in an allowlist.

### ENT-012: `make bullseye` cleanliness gate is not what CI runs

- **Priority:** P3
- **Dimensions:** Build / release / operations; Documentation / governance
- **Status:** observed fact
- **Evidence:** `Makefile:8–15` `bullseye` target = fmt + vet +
  build + test + **clean tree**. CI is test-only (`ci.yml:108–115`).
  Combined with ENT-006, `make bullseye` fails on current master
  before tests run.
- **Mechanism:** two “what green means” definitions. Agents that
  follow the Makefile local gate and agents that follow CI will
  disagree.
- **Blast radius:** contributor workflow; `make bullseye` name
  collides with the bullseye intent ledger (`bullseye.yaml`).
- **Counterevidence checked:** the target is clearly a local
  pre-commit bundle, not claimed in ci.yml.
- **Smallest coherent remediation:** either add fmt+vet to CI or
  rename the Makefile target so it is not mistaken for the
  product’s bullseye MCP.
- **Verification:** CI matches the local bundle you intend to keep.
- **Ratchet candidate:** hygiene `make_target` vs `ci_job` pair.

## Redundancy and competing-source-of-truth inventory

| Fact | Authorities | Drift observed? |
|---|---|---|
| sqldeep version | `go.mod` require, `go.mod replace` sibling SHA, `ci.yml` ref, `release.yml` ref, `e2e-nightly.yml` ref, `win-validate.sh` default | **yes** (ENT-002) |
| MCP tool set | `surface.go` ledger, `Definitions()` in `tools.go`, README, CLAUDE.md, STABILITY.md, agents-guide.md | **yes** (ENT-003) |
| Product version | `main.go:83` `0.86.0`; `installer/windows/mnemo.iss` `#define AppVersion "0.0.0"` (release-substituted — likely fine) | not verified end-to-end |
| Todo index | SQLite `todos*`, `todo_globs` config, fswatch names, `internal/todo` disk parser for threads | **yes** (ENT-004) |
| Search | `Store.Search` vs `UnifiedSearchOpts` | dual path (ENT-005) |
| Topic spans | structural `internal/segment`, compaction spans, streamseg; ranked in store | deliberate layering |
| Compactor health DTO | `compact.HealthSnapshot` vs `tools.CompactorHealth` (`tools.go:74–77`) | deliberate cycle break |
| LLM caller | `compact.LLMCaller` and `reviewer.LLMCaller` + `registry.llmAdapter` (`registry.go:42–61`) | deliberate cycle break |
| HTTP vs MCP | `api.Handler` REST for dashboard/shim; `tools.Handler` for agents; threads share `threads.View` (`view.go:8–10`) | healthy split |
| Schema | `schema.sql` vs live DB via sqlift | enforced AllowNone + additive test |

## Healthy structure worth retaining

- **Acyclic `internal/` packages** (import-graph SCC check). Registry
  and duplicated DTOs (`CompactorHealth`, `LLMCaller`) exist *because*
  of that constraint — keep the copies, do not merge packages.
- **Tool-surface ledger + bidirectional ratchet**
  (`surface.go`, `surface_test.go:35–77`). This is the fix for the
  70-tool failure mode; do not weaken it to a threshold.
- **sqlift `AllowNone` on the shipped path** (`store.go:912–914`)
  plus `TestSchemaUpgradeIsAdditive` (plans last-release schema →
  current; skips rather than faking green if git baseline missing).
- **`mnemo_query` hardening after Fable-5:** dedicated RO driver,
  authorizer denies ATTACH and `query_only` unset (`rodriver.go`),
  tests in `store_test.go` around 502–540. Federated tools are an
  allowlist (`federated.go:24–34`).
- **Ingest oversize fix (T104)** with an explicit regression
  (`audit_fable5_test.go`) and `ReadBytes` on Claude/Codex/Grok.
- **Opt-in egress matrix** (pricing, CLIP, Admin API, vault LLM /
  embeddings, federation) as documented in `CLAUDE.md` — restoring an
  old backup does not silently start those calls.
- **MNEMO_HOME isolation** and scale-test snapshot rules
  (`docs/testing.md`) — tests cannot touch the owner’s live DB.
- **Corpus registry** (`corpora.go`) as the way to add search
  domains without adding tools.
- **fswatch FD cap + telemetry** (🎯T142) instead of unbounded
  recursive watches.
- **Append-only schema + three-phase GC doctrine** — do not “clean
  up” `todos` tables with a migration.

## Hygiene posture

**Hygiene posture not declared.** There is no `hygiene.yaml` at the
repo root.

Explicit validator run (mandatory even though undeclared):

```
$ /Users/marcelo/.claude/skills/hygiene/hygiene_check.py
Traceback (most recent call last):
  ...
FileNotFoundError: [Errno 2] No such file or directory:
  '/Users/marcelo/work/github.com/marcelocantos/mnemo/hygiene.yaml'
```

Exit 1. The validator does not currently print a clean “undeclared”
message; it crashes on the missing file. This audit did **not**
initialize hygiene.

Observed (undeclared) reality, for when someone onboards later:

| Dimension | Rough held reality | Notes |
|---|---|---|
| correctness | tests in CI (ubuntu + windows cloud); e2e nightly; schema additive test; Windows VM gate is extra-repo | ENT-007 |
| quality | `gofmt`/`vet` in Makefile only; gofmt currently dirty | ENT-006 |
| security | no scanners | ENT-010 |
| release | GitHub Releases workflow, Homebrew documented | sqldeep pin ENT-002 |
| docs | README/LICENSE/gitignore present; surface docs drifted | ENT-003 |
| agent | CLAUDE.md + agents-guide + bullseye.yaml | — |

Overlap with entropy: ENT-002/006/007/010 are the items a future
`hygiene.yaml` should declare as `enforced` or honest `planned`.
Do not ratchet yaml from this report without a separate onboard.

## Oracle coverage and residue

| Property | Decided by | Notes |
|---|---|---|
| MCP tool names = ledger = count 18 | shipped test `TestToolSurfaceRatchet` / `TestToolSurfaceSize` | healthy |
| Schema upgrades are additive under AllowNone | shipped `TestSchemaUpgradeIsAdditive` + ApplyOptions | healthy |
| `mnemo_query` cannot write / ATTACH | shipped store tests + `rodriver` | healthy; Fable F2 closed |
| Oversized JSONL ingest | shipped `TestIngestOversizedLineNotDropped` | healthy; backfill scanner is residue (ENT-009) |
| Daemon MCP round-trip | auxiliary nightly e2e (`MNEMO_E2E=1`) | not on PR CI |
| Dashboard layout | HTML substring grep | not a render oracle |
| macOS watch/OCR/menubar | darwin unit tests, not CI | ENT-007 |
| sqldeep pin consistency | nothing | ENT-002 |
| gofmt on merge | Makefile only | ENT-006 |
| Full unit/integration suite on this snapshot | **not re-run** | residue |
| govulncheck / secrets | nothing | ENT-010 |
| Hygiene floors | n/a | undeclared |

**Owner residue (intent / taste, not mechanical leftover):**

1. Split `Backend` now, or wait until 1.0 freezes it (ENT-001)?
2. Keep committed `replace` to sibling sqldeep for local iteration,
   or publish-only (ENT-002)?
3. Restore a todo indexer, or fully retire config/watch (ENT-004)?
4. Accept nightly-only e2e and grep-only dashboard as the product
   bar, or add a thin PR journey (ENT-007)?
5. Onboard `hygiene.yaml` at all?

## Remediation sequence

1. **Unify the sqldeep pin** (ENT-002) so PR CI, win-validate, and
   release compile the same CGO library `go.mod` names. Add the
   string-equality ratchet. This is the only item that can make
   today’s merge signal lie about the shipped binary.
2. **Declare the local format/vet gate in CI** (ENT-006, ENT-012) —
   cheap, unblocks `make bullseye` once the seven files are
   formatted in a follow-up (not this commit).
3. **Converge docs to the ledger** (ENT-003) — README 18, drop empty
   tables, CLAUDE.md two missing tools. Optional hygiene command
   later.
4. **Retire todo config/watch wiring** without dropping tables
   (ENT-004). Delete dead Go methods that are not vault dumps
   (ENT-008).
5. **Route `expand` through unified search** (ENT-005) before anyone
   turns `DefaultSegmentExpand` on.
6. **Peel `Backend`** (ENT-001) along consumer ports; ratchet
   interface width. Do not rewrite `store.go`.
7. **Add macos-latest to CI** and decide the e2e/dashboard journey
   question (ENT-007) as owner residue.
8. **Onboard `hygiene.yaml`** only if requested, encoding the
   ratchets from 1–2 and 7 as `enforced` and the rest as `planned`.
9. Re-run this audit against the same definitions.

No architectural rewrite is required to close the P1s.
