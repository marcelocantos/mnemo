#!/usr/bin/env bash
# Local release for mnemo, run from the macOS/arm64 release host.
# Replaces the old GitHub Actions release workflow.
#
#   scripts/release.sh [--dry-run] [--notes FILE] VERSION
#   make release VERSION=x.y.z [DRY_RUN=1] [NOTES=FILE]
#
# Artifacts (names unchanged from the GHA era, minus Linux):
#   mnemo-VER-darwin-arm64.tar.gz        mnemo + NOTICE + LICENSE.zstd, built natively here
#   mnemo-VER-windows-{amd64,arm64}.zip  mnemo.exe + NOTICE + LICENSE.zstd, built on the VM
#   mnemo-VER-windows-{amd64,arm64}-setup.exe  Inno Setup installers (VM)
#
# Real run (not --dry-run), in order:
#   1. require: clean tree, HEAD == origin/master, main.go version == VERSION
#   2. build + package everything into dist/vVER/
#   3. tag vVER at HEAD and push the tag
#   4. create the GitHub release (or reuse one that already exists) and
#      upload the five assets with local `gh`
#   5. update the Homebrew tap with tapper (scripts/release-tap.sh ->
#      `tapper push`, config in tapper.yaml, token from the keychain);
#      tapper reads the uploaded assets, so it must run after step 4
# --dry-run does step 2 only and never tags, uploads or runs tapper.
#
# The Windows half runs scripts/win-release.ps1 on the Parallels VM. The
# commit must be on origin (the VM clones it), so push before releasing.
#
# Env:
#   MNEMO_SIGN_IDENTITY  codesign identity for the darwin binary. Default:
#                        the first "Developer ID Application" identity in
#                        the keychain. None found -> unsigned, with a warning.
#   WINCI_VM             ssh host for the Windows VM (default hms-vm)
#   SQLDEEP_REF          sqldeep tag to build against (default v0.23.0)
set -euo pipefail

DRY_RUN=0
NOTES=""
VERSION=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --notes) NOTES="${2:?--notes needs a file}"; shift ;;
    -h|--help) sed -n '/^set -euo/q;2,$p' "$0"; exit 0 ;;
    -*) echo "release: unknown flag $1" >&2; exit 2 ;;
    *) VERSION="$1" ;;
  esac
  shift
done

VM="${WINCI_VM:-hms-vm}"
SQLDEEP_REF="${SQLDEEP_REF:-v0.23.0}"
REPO="marcelocantos/mnemo"
REMOTE_PS="C:/Users/marcelo/win-release.ps1"
REMOTE_OUT="C:/Users/marcelo/winrel/out"

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

die() { echo "release: $*" >&2; exit 1; }
say() { echo "release: $*"; }

code_version="$(sed -nE 's/^const version = "([^"]+)".*/\1/p' main.go)"
[ -n "$VERSION" ] || VERSION="$code_version"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "VERSION must be x.y.z (got '$VERSION')"
TAG="v$VERSION"

# --- preflight -------------------------------------------------------------
[ "$(uname -s)/$(uname -m)" = "Darwin/arm64" ] || die "run on a macOS/arm64 host (darwin artifact is built natively)"
for c in git go gh ssh scp shasum tar; do
  command -v "$c" >/dev/null || die "missing command: $c"
done

if [ "$code_version" != "$VERSION" ]; then
  if [ "$DRY_RUN" = 1 ]; then
    say "WARNING: main.go version is $code_version, packaging as $VERSION (dry run only)"
  else
    die "main.go version is $code_version, not $VERSION — land the release-prep commit first"
  fi
fi

sha="$(git rev-parse HEAD)"
git fetch --quiet origin
if [ -z "$(git branch -r --contains "$sha" 2>/dev/null)" ]; then
  die "HEAD ${sha:0:12} is not on any remote branch — push first (the Windows VM clones it)"
fi

if [ "$DRY_RUN" = 0 ]; then
  [ -z "$(git status --porcelain)" ] || die "working tree is dirty"
  [ "$sha" = "$(git rev-parse origin/master)" ] || die "HEAD is not origin/master"
  if git ls-remote --exit-code --tags origin "refs/tags/$TAG" >/dev/null 2>&1; then
    remote_tag_sha="$(git ls-remote origin "refs/tags/$TAG^{}" | cut -f1)"
    [ -n "$remote_tag_sha" ] || remote_tag_sha="$(git ls-remote origin "refs/tags/$TAG" | cut -f1)"
    [ "$remote_tag_sha" = "$sha" ] || die "tag $TAG already exists on origin at ${remote_tag_sha:0:12}"
  fi
  gh auth status >/dev/null 2>&1 || die "gh is not logged in"
  command -v tapper >/dev/null || die "tapper not on PATH — brew install marcelocantos/tap/tapper"
  docker info >/dev/null 2>&1 || die "docker is not running (tapper runs homebrew-releaser in a container)"
  [ -n "$NOTES" ] || gh release view "$TAG" -R "$REPO" >/dev/null 2>&1 || \
    die "no existing release $TAG and no --notes FILE given"
  [ -z "$NOTES" ] || [ -f "$NOTES" ] || die "notes file not found: $NOTES"
fi

REL_OUT="dist/$TAG"
OUT="$ROOT/$REL_OUT"
STAGE="$(mktemp -d -t mnemo-release)"
trap 'rm -rf "$STAGE"' EXIT
rm -rf "$OUT"
mkdir -p "$OUT"
say "${TAG} from ${sha:0:12}$( [ "$DRY_RUN" = 1 ] && echo ' (DRY RUN)')"
say "staging in $STAGE, artifacts -> $REL_OUT"

# --- darwin/arm64 ------------------------------------------------------------
# Fresh, hermetic tree: the committed source (not the working tree) next to
# a pinned sqldeep, matching go.mod's replace => ../sqldeep/go/sqldeep.
git archive --format=tar --prefix=mnemo/ "$sha" | tar -x -C "$STAGE"
git clone --quiet --depth 1 --branch "$SQLDEEP_REF" --recurse-submodules --shallow-submodules \
  https://github.com/marcelocantos/sqldeep.git "$STAGE/sqldeep"

(
  cd "$STAGE/sqldeep"
  DP=vendor/deepparser/src
  mkdir -p build/deepparser
  for f in arena liteparser lp_tokenize lp_unparse parse; do
    cc -Wall -Wextra -Wno-unused-parameter -Wno-sign-compare -O2 -DNDEBUG -I"$DP" \
      -c "$DP/$f.c" -o "build/deepparser/$f.o"
  done
  c++ -std=c++20 -O2 -Idist -Ivendor/include -I"$DP" -c dist/sqldeep.cpp -o build/sqldeep.o
  cc -w -Idist -Ivendor/include -c dist/sqldeep_xml.c -o build/sqldeep_xml.o
  ar rcs build/libsqldeep.a build/sqldeep.o build/deepparser/*.o
)
say "built sqldeep $SQLDEEP_REF"

(
  cd "$STAGE/mnemo"
  GOWORK=off CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -tags sqlite_fts5 -ldflags "-s -w" -o mnemo .
)
DARWIN_BIN="$STAGE/mnemo/mnemo"
got="$("$DARWIN_BIN" --version 2>&1 | head -1)"
say "darwin binary: $got"

# Sign with a stable Team ID so TCC grants survive `brew upgrade` (🎯T101).
ident="${MNEMO_SIGN_IDENTITY:-}"
if [ -z "$ident" ]; then
  ident="$(security find-identity -v -p codesigning 2>/dev/null \
    | sed -nE 's/.*"(Developer ID Application:[^"]+)".*/\1/p' | head -1)"
fi
if [ -n "$ident" ] && security find-identity -v -p codesigning 2>/dev/null | grep -qF "$ident"; then
  codesign --force --options runtime --timestamp --sign "$ident" "$DARWIN_BIN"
  codesign --verify --strict --verbose=2 "$DARWIN_BIN"
  SIGNED="signed: $ident"
else
  SIGNED="UNSIGNED (no Developer ID Application identity in keychain)"
  say "WARNING: darwin binary is $SIGNED"
fi

(
  cd "$STAGE" && mkdir pkg
  cp "$DARWIN_BIN" pkg/mnemo
  cp mnemo/NOTICE pkg/NOTICE
  cp mnemo/internal/zstdc/LICENSE.zstd pkg/LICENSE.zstd
  COPYFILE_DISABLE=1 tar -czf "$OUT/mnemo-$VERSION-darwin-arm64.tar.gz" -C pkg mnemo NOTICE LICENSE.zstd
)
say "packaged mnemo-$VERSION-darwin-arm64.tar.gz"

# --- windows (amd64, arm64) on the VM --------------------------------------
say "windows: building on $VM (llvm-mingw + Inno Setup)…"
scp -q "$ROOT/scripts/win-release.ps1" "$VM:$REMOTE_PS" || die "scp to $VM failed (is the VM up?)"
win_log="$OUT/win-release.log"
ssh -o ConnectTimeout=15 "$VM" \
  "powershell -NoProfile -ExecutionPolicy Bypass -File $REMOTE_PS -Sha $sha -Version $VERSION -SqldeepRef $SQLDEEP_REF" \
  >"$win_log" 2>&1 || true
grep -vE '^[[:space:]]*$' "$win_log" | sed 's/^/  vm| /'
grep -q "win_release_exit=0" "$win_log" || die "windows build failed on $VM (log: $REL_OUT/win-release.log)"
for a in amd64 arm64; do
  for f in "mnemo-$VERSION-windows-$a.zip" "mnemo-$VERSION-windows-$a-setup.exe"; do
    scp -q "$VM:$REMOTE_OUT/$f" "$OUT/$f" || die "could not fetch $f from $VM"
  done
done
rm -f "$win_log"

ASSETS=(
  "mnemo-$VERSION-darwin-arm64.tar.gz"
  "mnemo-$VERSION-windows-amd64.zip"
  "mnemo-$VERSION-windows-arm64.zip"
  "mnemo-$VERSION-windows-amd64-setup.exe"
  "mnemo-$VERSION-windows-arm64-setup.exe"
)
(cd "$OUT" && shasum -a 256 "${ASSETS[@]}" > SHA256SUMS)

listing() {
  say "artifacts in $REL_OUT (darwin $SIGNED):"
  (cd "$OUT" && for f in "${ASSETS[@]}"; do
    printf '  %-44s %10s bytes  %s\n' "$f" "$(stat -f %z "$f")" "$(grep -F " $f" SHA256SUMS | cut -c1-16)…"
  done)
}

if [ "$DRY_RUN" = 1 ]; then
  listing
  command -v tapper >/dev/null || say "WARNING: tapper not on PATH (needed for the real run)"
  say "DRY RUN: skipped tag push, GitHub release upload and tapper push (tap)"
  exit 0
fi

# --- publish ------------------------------------------------------------------
if ! git ls-remote --exit-code --tags origin "refs/tags/$TAG" >/dev/null 2>&1; then
  git tag -a "$TAG" -m "$TAG" "$sha"
  git push origin "refs/tags/$TAG"
fi
if gh release view "$TAG" -R "$REPO" >/dev/null 2>&1; then
  say "release $TAG exists; uploading assets"
else
  gh release create "$TAG" -R "$REPO" --verify-tag --title "$TAG" --notes-file "$NOTES"
fi
(cd "$OUT" && gh release upload "$TAG" -R "$REPO" "${ASSETS[@]}" SHA256SUMS)

# The tap formula comes from tapper.yaml; tapper reads the assets just
# uploaded, so this must stay after the upload.
"$ROOT/scripts/release-tap.sh" "$TAG"
listing
say "released $TAG: https://github.com/$REPO/releases/tag/$TAG"
