#!/usr/bin/env bash
# Regenerate golden image fixtures from Markdown sources.
# Requires: vellum (MD -> PDF), pdftoppm (PDF -> PNG).
#
# Usage: scripts/gen-golden-images.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR="$REPO_ROOT/internal/store/testdata/images"

command -v vellum   >/dev/null || { echo "vellum not found on PATH"   >&2; exit 1; }
command -v pdftoppm >/dev/null || { echo "pdftoppm not found on PATH" >&2; exit 1; }

cd "$DIR"

# 1) Markdown -> PDF via vellum (goldmark + KaTeX + Mermaid + Prince).
for md in *.md; do
  vellum convert "$md" -o "${md%.md}.pdf"
done

# 2) PDF -> PNG via pdftoppm (150 DPI, first page only).
for pdf in *.pdf; do
  base="${pdf%.pdf}"
  pdftoppm -png -r 150 -f 1 -l 1 "$pdf" "$base"
  # pdftoppm emits base-1.png; rename to base.png.
  if [ -f "${base}-1.png" ]; then
    mv -f "${base}-1.png" "${base}.png"
  fi
done

echo "Regenerated $(ls *.png | wc -l | tr -d ' ') PNG fixtures in $DIR"
