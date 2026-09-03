#!/bin/sh
# Start mnemo for supervisord. Homebrew Cellar only on :19419 —
# a tree-built binary on that port is the double-serve failure in CLAUDE.md.
set -e

if [ -z "${HOME:-}" ]; then
  HOME="$(eval echo ~"$(id -un)")"
  export HOME
fi
export USER="${USER:-$(id -un)}"
export PATH="/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:${HOME}/.cargo/bin:${HOME}/.local/bin:${HOME}/.py/bin:${HOME}/go/bin:${HOME}/.claude/local:/usr/bin:/bin:/usr/sbin:/sbin"

if [ -n "${MNEMO_BIN:-}" ]; then
  BIN="$MNEMO_BIN"
  if [ ! -x "$BIN" ]; then
    echo "mnemo: MNEMO_BIN=$BIN is not executable" >&2
    exit 1
  fi
else
  BIN="$(command -v mnemo 2>/dev/null || true)"
  if [ -z "$BIN" ] && command -v brew >/dev/null 2>&1; then
    BIN="$(brew --prefix)/opt/mnemo/bin/mnemo"
  fi
  if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
    echo "mnemo: no mnemo on PATH and no Homebrew install found." >&2
    echo "mnemo: brew install marcelocantos/tap/mnemo" >&2
    exit 1
  fi
fi

if [ "${1:-}" = "--print-bin" ]; then
  echo "$BIN"
  exit 0
fi

# Bare mnemo is the HTTP daemon (🎯T160). --addr only when the operator
# pins one; otherwise the binary's localhost dual-stack default stands.
if [ -n "${MNEMO_ADDR:-}" ]; then
  echo "mnemo: running $BIN --addr $MNEMO_ADDR" >&2
  exec "$BIN" --addr "$MNEMO_ADDR"
fi
echo "mnemo: running $BIN" >&2
exec "$BIN"
