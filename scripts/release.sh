#!/usr/bin/env bash
# Local release for mnemo, run from the macOS/arm64 release host
# (Colossus). Replaces the old GitHub Actions release workflow.
#
#   scripts/release.sh [--dry-run] [--notes FILE] VERSION
#   make release VERSION=x.y.z [DRY_RUN=1] [NOTES=FILE]
#
# Artifacts (names unchanged from the GHA era, minus Linux):
#   mnemo-VER-darwin-arm64.tar.gz        mnemo + NOTICE, built natively here
#   mnemo-VER-windows-{amd64,arm64}.zip  mnemo.exe + NOTICE, built on the VM
#   mnemo-VER-windows-{amd64,arm64}-setup.exe  Inno Setup installers (VM)
#
# Real run (not --dry-run), in order:
#   1. require: clean tree, HEAD == origin/master, main.go version == VERSION
#   2. build + package everything into dist/vVER/
#   3. tag vVER at HEAD and push the tag
#   4. create the GitHub release (or reuse one that already exists) and
#      upload the five assets with local `gh`
#   5. render Formula/mnemo.rb (darwin/arm64 only, real sha256s) into a
#      fresh clone of marcelocantos/homebrew-tap and push it with local
#      git/gh auth
# --dry-run does step 2 only (plus a rendered formula with a placeholder
# source sha256) and never tags, uploads or pushes the tap.
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
#   TAP_REPO             tap repo (default marcelocantos/homebrew-tap)
set -euo pipefail

DRY_RUN=0
NOTES=""
VERSION=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --notes) NOTES="${2:?--notes needs a file}"; shift ;;
    -h|--help) sed -n '2,36p' "$0"; exit 0 ;;
    -*) echo "release: unknown flag $1" >&2; exit 2 ;;
    *) VERSION="$1" ;;
  esac
  shift
done

VM="${WINCI_VM:-hms-vm}"
SQLDEEP_REF="${SQLDEEP_REF:-v0.23.0}"
TAP_REPO="${TAP_REPO:-marcelocantos/homebrew-tap}"
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
for c in git go gh ssh scp shasum tar curl; do
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

cp "$STAGE/mnemo/NOTICE" "$STAGE/NOTICE"
cp "$DARWIN_BIN" "$STAGE/mnemo-bin"
(
  cd "$STAGE" && mkdir pkg && cp mnemo-bin pkg/mnemo && cp NOTICE pkg/NOTICE
  COPYFILE_DISABLE=1 tar -czf "$OUT/mnemo-$VERSION-darwin-arm64.tar.gz" -C pkg mnemo NOTICE
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

# --- formula ----------------------------------------------------------------
render_formula() { # $1 = source tarball sha256, $2 = darwin tarball sha256
  cat <<RUBY
# typed: true
# frozen_string_literal: true

# Generated by mnemo's scripts/release.sh. Edit the template there.
class Mnemo < Formula
  depends_on "gh"
  depends_on "mupdf"
  depends_on "poppler"
  depends_on "tesseract"
  depends_on "uv"

  service do
    run [opt_bin/"mnemo"]
    keep_alive true
    log_path var/"log/mnemo.log"
    error_log_path var/"log/mnemo.log"
    working_dir HOMEBREW_PREFIX
    # std_service_path_env FIRST. The compactor spawns \`claude\`,
    # which Homebrew does not package and which lives in a per-user
    # directory, so those two stay on the daemon's PATH — but behind
    # the system path, or anything planted in a user-writable dir
    # would win for a background service. Everything else mnemo
    # spawns (uv, gh, tesseract, poppler, mupdf) is a declared
    # dependency and resolves from HOMEBREW_PREFIX.
    environment_variables PATH: [
      std_service_path_env,
      "#{Dir.home}/.local/bin",
      "#{Dir.home}/.claude/local",
    ].join(":")
  end

  desc "Claude code transcript memory  mcp server for searching past sessions"
  homepage "https://github.com/$REPO"
  url "https://github.com/$REPO/archive/refs/tags/$TAG.tar.gz"
  version "$VERSION"
  sha256 "$1"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/$REPO/releases/download/$TAG/mnemo-$VERSION-darwin-arm64.tar.gz"
      sha256 "$2"
    end
  end

  def install
    bin.install "mnemo" => "mnemo"
  end

  test do
    system bin/"mnemo", "--version"
  end
end
RUBY
}
darwin_sha="$(shasum -a 256 "$OUT/mnemo-$VERSION-darwin-arm64.tar.gz" | cut -d' ' -f1)"

listing() {
  say "artifacts in $REL_OUT (darwin $SIGNED):"
  (cd "$OUT" && for f in "${ASSETS[@]}"; do
    printf '  %-44s %10s bytes  %s\n' "$f" "$(stat -f %z "$f")" "$(grep -F " $f" SHA256SUMS | cut -c1-16)…"
  done)
}

if [ "$DRY_RUN" = 1 ]; then
  render_formula "0000000000000000000000000000000000000000000000000000000000000000" "$darwin_sha" > "$OUT/mnemo.rb"
  if command -v ruby >/dev/null; then ruby -c "$OUT/mnemo.rb" >/dev/null && say "formula syntax OK"; fi
  listing
  say "DRY RUN: rendered formula at $REL_OUT/mnemo.rb (placeholder source sha256)"
  say "DRY RUN: skipped tag push, GitHub release upload and tap push"
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

src_sha="$(curl -fsSL "https://github.com/$REPO/archive/refs/tags/$TAG.tar.gz" | shasum -a 256 | cut -d' ' -f1)"
render_formula "$src_sha" "$darwin_sha" > "$OUT/mnemo.rb"

TAP="$STAGE/tap"
gh repo clone "$TAP_REPO" "$TAP" -- --quiet --depth 1
cp "$OUT/mnemo.rb" "$TAP/Formula/mnemo.rb"
(
  cd "$TAP"
  git add Formula/mnemo.rb
  if git diff --cached --quiet; then
    say "tap formula already up to date"
  else
    git commit --quiet -m "Brew formula update for mnemo version $TAG"
    git push --quiet origin HEAD
    say "pushed tap formula for $TAG"
  fi
)
listing
say "released $TAG: https://github.com/$REPO/releases/tag/$TAG"
