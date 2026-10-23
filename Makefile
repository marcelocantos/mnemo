.PHONY: bullseye build test test-scale e2e snapshot vet fmt-check release

# Recipes use bash so pipefail / $$status preserve go test's exit
# through the output-trimming step (🎯T194).
SHELL := /bin/bash

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
	@set -o pipefail; \
	  log=$$(mktemp -t mnemo-bullseye-tests.XXXXXX); \
	  go test -tags "$(BUILD_TAGS)" ./... >$$log 2>&1; \
	  status=$$?; \
	  tail -20 $$log; \
	  if [ $$status -ne 0 ]; then \
	    echo "--- failing tests ---"; \
	    grep -E '^(--- FAIL|FAIL[[:space:]])' $$log || true; \
	  fi; \
	  rm -f $$log; \
	  [ $$status -eq 0 ] && echo "✓ tests" || exit $$status
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
# Tier 3 scale tests are deliberately excluded so the default gate
# stays fast and never reaches at the user's real data.
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

# Daemon-subprocess e2e suite (internal/e2e), gated behind MNEMO_E2E=1
# so `go test ./...` stays fast. The harness builds its own mnemo
# binary. Run it locally before releases or after touching the
# transport/daemon lifecycle; nothing schedules it.
e2e:
	MNEMO_E2E=1 go test -tags "$(BUILD_TAGS)" -timeout=10m -v -run . ./internal/e2e/

# Local release (replaces the GitHub Actions release workflow). See
# docs/releasing.md. DRY_RUN=1 builds and packages every artifact into
# dist/vVERSION/ but never tags, uploads or pushes the Homebrew tap.
# (Not `make --dry-run`: that is make's own flag.)
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=x.y.z [DRY_RUN=1] [NOTES=file]" >&2; exit 2; }
	scripts/release.sh $(if $(DRY_RUN),--dry-run) $(if $(NOTES),--notes $(NOTES)) $(VERSION)

# 🎯T73: invoke the snapshot helper. Prints `MNEMO_HOME=<path>` on
# its last stdout line for `eval $(make snapshot)` workflows.
snapshot:
	@go run ./cmd/mnemo-test-snapshot

vet:
	go vet -tags "$(BUILD_TAGS)" ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
