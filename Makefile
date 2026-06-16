.PHONY: bullseye build test test-scale snapshot vet fmt-check

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
	@test -z "$$(git status --porcelain)" && echo "✓ clean" || \
	 (echo "✗ dirty tree:"; git status --short; exit 1)

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

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
