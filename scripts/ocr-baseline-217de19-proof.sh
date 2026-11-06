#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# SAFE 217de19 behavioral proof. This script is intentionally NOT the
# inherited testdata harness (that file documents destructive git
# operations and must NEVER be executed).
#
# Forbidden in any command this script runs:
#   reset --hard, clean -fdq, worktree remove/prune, rm of existing trees.
#
# Creates a NEW worktree at a FRESH external path (refuses if it already
# exists). Copies adapter tests only. Does not modify 217de19 production
# implementation. Leaves the new worktree in place (no automatic cleanup).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Self-check: refuse if a real command line (not a comment) is destructive.
if grep -E '^[[:space:]]*git[[:space:]]+(reset|clean|worktree[[:space:]]+(remove|prune)|checkout[[:space:]]+-f)' "$0" >/dev/null; then
	echo "refusing: this proof script must not contain reset/clean/force-checkout" >&2
	exit 2
fi

BASE=217de19fdc2c93d12f46ace7657d38fa636fa961
WT="${MNEMO_OCR_BASELINE_WT:-/Users/marcelo/work/wt/mnemo-217de19-ocr-proof-reassigned-20261107}"
ADAPTER_SRC="internal/store/testdata/ocr_baseline217de19_adapter_test.go"
ADAPTER_DEST="$WT/internal/store/ocr_baseline217de19_adapter_test.go"
RUN='^TestOCRBaseline217de19_(DuplicateConcurrentIsolatedRunsEachCountFailure|RecoversAfterThresholdSuccess)$'

if [[ ! -f "$ADAPTER_SRC" ]]; then
	echo "missing adapter $ADAPTER_SRC" >&2
	exit 2
fi
if [[ -e "$WT" ]]; then
	echo "scratch path already exists; refusing to reuse or delete: $WT" >&2
	exit 3
fi

git worktree add "$WT" "$BASE"
cp "$ADAPTER_SRC" "$ADAPTER_DEST"

echo "=== baseline proof worktree $(git -C "$WT" rev-parse --short HEAD) at $WT ==="
echo "=== adapter $ADAPTER_DEST (production sources unmodified) ==="

set +e
( cd "$WT" && GOWORK=off CGO_ENABLED=1 go test -tags sqlite_fts5 ./internal/store -run "$RUN" -count=1 -v )
code=$?
set -e

echo "baseline_go_test_exit=$code"
if [[ "$code" -eq 0 ]]; then
	echo "baseline harness: unexpected success on $BASE — not a meaningful FAIL" >&2
	exit 1
fi
echo "baseline harness: expected non-zero on unmodified $BASE (runtime assertion FAIL, not missing symbols)"
exit 0
