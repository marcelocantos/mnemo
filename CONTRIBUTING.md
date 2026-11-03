# Contributing to mnemo

mnemo uses a **PR-free flow**: there is no GitHub Actions CI, and
changes are committed straight to `master` once the local gates pass.

## Gates (required before pushing to master)

1. `make bullseye` on macOS — gofmt, `go vet`, build and
   `go test -tags sqlite_fts5 ./...`.
2. `scripts/win-validate.sh` reports **PASS** — builds and tests the
   pushed commit on the Windows ARM64 VM (`ssh hms-vm`).

Useful but not gating: `make e2e` (daemon-subprocess suite,
`MNEMO_E2E=1`) and `make test-scale` (snapshot-gated Tier 3; see
`docs/testing.md`).

## Platforms

macOS (arm64) and Windows (amd64, arm64). Linux is not supported.

## Releases

`make release VERSION=x.y.z` from the release host; see
`docs/releasing.md`.

## Tests

Use generic fixtures (`/Users/alice/dev/app`, `example/repo`), never
real transcripts or paths. See `docs/testing.md`.
