#!/usr/bin/env bash
# Install/validate/uninstall the devagent scout LaunchAgent (macOS).
#
# FR-GO-15 cutover (#204): the plist launches the PATH-resolved devagent
# binary (command -v devagent, falling back to ~/.local/bin/devagent) — the
# Go binary after cutover, the npm-linked Node CLI during the soak window —
# with `scout` as the first argument. DEVAGENT_SUPPRESS_DEPRECATION=1 is
# written into the plist environment so the Node fallback's deprecation
# banner never pollutes the scout log.
# Usage:
#   scripts/install-scout-launchagent.sh --repo <path> [--interval <min>] [--worker opencode|claude-code|omp|pi]
#   scripts/install-scout-launchagent.sh --validate                 # plutil -lint only
#   scripts/install-scout-launchagent.sh --uninstall                # bootout + remove plist
set -euo pipefail

LABEL="com.devagent.scout"
PLIST_DIR="${HOME}/Library/LaunchAgents"
PLIST="${PLIST_DIR}/${LABEL}.plist"
LOG_FILE="${HOME}/Library/Logs/devagent-scout.log"

REPO=""
INTERVAL="30"
WORKER="omp"
TIMEOUT_MIN="30"
VALIDATE=0
UNINSTALL=0

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    --worker) WORKER="$2"; shift 2 ;;
    --timeout) TIMEOUT_MIN="$2"; shift 2 ;;
    --validate) VALIDATE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

UID_NUM="$(id -u)"
GUI_TARGET="gui/${UID_NUM}"

if [ "$VALIDATE" -eq 1 ]; then
  [ -f "$PLIST" ] || { echo "no plist at $PLIST" >&2; exit 1; }
  plutil -lint "$PLIST"
  exit 0
fi

if [ "$UNINSTALL" -eq 1 ]; then
  launchctl bootout "$GUI_TARGET/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST"
  echo "uninstalled $LABEL"
  exit 0
fi

[ -n "$REPO" ] || { echo "--repo <path> is required (or use --validate/--uninstall)" >&2; exit 2; }
REPO="$(cd "$REPO" && pwd)"
DEVAGENT_BIN="$(command -v devagent || true)"
[ -n "$DEVAGENT_BIN" ] || DEVAGENT_BIN="${HOME}/.local/bin/devagent"
[ -x "$DEVAGENT_BIN" ] || { echo "devagent binary not found (looked on PATH and ${HOME}/.local/bin/devagent); install it first" >&2; exit 1; }

mkdir -p "$PLIST_DIR" "$(dirname "$LOG_FILE")"

# LaunchAgents get a minimal default PATH; embed this shell's PATH so the
# scout can find opencode/claude/git/gh installed in user locations.
INSTALL_PATH="$PATH"

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>${INSTALL_PATH}</string>
    <key>HOME</key><string>${HOME}</string>
    <key>DEVAGENT_SUPPRESS_DEPRECATION</key><string>1</string>
  </dict>
  <key>ProgramArguments</key>
  <array>
    <string>${DEVAGENT_BIN}</string>
    <string>scout</string>
    <string>--repo</string><string>${REPO}</string>
    <string>--interval</string><string>${INTERVAL}</string>
    <string>--worker</string><string>${WORKER}</string>
    <string>--timeout</string><string>${TIMEOUT_MIN}</string>
  </array>
  <key>WorkingDirectory</key><string>${REPO}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${LOG_FILE}</string>
  <key>StandardErrorPath</key><string>${LOG_FILE}</string>
  <key>ThrottleInterval</key><integer>60</integer>
</dict>
</plist>
EOF

plutil -lint "$PLIST"

launchctl bootout "$GUI_TARGET/$LABEL" >/dev/null 2>&1 || true
launchctl bootstrap "$GUI_TARGET" "$PLIST"

# Read back and assert this install actually won the shared label slot.
if ! grep -q "<string>${REPO}</string>" "$PLIST"; then
  echo "warning: plist no longer points at ${REPO} (concurrent install?); reinstalling" >&2
  launchctl bootout "$GUI_TARGET/$LABEL" >/dev/null 2>&1 || true
  launchctl bootstrap "$GUI_TARGET" "$PLIST"
fi

echo "installed + started $LABEL (scout ${WORKER} every ${INTERVAL}m, repo ${REPO})"
