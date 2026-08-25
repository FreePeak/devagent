#!/usr/bin/env bash
# git-cleanup-merged.sh — auto-cleanup of local branches + worktrees whose MR/PR was merged,
# across ALL nested git repos under a scan root (default: ~/work).
#
# Why: ~270 nested repos accumulate stale local branches and worktrees after their
# GitHub PRs / GitLab MRs are merged. Nothing else on this machine covers that scope:
#   - reclaim-disk.sh            → worktrees only, freepeak only, no forge awareness
#   - OmniRoute prune-stale-*    → single repo
#   - cleanup-stale.sh           → orphaned processes, unrelated
#
# How it decides a branch is safe to delete (ALL gates must pass):
#   1. Merged signal:
#        a. forge API confirms a MERGED MR/PR exists for this source branch
#           (gh for github remotes, glab for gitlab remotes), OR
#        b. fallback (offline/no-forge): branch tip is an ancestor of origin/<default>
#           — catches true merges but NOT squash merges (use the API for those).
#   2. Not a protected branch: main/master/develop*/staging/production/release*.
#   3. Not checked out anywhere (main checkout or any surviving worktree).
#   4. Its worktree (if any) is clean — no uncommitted/untracked files.
#   5. No unpushed commits ahead of its upstream (when an upstream exists).
#
# Usage:
#   scripts/git-cleanup-merged.sh                    # dry-run report over ~/work
#   scripts/git-cleanup-merged.sh --root ~/work/be   # limit scan root
#   scripts/git-cleanup-merged.sh --apply            # actually remove worktrees/branches
#   scripts/git-cleanup-merged.sh --fetch            # git fetch --prune each repo first (slower, fresher refs)
#   scripts/git-cleanup-merged.sh --install-launchagent   # weekly auto-run (--apply --fetch) via launchd
#   scripts/git-cleanup-merged.sh --uninstall-launchagent
#
# Env:
#   GIT_CLEANUP_ROOT   scan root (default $HOME/work)
#   GIT_CLEANUP_GITLAB_HOSTS  comma-separated extra GitLab hostnames to recognize
#                      (default "git.begroup.team"; hosts containing "github"/"gitlab" are auto-detected)

set -euo pipefail

# LaunchAgent contexts run with a bare PATH; make sure git/gh/glab/jq are reachable.
PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
export PATH

ROOT="${GIT_CLEANUP_ROOT:-$HOME/work}"
GITLAB_HOSTS="${GIT_CLEANUP_GITLAB_HOSTS:-git.begroup.team}"
APPLY=0 FETCH=0 INSTALL=0 UNINSTALL=0 INTERVAL_HOURS=168

have() { command -v "$1" >/dev/null 2>&1; }
HAVE_GH=0 HAVE_GLAB=0
have gh && HAVE_GH=1
have glab && HAVE_GLAB=1

usage() { sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1 ;;
    --fetch) FETCH=1 ;;
    --root) ROOT="${2:?--root needs a value}"; shift ;;
    --install-launchagent) INSTALL=1 ;;
    --uninstall-launchagent) UNINSTALL=1 ;;
    --interval-hours) INTERVAL_HOURS="${2:?--interval-hours needs a value}"; shift ;;
    -h|--help|help) usage ;;
    *) echo "unknown arg: $1 (see --help)" >&2; exit 2 ;;
  esac
  shift
done

LABEL="com.devagent.git-cleanup-merged"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
SCRIPT_PATH="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
LOG_FILE="$HOME/.local/var/git-cleanup-merged.log"

install_launchagent() {
  mkdir -p "$HOME/.local/var" "$HOME/Library/LaunchAgents"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>$SCRIPT_PATH</string>
        <string>--apply</string>
        <string>--fetch</string>
    </array>
    <key>StartInterval</key><integer>$(( INTERVAL_HOURS * 3600 ))</integer>
    <key>StandardOutPath</key><string>$LOG_FILE</string>
    <key>StandardErrorPath</key><string>$LOG_FILE</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  echo "installed $PLIST (every ${INTERVAL_HOURS}h, runs --apply --fetch; log: $LOG_FILE)"
}

uninstall_launchagent() {
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  rm -f "$PLIST"
  echo "uninstalled $LABEL"
}

[ "$INSTALL" = 1 ] && { install_launchagent; exit 0; }
[ "$UNINSTALL" = 1 ] && { uninstall_launchagent; exit 0; }

if [ ! -d "$ROOT" ]; then echo "scan root does not exist: $ROOT" >&2; exit 2; fi
if [ "$HAVE_GH" = 0 ] && [ "$HAVE_GLAB" = 0 ]; then
  echo "note: neither gh nor glab found — falling back to merge-ancestry detection only (misses squash merges)" >&2
fi

# counters
N_REPOS=0 N_DEL=0 N_WOULD=0 N_KEEP=0 N_SKIP=0

say() { printf '%s\n' "$*"; }

protected() {
  case "$1" in
    main|master|develop|development|staging|production|release*|HEAD) return 0 ;;
    *) return 1 ;;
  esac
}

forge_of() {
  local url
  url=$(git -C "$1" config --get remote.origin.url 2>/dev/null) || { echo none; return; }
  case "$url" in
    *github*)          echo github ;;
    *gitlab*|*"$GITLAB_HOSTS"*) echo gitlab ;;
    *)                 echo none ;;
  esac
}

# api_branch_merged FORGE REPO BRANCH -> rc 0 = forge confirms a merged MR/PR,
# rc 1 = no merged MR/PR, rc 2 = could not verify (offline, auth, unknown forge)
api_branch_merged() {
  local forge="$1" repo="$2" br="$3" n=""
  case "$forge" in
    github)
      [ "$HAVE_GH" = 1 ] || return 2
      n=$(cd "$repo" && gh pr list --state merged --head "$br" --limit 5 \
            --json number --jq 'length' 2>/dev/null) || return 2 ;;
    gitlab)
      [ "$HAVE_GLAB" = 1 ] || return 2
      n=$(cd "$repo" && glab mr list --source-branch="$br" -M --per-page 100 \
            -F json --jq 'length' 2>/dev/null) || return 2 ;;
    *) return 2 ;;
  esac
  case "$n" in ''|*[!0-9]*) return 2 ;; esac
  [ "$n" -ge 1 ]
}

default_branch() {
  local b
  b=$(git -C "$1" symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null) && { echo "${b#origin/}"; return; }
  for b in main master; do
    if git -C "$1" show-ref --verify --quiet "refs/remotes/origin/$b"; then echo "$b"; return; fi
  done
  echo ""
}

process_repo() {
  local repo="$1" forge db br br_ref wt dirty up ahead merged_reason merged_rc
  forge=$(forge_of "$repo")

  # candidate local branches: everything except protected ones
  local -a cands=()
  while IFS= read -r br_ref; do
    br="${br_ref#refs/heads/}"
    protected "$br" && continue
    cands+=("$br")
  done < <(git -C "$repo" for-each-ref --format='%(refname)' refs/heads/ 2>/dev/null)
  [ "${#cands[@]}" -gt 0 ] || return 0

  N_REPOS=$((N_REPOS + 1))
  db=$(default_branch "$repo")

  # map branch -> secondary worktree path ("$br|$wt")
  local -a wts=()
  local first=1 porcelain_wt porcelain_br
  while IFS= read -r line; do
    case "$line" in
      worktree\ *) porcelain_wt="${line#worktree }" ;;
      branch\ *)
        if [ "$first" = 1 ]; then first=0; continue; fi   # main checkout entry
        porcelain_br="${line#branch refs/heads/}"
        wts+=("$porcelain_br|$porcelain_wt") ;;
      detached) first=0 ;;
    esac
  done < <(git -C "$repo" worktree list --porcelain 2>/dev/null)

  for br in "${cands[@]}"; do
    wt=""
    local entry
    for entry in "${wts[@]+"${wts[@]}"}"; do
      if [ "${entry%%|*}" = "$br" ]; then wt="${entry#*|}"; break; fi
    done

    # gate: clean worktree only
    if [ -n "$wt" ]; then
      dirty=$(git -C "$wt" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
      if [ "$dirty" != "0" ]; then
        say "SKIP   (dirty worktree)         [$repo] $br"
        N_SKIP=$((N_SKIP + 1)); continue
      fi
    fi

    # gate: nothing unpushed (a stale upstream — remote branch deleted after merge — is ignored)
    up=$(git -C "$repo" for-each-ref --format='%(upstream:short)' "refs/heads/$br" 2>/dev/null)
    if [ -n "$up" ] && ! git -C "$repo" rev-parse --verify --quiet "$up" >/dev/null 2>&1; then
      up=""
    fi
    if [ -n "$up" ]; then
      ahead=$(git -C "$repo" rev-list --count "$up..$br" 2>/dev/null || echo "?")
      if [ "$ahead" != "0" ]; then
        say "KEEP   ($ahead unpushed commits) [$repo] $br"
        N_KEEP=$((N_KEEP + 1)); continue
      fi
    fi

    # merged signal: forge API first, ancestry fallback
    merged_reason=""
    api_branch_merged "$forge" "$repo" "$br" && merged_rc=0 || merged_rc=$?
    if [ "$merged_rc" = 0 ]; then
      merged_reason="${forge} MR/PR merged"
    elif [ "$merged_rc" = 2 ] && [ -n "$db" ] \
        && git -C "$repo" merge-base --is-ancestor "$br" "refs/remotes/origin/$db" 2>/dev/null; then
      merged_reason="fully merged into origin/$db"
    fi

    if [ -z "$merged_reason" ]; then
      say "KEEP                           [$repo] $br"
      N_KEEP=$((N_KEEP + 1)); continue
    fi

    if [ "$APPLY" = 1 ]; then
      if [ -n "$wt" ]; then
        git -C "$repo" worktree unlock "$wt" 2>/dev/null || true
        git -C "$repo" worktree remove "$wt" >/dev/null 2>&1 \
          || { say "SKIP   (worktree remove failed) [$repo] $wt"; N_SKIP=$((N_SKIP + 1)); continue; }
      fi
      git -C "$repo" branch -D "$br" >/dev/null 2>&1 \
        || { say "SKIP   (branch delete failed)  [$repo] $br"; N_SKIP=$((N_SKIP + 1)); continue; }
      say "DEL    ($merged_reason)        [$repo] $br${wt:+ + worktree}"
      N_DEL=$((N_DEL + 1))
    else
      say "WOULD  ($merged_reason)        [$repo] $br${wt:+ + worktree}"
      N_WOULD=$((N_WOULD + 1))
    fi
  done

  git -C "$repo" worktree prune 2>/dev/null || true
}

START=$(date +%s)
say "== git-cleanup-merged $(date '+%Y-%m-%d %H:%M:%S') root=$ROOT mode=$([ "$APPLY" = 1 ] && echo APPLY || echo DRY-RUN) fetch=$FETCH forge(gh=$HAVE_GH glab=$HAVE_GLAB) =="

while IFS= read -r repo; do
  if [ "$FETCH" = 1 ] && git -C "$repo" remote get-url origin >/dev/null 2>&1; then
    git -C "$repo" fetch --prune --quiet origin 2>/dev/null || true
  fi
  process_repo "$repo" || true
done < <(find "$ROOT" -type d \( -name node_modules -o -name .venv -o -name target -o -name vendor \
     -o -name .build -o -name dist -o -name build -o -name out \) -prune -o \
     -type d -name .git -print 2>/dev/null | sed 's|/.git$||' | sort)

ELAPSED=$(( $(date +%s) - START ))
say "---"
say "repos touched: $N_REPOS | $([ "$APPLY" = 1 ] && echo deleted || echo would-delete): $([ "$APPLY" = 1 ] && echo "$N_DEL" || echo "$N_WOULD") | kept: $N_KEEP | skipped: $N_SKIP | ${ELAPSED}s"
[ "$APPLY" = 1 ] || say "(dry-run: pass --apply to execute; see --help)"
