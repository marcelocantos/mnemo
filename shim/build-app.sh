#!/usr/bin/env bash
# Copyright 2026 Marcelo Cantos
# SPDX-License-Identifier: Apache-2.0
#
# Assemble Mnemo.app from the SPM build (🎯T85.5). Produces a signed
# .app bundle the mnemo daemon launches via `open -g` (Integration §0.1).
#
# Usage:
#   shim/build-app.sh [OUTPUT_DIR]
#
# OUTPUT_DIR defaults to shim/dist. For distribution, sign with a Developer ID
# and notarize (see DISTRIBUTION below); the default here is an ad-hoc signature
# which is sufficient for a stable local TCC identity on the build machine.
set -euo pipefail

SHIM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${1:-$SHIM_DIR/dist}"
APP="$OUT_DIR/Mnemo.app"
# Signing identity. A *stable* identity is what makes TCC grants persist across
# rebuilds — Accessibility (the ⌥⌥ hotkey) and Automation (iTerm2). Ad-hoc ("-")
# changes the cdhash every build, which revokes those grants and forces a
# re-grant after each rebuild. So default to a local "Apple Development" identity
# when one exists; fall back to ad-hoc only when none is present (e.g. CI).
SIGN_ID="${MNEMO_SIGN_ID:-}"
if [ -z "$SIGN_ID" ]; then
    SIGN_ID="$(security find-identity -v -p codesigning 2>/dev/null \
        | awk -F'"' '/Apple Development/{print $2; exit}')"
    SIGN_ID="${SIGN_ID:--}"
fi

echo "==> swift build -c release"
swift build --package-path "$SHIM_DIR" -c release

BIN="$(swift build --package-path "$SHIM_DIR" -c release --show-bin-path)/Mnemo"
[ -x "$BIN" ] || { echo "build produced no Mnemo binary at $BIN" >&2; exit 1; }

echo "==> assembling $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/Mnemo"
cp "$SHIM_DIR/Info.plist" "$APP/Contents/Info.plist"

echo "==> codesign (identity: $SIGN_ID)"
codesign --force --options runtime --sign "$SIGN_ID" "$APP"
codesign --verify --strict "$APP" && echo "    signature OK"

echo "Built $APP"
cat <<'DISTRIBUTION'

DISTRIBUTION (cross-repo / human steps):
  - For Homebrew distribution, sign with a Developer ID and notarize:
      MNEMO_SIGN_ID="Developer ID Application: <name> (<team>)" shim/build-app.sh
      xcrun notarytool submit dist/Mnemo.app.zip --keychain-profile <p> --wait
      xcrun stapler staple dist/Mnemo.app
  - The mnemo Homebrew formula (marcelocantos/homebrew-tap) installs the .app
    under libexec and the daemon auto-launches it; see shim/PACKAGING.md.
DISTRIBUTION
