#!/usr/bin/env bash
# One authority for the sqldeep version. go.mod is it; every workflow
# checkout ref and the Windows-VM gate default must agree with it.
#
# Rationale: the merge gates (PR CI and scripts/win-validate.sh) once
# pinned an older sqldeep than the nightly and the release workflows, so
# a sqldeep regression could be green on a PR and red on release, with
# nothing to say which was telling the truth.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

want="$(sed -n 's|^[[:space:]]*github.com/marcelocantos/sqldeep/go/sqldeep \(v[0-9.]*\).*|\1|p' "$ROOT/go.mod" | head -1)"
if [ -z "$want" ]; then
  echo "check-sqldeep-pin: no sqldeep require line in go.mod" >&2
  exit 2
fi

status=0
report() { # name actual
  printf '%-40s %s\n' "$1" "$2"
  [ "$2" = "$want" ] || status=1
}

report "go.mod" "$want"

for wf in ci.yml e2e-nightly.yml release.yml; do
  path="$ROOT/.github/workflows/$wf"
  [ -f "$path" ] || continue
  # Every `ref:` inside a checkout step for repository marcelocantos/sqldeep.
  n=0
  while IFS= read -r ref; do
    n=$((n + 1))
    report "$wf ref #$n" "$ref"
  done < <(awk '
    /repository: *marcelocantos\/sqldeep/ { insqldeep = 1; next }
    insqldeep && /ref: *v[0-9]/ { sub(/.*ref: */, ""); print $1; insqldeep = 0 }
    insqldeep && /^[[:space:]]*- / { insqldeep = 0 }
  ' "$path")
  [ "$n" -gt 0 ] || echo "check-sqldeep-pin: no sqldeep checkout found in $wf (ok if intentional)" >&2
done

winref="$(sed -n 's|^SQLDEEP_REF="${WINCI_SQLDEEP_REF:-\(v[0-9.]*\)}"|\1|p' "$ROOT/scripts/win-validate.sh" | head -1)"
[ -n "$winref" ] && report "win-validate.sh default" "$winref"

windoc="$(sed -n 's|.*WINCI_SQLDEEP_REF (default \(v[0-9.]*\)).*|\1|p' "$ROOT/scripts/win-validate.sh" | head -1)"
[ -n "$windoc" ] && report "win-validate.sh doc comment" "$windoc"

if [ "$status" -ne 0 ]; then
  echo >&2
  echo "check-sqldeep-pin: FAIL — the pins above disagree with go.mod ($want)." >&2
  echo "Raise the laggards; never lower the release, which is the shipped artefact." >&2
fi
exit "$status"
