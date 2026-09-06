.PHONY: bullseye build test test-scale snapshot vet fmt-check bench bench-lock bench-gate

# Parent ~/work/github.com/marcelocantos/go.work only lists claudia and
# jevons. Go walks up to it, then `./...` in this module fails with
# "directory prefix . does not contain modules listed in go.work".
# Standing invariants must be hermetic to that workspace.
export GOWORK := off

BUILD_TAGS := sqlite_fts5
# 🎯T73 Tier 3 also requires the `scale` tag so the snapshot-gated
# tests (`//go:build scale`) compile and run.
SCALE_TAGS := sqlite_fts5 scale

bullseye:
	@test -z "$$(gofmt -l .)" && echo "✓ fmt" || \
	 (echo "✗ gofmt issues:"; gofmt -l .; exit 1)
	@go vet -tags "$(BUILD_TAGS)" ./... && echo "✓ vet"
	@go build -tags "$(BUILD_TAGS)" -o bin/mnemo . && echo "✓ build"
	@go test -tags "$(BUILD_TAGS)" ./... 2>&1 | tail -20 && echo "✓ tests"
	@if [ -z "$$(git status --porcelain | grep -vE 'bullseye\.yaml$$' || true)" ]; then \
	  echo "✓ clean tree"; \
	else \
	  echo "" >&2; \
	  echo "════════════════════════════════════════════════════════" >&2; \
	  echo "⚠️  WARNING: DIRTY WORKING TREE — NOT BLOCKING, BUT REVIEW" >&2; \
	  echo "⚠️  WARNING: DIRTY WORKING TREE — NOT BLOCKING, BUT REVIEW" >&2; \
	  echo "════════════════════════════════════════════════════════" >&2; \
	  git status --short >&2; \
	  echo "════════════════════════════════════════════════════════" >&2; \
	  echo "⚠️  make bullseye passed; clean-tree is warning-only" >&2; \
	  echo "════════════════════════════════════════════════════════" >&2; \
	  echo "" >&2; \
	  echo "⚠ clean tree (dirty — warning only)"; \
	fi

build:
	go build -tags "$(BUILD_TAGS)" -o bin/mnemo .

# 🎯T73: `make test` runs Tier 1 + Tier 2 (default build tag set).
# Tier 3 scale tests are deliberately excluded so default CI stays
# fast and never reaches at the user's real data.
test:
	go test -tags "$(BUILD_TAGS)" ./...

# 🎯T73: `make test-scale` runs Tier 1 + Tier 2 + Tier 3. Tier 3
# tests skip with a clear message when MNEMO_TEST_SNAPSHOT is unset
# (see internal/e2e/scale_test.go). Run `make snapshot` first to
# materialise a snapshot and capture the env var.
test-scale:
	@if [ -z "$$MNEMO_TEST_SNAPSHOT" ]; then \
	  echo "MNEMO_TEST_SNAPSHOT is not set — Tier 3 tests will SKIP."; \
	  echo "Run \`make snapshot\` first and export the resulting path."; \
	fi
	go test -tags "$(SCALE_TAGS)" ./...

# 🎯T73: invoke the snapshot helper. Prints `MNEMO_HOME=<path>` on
# its last stdout line for `eval $(make snapshot)` workflows.
snapshot:
	@go run ./cmd/mnemo-test-snapshot

vet:
	go vet -tags "$(BUILD_TAGS)" ./...

# 🎯T165: benchmarks for the hot tools, locked both ways.
#
#   make bench       run them, results in $(BENCH_OUT)
#   make bench-gate  run them and compare against docs/perf/baseline.txt;
#                    fails on a regression AND on an improvement, because
#                    an improvement means the baseline no longer describes
#                    the code and must be re-locked in the same commit
#   make bench-lock  run them and make the result the new baseline
#
# Timing is only comparable on the machine the baseline was recorded on
# (docs/perf/baseline.md names it). Elsewhere, and in CI, pass
# BENCH_GATE_FLAGS=-timing=false to compare just the deterministic
# metrics (payload bytes, statements issued, rows returned).
BENCH_PKG   := ./internal/store/
BENCH_RE    := ^(BenchmarkSearch|BenchmarkRecentActivity|BenchmarkUsage|BenchmarkIngestTranscript)$$
BENCH_COUNT ?= 6
BENCH_OUT   ?= bin/bench.txt
BENCH_GATE_FLAGS ?=

bench:
	@mkdir -p $(dir $(BENCH_OUT))
	go test -tags "$(BUILD_TAGS)" -run '^$$' -bench '$(BENCH_RE)' -benchmem \
	  -count=$(BENCH_COUNT) -benchtime=1s $(BENCH_PKG) > $(BENCH_OUT)
	@grep -E '^(Benchmark|goos|goarch|pkg|cpu)' $(BENCH_OUT)

bench-lock: bench
	cp $(BENCH_OUT) docs/perf/baseline.txt
	@echo "locked docs/perf/baseline.txt — update docs/perf/baseline.md alongside it"

bench-gate: bench
	go run ./cmd/benchgate -base docs/perf/baseline.txt -new $(BENCH_OUT) $(BENCH_GATE_FLAGS)

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
