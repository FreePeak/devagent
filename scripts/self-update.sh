#!/usr/bin/env bash
# devagent self-update: pull latest main, rebuild the Go binary, restart the
# scout LaunchAgent. Guarded: refuses to run on a dirty worktree. Never prints
# secrets. (FR-GO-16 #205: the Node npm ci/build steps are gone — the single
# build step is the make build recipe.)
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

echo "[self-update] go build (devagent-go)"
go build -trimpath -o devagent-go ./cmd/devagent

if [ "$(uname -s)" = "Darwin" ] && launchctl print "gui/${UID_NUM}/${LABEL}" >/dev/null 2>&1; then
  echo "[self-update] launchctl kickstart ${LABEL}"
  launchctl kickstart "gui/${UID_NUM}/${LABEL}"
fi

echo "[self-update] ok"
