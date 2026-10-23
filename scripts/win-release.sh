#!/usr/bin/env bash
# One-time (idempotent) provisioning of the Windows release toolchain on
# the VM: llvm-mingw + Inno Setup under C:\Users\marcelo\winrel\tools.
# scripts/release.sh does the actual builds.
#
#   scripts/win-release.sh --provision
set -euo pipefail
VM="${WINCI_VM:-hms-vm}"
REMOTE_PS="C:/Users/marcelo/win-release.ps1"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ "${1:-}" = "--provision" ] || { echo "usage: $0 --provision" >&2; exit 2; }
scp -q "$HERE/win-release.ps1" "$VM:$REMOTE_PS"
ssh -o ConnectTimeout=15 "$VM" \
  "powershell -NoProfile -ExecutionPolicy Bypass -File $REMOTE_PS -Provision -ProvisionOnly"
