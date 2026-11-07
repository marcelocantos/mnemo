# mnemo OCR single-flight / cooldown — review report (REVIEW ONLY)

**Written:** 2026-11-07 — **not** a “ready to ship” proof; see **§8 Gaps**.

**Scope:** Review branch + PR only. No merge to default branch, force-push, release, live daemon/DB change, requeue, or destructive git operations. Preserve live **0.116.0 PID 27921**.

---

## 1. Branch + PR

| Item | Value |
|------|--------|
| Branch | `fix/ocr-singleflight-cooldown` |
| **PR** | https://github.com/marcelocantos/mnemo/pull/210 |
| **Exact HEAD (this correction)** | `d48c25ade9ceb52b69d8b8acf7b4bf3b5a53006d` (short `d48c25a`) |

---

## 2. Controller-aligned behaviour (what changed this pass)

1. **`images.ocr` (Fast, in-memory only)** — `internal/registry/diagchecks.go`  
   - **WARN** when `OCRProcessStatus().Reason == apple_vision_breaker` and breaker is **`open` (cooldown active)** or **`half_open` (probing)**.  
   - Detail carries **trip time** (`OpenedAt`), **last error**, **remaining cooldown** (open), **next half-open / probing window** (`NextTrialAt`), and explicit copy that **cooldown expiry alone is not recovery** — only a **successful actual trial** closes the breaker.  
   - **`disable_ocr` (config)** remains an explicit **healthy** path (normal disable).

2. **`OCRProcessStatus` / snapshot** — `internal/store/ocr_subprocess.go`  
   - While the underlying `breaker.Snapshot` can still say `open` until `Allow()` runs, after cooldown elapses we surface **effective `half_open`** in the snapshot for **diagnostics alignment** with “next trial” semantics.  
   - **`Disabled: true`** only while **open with cooldown still remaining** (Apple Vision not offered via `ocrBackend()`); half_open probing does not flip `Disabled` in the same way (operator still sees WARN + detail).  

3. **Successful close only on a real success** — `runAppleVisionOCRIsolated` now calls `Record(success)` only when the worker JSON has **no `error` field**. A **clean exit with `error` in JSON** is **`Record(false)`** (counts toward threshold / half-open failure).

4. **At most one concurrent half-open subprocess** — `ocrHalfOpenTrial` mutex + `TryLock` so parallel callers get a deterministic “recovery trial already in progress” error instead of N parallel probes.

5. **Test isolation** — `OCRBreakerTestLock()` mutex so `internal/store` and `internal/registry` tests do not race on the process-wide breaker when `-parallel > 1`.

---

## 3. Named tests (deterministic / health)

| Area | Test |
|------|------|
| Single-flight + one failure | `TestOCRSingleFlightDedupesConcurrentWorkers`, `TestOCRSingleFlightOneFailedRunOneBreakerCount` |
| JSON failure → breaker | `TestOCRJSONReportedErrorCountsTowardBreaker` |
| Threshold / reset / cooldown | `TestOCRCircuitBreakerOpensAfterLimit`, `TestOCRSuccessResetsBreakerRun`, `TestOCRCircuitBreakerRecoversAfterCooldown`, `TestOCRHalfOpenFailedTrialReopensWithCooldown` |
| Half-open concurrency | `TestOCRHalfOpenSerializesConcurrentTrials` |
| `/health` images.ocr | `TestImagesOCRHealthWarnsWhenBreakerOpen`, `TestImagesOCRHealthWarnsWhenBreakerHalfOpen`, `TestImagesOCRHealthSequenceOpenToClosed`, `TestImagesOCRHealthOkWhenEnabled` |

---

## 4. True baseline @ `217de19` (adapter harness — meaningful FAIL)

**Not** a checkout of `217de19` (would be destructive / out of scope). **Tagged adapter** `internal/store/ocr_baseline217de19_test.go` (`//go:build ocr_baseline217de19`) reproduces **duplicate concurrent `runAppleVisionOCRIsolated` deaths without single-flight** and asserts the **post-fix** contract (`Failures == 1`). On that semantic, the test **FAILs with a runtime assertion** (not missing symbols):

```bash
# BEFORE semantics (meaningful FAIL — e.g. got 3, want 1):
GOWORK=off go test -tags 'sqlite_fts5,ocr_baseline217de19' ./internal/store -run TestOCRBaseline217de19 -count=1
```

**AFTER (default build — PASS, file excluded):**

```bash
GOWORK=off go test -tags sqlite_fts5 ./internal/store -run TestOCRBaseline217de19 -count=1
# ok (no tests run)
```

---

## 5. Four gates — commands (run in worktree; preserve real exit codes)

Record **`git rev-parse HEAD`**, **`git status -sb`** before and after each gate.

1. `make bullseye`
2. `GOWORK=off go test -tags sqlite_fts5 ./...`
3. `make e2e`
4. **Windows (hms-vm, PowerShell, one run, final HEAD only):** `scripts/win-validate.sh` — **must** be re-run on the **exact final commit** after these corrections; result at `c74df85` is **not** sufficient.

**This session (macOS worktree) @ `d48c25a`, clean tree (`## fix/...` ahead of origin by 1, no dirty files):**

| Step | HEAD | `git status` | Exit |
|------|------|----------------|------|
| before `make bullseye` | `d48c25a` | clean | `bullseye_exit=0` |
| before `go test ./...` | `d48c25a` | clean | `go_test_exit=0` |
| before `make e2e` | `d48c25a` | clean | `e2e_exit=0` |

Commands (pipelines **not** masked — each exit printed):

```bash
cd "$(git rev-parse --show-toplevel)"
echo "=== BEFORE bullseye ===" && git rev-parse HEAD && git status -sb
make bullseye; echo "bullseye_exit=$?"
echo "=== BEFORE go test ===" && git rev-parse HEAD && git status -sb
GOWORK=off go test -tags sqlite_fts5 ./...; echo "go_test_exit=$?"
echo "=== BEFORE e2e ===" && git rev-parse HEAD && git status -sb
make e2e; echo "e2e_exit=$?"
```

**Note:** An earlier **dirty-tree** dry run at parent `c74df85` was used only during development; **final** gate rows above are **`d48c25a` + clean** (single authoritative run after commit).

**Still required (not this seat):** `scripts/win-validate.sh` on **hms-vm** (PowerShell) at **`d48c25a` exactly once** — old `c74df85` result invalid per review brief.

---

## 6. Cooldown / health semantics (summary)

- **Closed:** `images.ocr` **ok** when Apple Vision path is healthy (and not config-disabled).  
- **Open (cooldown active):** **WARN** — trips counted, **remaining** cooldown and **next** half-open instant documented; **not** “recovered” when timer hits zero.  
- **Half-open (probing):** **WARN** — one recovery trial semantics; success **closes**; failure **re-opens** with **fresh bounded cooldown**.  
- **Single-flight** reduces duplicate concurrent **logical** failures per image; it is a **mitigation**. **Native Vision / Metal root cause** for systemic aborts is **not proved** in this harness.

---

## 7. Historical note

Duplicate concurrent validations **did** occur before correction; this report does not claim otherwise.

---

## 8. Gaps / blockers (honest)

- **`scripts/win-validate.sh` on hms-vm** at **final HEAD** after this commit: **not executed in this agent seat** — required manually (PowerShell, one run).  
- **Native Apple Vision** concurrency / abort **root cause** is **not** lab-proved here; only subprocess isolation + breaker + single-flight behaviour are tested.  
- **Intermediate broken implementation** is **not** substitute for baseline proof — only the tagged adapter + narrative above.  
- Do **not** treat this document as “green everywhere” until **all four gates** are re-run on the **committed** head and **win-validate** is fresh.

---

## Milestones

- **2026-11-07** — Controller review corrections: `images.ocr` WARN open + half_open, richer `OCRProcessStatus`, JSON failure recording, half-open trial mutex, registry/store tests + baseline adapter, `OCRBreakerTestLock` for parallel safety.
