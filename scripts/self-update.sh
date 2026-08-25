#!/usr/bin/env bash
# devagent self-update: pull latest main, rebuild, restart the scout LaunchAgent.
# Guarded: refuses to run on a dirty worktree. Never prints secrets.
set -euo pipefail

REPO_PATH="${1:-$(pwd)}"
LABEL="com.devagent.scout"
UID_NUM="$(id -u)"

cd "$REPO_PATH"

DIRTY="$(git status --porcelain | grep -v -e '\.devagent/' -e '\.selfbuild/' || true)"
if [ -n "$DIRTY" ]; then
  echo "self-update skipped: dirty worktree" >&2
  exit 1
fi

echo "[self-update] git pull --ff-only"
git pull --ff-only

if [ -f package-lock.json ]; then
  echo "[self-update] npm ci"
  npm ci --ignore-scripts
else
  echo "[self-update] npm install"
  npm install --ignore-scripts
fi

echo "[self-update] npm run build"
npm run build

if [ "$(uname -s)" = "Darwin" ] && launchctl print "gui/${UID_NUM}/${LABEL}" >/dev/null 2>&1; then
  echo "[self-update] launchctl kickstart ${LABEL}"
  launchctl kickstart "gui/${UID_NUM}/${LABEL}"
fi

echo "[self-update] ok"
