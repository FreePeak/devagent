#!/usr/bin/env bash
# Auto-close and delete spawned DevAgent self-build sessions in Orca (macOS).
#
# The "devagent-selfbuild" watchdog automation creates one new Orca workspace
# (auto-devagent-selfbuild-run-N-<ts>) plus a live terminal per run and never
# reclaims them. This script finds those leftovers and tears them down safely.
#
# Safety gates per workspace (ALL must pass before close/delete):
#   1. Idle: nothing inside the workspace modified within ORCA_MIN_AGE_SECS.
#   2. Branch safety: the worktree HEAD is already merged into origin/main
#      (or pushed to origin verbatim), so deletion cannot lose work.
#   3. Never touches the main repo worktree or non-selfbuild workspaces.
#
# Dry-run by default. Pass --apply to actually stop terminals and remove
# worktrees. Pass --install-launchagent to schedule it hourly on macOS.
#
# Protocol docs: docs/SELF-BUILD-LOOP.md ("Orca integration", mode A cleanup)
set -euo pipefail

# LaunchAgent contexts run with a bare PATH; make sure orca/git/jq are reachable.
PATH="/usr/local/bin:/opt/homebrew/bin:$PATH"
export PATH

REPO_SELECTOR="${ORCA_SELFBUILD_REPO:-name:devagent}"
MIN_AGE_SECS="${ORCA_MIN_AGE_SECS:-3600}"
RUN_MAX_AGE_SECS="${ORCA_RUN_MAX_AGE_SECS:-14400}"
MAIN_REPO="${ORCA_MAIN_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
PATTERN='auto-devagent-'
APPLY=0
ACTION="none"

# Workspace paths owned by automation runs Orca still considers open, i.e.
# status is anything but completed/failed and the run started within
# RUN_MAX_AGE_SECS (bounds protection if Orca never marks a run terminal).
live_run_paths() {
  local aids aid now_ms
  now_ms="$(( $(date +%s) * 1000 ))"
  aids="$(orca automations list --json 2>/dev/null | jq -r '.result.automations[]?.id // empty' 2>/dev/null)" || aids=""
  [ -n "$aids" ] || return 0
  while IFS= read -r aid; do
    [ -n "$aid" ] || continue
    orca automations runs --id "$aid" --json 2>/dev/null \
      | jq -r --argjson now "$now_ms" --argjson max "$(( RUN_MAX_AGE_SECS * 1000 ))" '
          (.result.runs // [])[]
          | select(.status != "completed" and .status != "failed")
          | select((.scheduledFor // 0) > ($now - $max))
          | .workspaceId // empty' 2>/dev/null || true
  done <<< "$aids" | sed 's/^.*:://' | sort -u
}

for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=1 ;;
    --install-launchagent) ACTION="install-launchagent" ;;
    --uninstall-launchagent) ACTION="uninstall-launchagent" ;;
    *) echo "unknown arg: $arg (supported: --apply --install-launchagent --uninstall-launchagent)" >&2 ; exit 2 ;;
  esac
done

launchagent_plist() {
  echo "$HOME/Library/LaunchAgents/com.devagent.orca-selfbuild-cleanup.plist"
}

install_launchagent() {
  local script plist label
  script="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  plist="$(launchagent_plist)"
  label="com.devagent.orca-selfbuild-cleanup"
  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>$script</string>
    <string>--apply</string>
  </array>
  <key>StartInterval</key><integer>3600</integer>
  <key>RunAtLoad</key><false/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/orca-selfbuild-cleanup.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/orca-selfbuild-cleanup.log</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  echo "[launchagent] installed $plist (hourly, --apply); log: ~/Library/Logs/orca-selfbuild-cleanup.log"
}

uninstall_launchagent() {
  local plist label
  plist="$(launchagent_plist)"
  label="com.devagent.orca-selfbuild-cleanup"
  launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1 || true
  rm -f "$plist"
  echo "[launchagent] uninstalled"
}

cleanup_workspaces() {
  cd "$MAIN_REPO"
  # Freshen the merge-safety reference; offline runs keep stale refs and stay safe
  # because merged-in commits never leave origin.
  git fetch origin main --quiet || echo "[git] fetch failed, using cached origin/main"

  local removed=0 skipped=0 candidates
  # Never fail the sweep on empty/malformed orca output (this crashed the whole
  # run with "Cannot iterate over null" under LaunchAgent, exit code 5).
  candidates="$(orca worktree list --repo "$REPO_SELECTOR" --json 2>/dev/null \
    | jq -r '(.result.worktrees // [])
             | .[]?
             | select(.isMainWorktree == false)
             | select(.path | contains("orca/workspaces/") and contains("'"$PATTERN"'"))
             | [.id, .path] | @tsv')" || candidates=""

  if [ -z "$candidates" ]; then
    echo "[cleanup] no selfbuild workspaces found"
    return 0
  fi

  # Read from fd 3 so commands inside the loop cannot swallow the candidate list.
  local live_paths
  live_paths="$(live_run_paths)"
  while IFS="$(printf '\t')" read -r -u 3 wt_id wt_path; do
    [ -n "$wt_id" ] || continue
    local_name="$(basename "$wt_path")"

    # Gate 0: Orca run liveness. A workspace whose automation run is still
    # open in Orca must never be torn down, even if the filesystem looks idle:
    # deleting an active run's workspace is what made runs vanish mid-life.
    if grep -Fqx "$wt_path" <<<"$live_paths"; then
      echo "[skip] $local_name : orca run still open (protected)"
      skipped=$(( skipped + 1 ))
      continue
    fi

    # Gate 1: idle age. Skip anything touched recently (a run may still be working).
    if [ -n "$(find "$wt_path" -newermt "-${MIN_AGE_SECS} seconds" -print -quit 2>/dev/null)" ]; then
      echo "[skip] $local_name : modified within last ${MIN_AGE_SECS}s (possibly active)"
      skipped=$(( skipped + 1 ))
      continue
    fi

    # Gate 2: branch safety. HEAD must be an ancestor of origin/main (or exist
    # verbatim on origin) so removing the worktree loses no commits.
    head_sha="$(git -C "$wt_path" rev-parse HEAD 2>/dev/null || true)"
    if [ -z "$head_sha" ]; then
      echo "[skip] $local_name : cannot resolve HEAD"
      skipped=$(( skipped + 1 ))
      continue
    fi
    if ! git merge-base --is-ancestor "$head_sha" origin/main 2>/dev/null \
       && [ "$(git ls-remote origin "refs/heads/$local_name" 2>/dev/null | cut -f1)" != "$head_sha" ]; then
      echo "[skip] $local_name : HEAD ${head_sha} not on origin (unpushed work)"
      skipped=$(( skipped + 1 ))
      continue
    fi

    if [ "$APPLY" = 1 ]; then
      # Close any terminal attached to this worktree, then delete the workspace.
      orca terminal stop --worktree "id:$wt_id" </dev/null >/dev/null 2>&1 || true
      # Detach nested git worktrees (devagent task spawns .devagent-worktrees/TASK);
      # orca worktree rm refuses to delete a workspace that still contains one.
      git -C "$wt_path" worktree list --porcelain 2>/dev/null \
        | awk '/^worktree /{print $2}' | tail -n +2 \
        | while IFS= read -r nested; do
            git -C "$wt_path" worktree remove --force "$nested" >/dev/null 2>&1 || rm -rf "$nested"
          done
      # Nested devagent-task worktrees can be registered in Orca without appearing
      # in worktree list; drop them by path or the parent rm is refused.
      orca worktree rm --worktree "path:$wt_path/.devagent-worktrees/TASK" --force </dev/null >/dev/null 2>&1 || true
      if orca worktree rm --worktree "id:$wt_id" --force </dev/null >/dev/null 2>&1; then
        echo "[removed] $local_name (terminal closed, worktree deleted)"
        removed=$(( removed + 1 ))
      elif ! orca worktree list --repo "$REPO_SELECTOR" --json 2>/dev/null | grep -qF "$wt_id"; then
        # Registration already gone but the directory stayed behind (seen when
        # Orca drops a workspace itself). Gates 0-2 passed, so the tree holds
        # no unpushed work: reclaim it directly.
        rm -rf "$wt_path" \
          && { echo "[removed] $local_name (orphan dir reclaimed: registration was gone)"; removed=$(( removed + 1 )); } \
          || { echo "[error] $local_name : orphan rm failed"; skipped=$(( skipped + 1 )); }
      else
        echo "[error] $local_name : worktree rm failed"
        skipped=$(( skipped + 1 ))
      fi
    else
      echo "[dry-run] would close terminal + delete $local_name"
      removed=$(( removed + 1 ))
    fi
  done 3<<< "$candidates"

  echo "[cleanup] done: $removed reclaimed, $skipped skipped (apply=$APPLY)"
}

case "$ACTION" in
  install-launchagent) install_launchagent ;;
  uninstall-launchagent) uninstall_launchagent ;;
esac

cleanup_workspaces
