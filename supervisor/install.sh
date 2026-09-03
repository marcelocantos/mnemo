#!/bin/sh
# Render supervisor/mnemo.ini into supervisor.d and make supervisord
# the owner of :19419. Exactly one parent may own that port — this
# installer evicts brew services / launchd before starting (same
# takeover as spyder/bullseye supervisor/install.sh).
set -e

REPO="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
CONF_DIR="${SUPERVISOR_CONF_DIR:-/opt/homebrew/etc/supervisor.d}"
DEST="$CONF_DIR/mnemo.ini"
TEMPLATE="$REPO/supervisor/mnemo.ini"
PORT="${MNEMO_PORT:-19419}"

if [ -z "${HOME:-}" ]; then
  HOME="$(eval echo ~"$(id -un)")"
  export HOME
fi

mkdir -p "$CONF_DIR"
mkdir -p "$HOME/.local/var/log"
chmod +x "$REPO/supervisor/run-mnemo.sh" "$REPO/supervisor/install.sh"

rm -f "$DEST"
sed "s|@REPO@|$REPO|g" "$TEMPLATE" >"$DEST"
echo "rendered $DEST (from $TEMPLATE)"

if [ "${SUPERVISOR_SKIP_CTL:-}" = 1 ]; then
  exit 0
fi

if ! command -v supervisorctl >/dev/null 2>&1; then
  echo "supervisorctl not on PATH — ini written; start Homebrew supervisor to load it" >&2
  exit 1
fi

# Evict launchd / brew services so they cannot reclaim :19419.
if command -v brew >/dev/null 2>&1; then
  brew services stop mnemo >/dev/null 2>&1 || true
fi
if command -v launchctl >/dev/null 2>&1; then
  launchctl bootout "gui/$(id -u)/homebrew.mxcl.mnemo" 2>/dev/null || true
fi

if command -v lsof >/dev/null 2>&1; then
  holder="$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN -t 2>/dev/null || true)"
  holder="$(printf '%s\n' "$holder" | head -n 1)"
  if [ -n "$holder" ]; then
    echo "mnemo: stopping pid $holder still holding :$PORT"
    kill "$holder" 2>/dev/null || true
    i=0
    while [ "$i" -lt 20 ] && kill -0 "$holder" 2>/dev/null; do
      sleep 1
      i=$((i + 1))
    done
    kill -9 "$holder" 2>/dev/null || true
  fi
fi

supervisorctl reread
supervisorctl update
supervisorctl restart mnemo 2>/dev/null || supervisorctl start mnemo

echo "mnemo installed at $DEST"
supervisorctl status mnemo
