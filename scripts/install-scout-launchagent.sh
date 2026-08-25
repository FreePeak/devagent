#!/usr/bin/env bash
# Install/validate/uninstall the devagent scout LaunchAgent (macOS).
# Usage:
#   scripts/install-scout-launchagent.sh --repo <path> [--interval <min>] [--worker opencode|claude-code]
#   scripts/install-scout-launchagent.sh --validate                 # plutil -lint only
#   scripts/install-scout-launchagent.sh --uninstall                # bootout + remove plist
set -euo pipefail

LABEL="com.devagent.scout"
PLIST_DIR="${HOME}/Library/LaunchAgents"
PLIST="${PLIST_DIR}/${LABEL}.plist"
LOG_FILE="${HOME}/Library/Logs/devagent-scout.log"

REPO=""
INTERVAL="30"
WORKER="opencode"
TIMEOUT_MIN="12"
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
CLI_JS="${REPO}/dist/src/cli.js"
[ -f "$CLI_JS" ] || { echo "missing ${CLI_JS}; run npm run build first" >&2; exit 1; }

mkdir -p "$PLIST_DIR" "$(dirname "$LOG_FILE")"

NODE_BIN="$(command -v node)"
# LaunchAgents get a minimal default PATH; embed this shell's PATH so the
# scout can find opencode/claude/git installed in user locations.
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
  </dict>
  <key>ProgramArguments</key>
  <array>
    <string>${NODE_BIN}</string>
    <string>${CLI_JS}</string>
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
