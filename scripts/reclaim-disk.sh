#!/usr/bin/env bash
# reclaim-disk.sh — free disk space left behind by coding agents and builds.
#
# Machine disk space is limited: finished work must not leave worktrees,
# installed libs, or build caches behind. This script finds and removes them.
#
# Usage:
#   scripts/reclaim-disk.sh report                 # sizes only, no changes
#   scripts/reclaim-disk.sh clean [categories...]  # dry-run removal plan
#   scripts/reclaim-disk.sh clean --yes [cats]     # actually delete
#
# Categories:
#   worktrees  stale git worktrees (clean tree + fully merged branch only)
#   builds     node_modules/.venv/target inside removed-or-stale worktrees
#   caches     rebuildable tool caches (cargo-target, codex-runtimes, ...)
#   transcripts Claude Code shell snapshots + project transcripts older than 30d
#
# Never touches: opencode session DBs, cursor/opencode chat history,
# uncommitted files in any worktree. Those are data, not cache.

set -euo pipefail

APPLY=0
if [ "${1:-}" = "clean" ]; then
  shift
  if [ "${1:-}" = "--yes" ]; then APPLY=1; shift; fi
fi

MODE="${1:-report}"
[ "$MODE" = "clean" ] && shift
CATEGORIES="${*:-worktrees builds caches transcripts}"
SCAN_ROOT="${DEVAGENT_SCAN_ROOT:-$HOME/work/harvey/freepeak}"

freed=0
note() { printf '%s\n' "$*"; }

dir_size_mb() { du -sm "$1" 2>/dev/null | cut -f1 || echo 0; }

remove_dir() {
  local path="$1" mb
  mb=$(dir_size_mb "$path")
  if [ "$APPLY" = "1" ]; then
    rm -rf "$path"
    note "  removed ${mb}M  $path"
  else
    note "  would remove ${mb}M  $path"
  fi
  freed=$((freed + mb))
}

default_branch() {
  local repo="$1" b
  for b in main master; do
    if git -C "$repo" show-ref --verify --quiet "refs/heads/$b"; then echo "$b"; return; fi
  done
  for b in origin/main origin/master; do
    if git -C "$repo" rev-parse --verify --quiet "$b" >/dev/null; then echo "${b#origin/}"; return; fi
  done
}

clean_worktrees() {
  note "== worktrees under $SCAN_ROOT =="
  local repo wt base dirty ahead
  for repo in "$SCAN_ROOT"/*/; do
    [ -d "$repo/.git" ] || continue
    base=$(default_branch "$repo")
    while IFS= read -r wt; do
      [ -n "$wt" ] || continue
      # skip anything with uncommitted or untracked files — never destroy work
      dirty=$(git -C "$wt" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
      [ "$dirty" != "0" ] && { note "  SKIP (dirty)        $wt"; continue; }
      branch=$(git -C "$wt" branch --show-current 2>/dev/null)
      ahead=$(git -C "$repo" rev-list --count "$base..${branch:-HEAD}" 2>/dev/null || echo "?")
      [ "$ahead" != "0" ] && { note "  SKIP ($ahead unmerged commits) $wt"; continue; }
      git -C "$repo" worktree unlock "$wt" 2>/dev/null || true
      remove_dir "$wt"
      if [ "$APPLY" = "1" ] && [ -n "$branch" ]; then
        git -C "$repo" worktree prune
        git -C "$repo" branch -d "$branch" 2>/dev/null || true
      fi
    done < <(git -C "$repo" worktree list --porcelain 2>/dev/null | grep '^worktree ' \
             | sed 's/^worktree //' | grep -v "^$(cd "$repo" && pwd)$")
  done
}

clean_builds() {
  note "== build dirs inside stale worktrees =="
  find "$SCAN_ROOT" -type d \( -name node_modules -o -name .venv -o -name target \) \
    -path '*worktree*' -prune -print 2>/dev/null | while read -r d; do
    remove_dir "$d"
  done
}

clean_caches() {
  note "== rebuildable tool caches =="
  # cargo-target dominates: Rust incremental artifacts are safe to delete.
  local c
  for c in "$HOME/.cache/cargo-target" "$HOME/.cache/codex-runtimes" \
           "$HOME/.cache/kilo" "$HOME/.cache/opencode"; do
    [ -d "$c" ] && remove_dir "$c"
  done
}

clean_transcripts() {
  note "== agent transcripts older than 30 days =="
  local cutoff=$((30 * 86400)) f
  find "$HOME/.claude/projects" -type f -mtime +30 -print0 2>/dev/null | while IFS= read -rd '' f; do
    remove_dir "$f"   # works for single files too
  done
  find "$HOME/.claude/shell-snapshots" -type f -mtime +7 -print0 2>/dev/null | while IFS= read -rd '' f; do
    remove_dir "$f"
  done
}

for cat in $CATEGORIES; do
  case "$cat" in
    worktrees)   clean_worktrees ;;
    builds)      clean_builds ;;
    caches)      clean_caches ;;
    transcripts) clean_transcripts ;;
    *) note "unknown category: $cat"; exit 2 ;;
  esac
done

unit="M"; total=$freed
if [ "$total" -ge 1024 ]; then total="$((total / 1024)).$((total % 1024 * 10 / 1024))G"; fi
note "---"
note "total $([ $APPLY = 1 ] && echo freed || echo reclaimable): ${total}"
