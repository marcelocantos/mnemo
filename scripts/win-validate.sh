#!/usr/bin/env bash
# 🎯T100: "local vm ci" — build + test mnemo on Windows on the Parallels
# VM, used as the windows-vm-validated pre-merge gate (replacing the slow
# required Windows cloud-CI check).
#
# Validates the *pushed* current commit: push your branch first, then run
# this. It scp's scripts/win-validate.ps1 to the VM and runs it (clone the
# commit + sqldeep, build libsqldeep.a with clang, `go test -tags
# sqlite_fts5` on windows/arm64). Exit 0 = Windows is green.
#
# Config: WINCI_VM (default hms-vm), WINCI_SQLDEEP_REF (default v0.22.0).
set -uo pipefail

VM="${WINCI_VM:-hms-vm}"
SQLDEEP_REF="${WINCI_SQLDEEP_REF:-v0.22.0}"
REMOTE_PS="C:/Users/marcelo/win-validate.ps1"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

sha="$(git rev-parse HEAD)"

# The gate validates what will merge, so the commit must be on origin.
if [ -z "$(git branch -r --contains "$sha" 2>/dev/null)" ]; then
  echo "win-validate: HEAD ${sha:0:12} is not on any remote branch — push first." >&2
  exit 2
fi

echo "win-validate: $VM building + testing ${sha:0:12} (windows/arm64, clang CGO)…"

if ! scp -q "$HERE/win-validate.ps1" "$VM:$REMOTE_PS"; then
  echo "win-validate: scp to $VM failed (is the VM up / ssh $VM reachable?)" >&2
  exit 3
fi

out="$(ssh -o ConnectTimeout=15 "$VM" \
  "powershell -NoProfile -ExecutionPolicy Bypass -File $REMOTE_PS -Sha $sha -SqldeepRef $SQLDEEP_REF" 2>&1)"
echo "$out" | grep -vE '^[[:space:]]*$'

# Authoritative pass signal is the script's own go_test_exit marker —
# robust against any ssh exit-code propagation quirks.
if printf '%s\n' "$out" | grep -q "go_test_exit=0"; then
  echo "win-validate: PASS (windows/arm64)"
  exit 0
fi
echo "win-validate: FAIL — Windows build/tests did not pass on $VM" >&2
exit 1
