#!/usr/bin/env bash
# Durable state sync for the DevAgent self-build loop.
# The loop's state (.selfbuild/) is gitignored, so fresh Orca workspaces start
# with an empty ledger and lessons file. This helper mirrors those two files to
# the orphan branch `selfbuild/state` on origin so every run continues where
# the last one stopped.
#
# Usage:
#   scripts/selfbuild-state.sh pull   # merge origin/selfbuild/state into local .selfbuild/
#   scripts/selfbuild-state.sh push   # publish local .selfbuild/ to origin/selfbuild/state
#
# Merge policy:
#   ledger.jsonl : one line per loop number; on collision the later "ts" wins.
#   lessons.md   : ratchet-only union of unique lines (matches protocol rules).
set -euo pipefail

REPO="${SELFBUILD_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
STATE="$REPO/.selfbuild"
BRANCH="selfbuild/state"
REMOTE_REF="refs/remotes/origin/selfbuild-state"
LEDGER="$STATE/ledger.jsonl"
LESSONS="$STATE/lessons.md"

mkdir -p "$STATE/research" "$STATE/goals" "$STATE/logs" "$STATE/curation"
cd "$REPO"

fetch_state() {
  if git fetch --quiet origin "$BRANCH:$REMOTE_REF" 2>/dev/null; then
    return 0
  fi
  return 1
}

file_at_state() { # file_at_state <path> ; echoes remote content or nothing
  if git rev-parse --verify --quiet "$REMOTE_REF" >/dev/null; then
    git show "$REMOTE_REF:$1" 2>/dev/null || true
  fi
}

merge_ledger() { # merge_ledger <remote-file-or-empty>
  local tmp="$STATE/.ledger.merge"
  { file_at_state ".selfbuild/ledger.jsonl"; [ -f "$LEDGER" ] && cat "$LEDGER" || true; } \
    | sed '/^[[:space:]]*$/d' \
    | awk '
      {
        line = $0; loop = ""; ts = "";
        if (match(line, /"loop":[0-9]+/))
          loop = substr(line, RSTART + 7, RLENGTH - 7);
        if (loop == "") next;
        if (match(line, /"ts":"[^"]*"/))
          ts = substr(line, RSTART + 6, RLENGTH - 7);
        printf "%s\t%s\t%s\n", loop, ts, line;
      }' \
    | sort -t "$(printf '\t')" -k1,1n -k2,2 \
    | awk -F "$(printf '\t')" '{ last[$1] = $3 } END { for (k in last) printf "%s\t%s\n", k, last[k] }' \
    | sort -t "$(printf '\t')" -k1,1n \
    | cut -f2 > "$tmp"
  [ -s "$tmp" ] && mv "$tmp" "$LEDGER" || rm -f "$tmp"
}

merge_lessons() {
  local tmp="$STATE/.lessons.merge"
  touch "$LESSONS"
  { file_at_state ".selfbuild/lessons.md"; cat "$LESSONS"; } | awk '!seen[$0]++' > "$tmp"
  mv "$tmp" "$LESSONS"
}

case "${1:-}" in
  pull)
    if ! fetch_state; then
      echo "[state] no remote state yet ($BRANCH) — starting fresh"
      exit 0
    fi
    merge_ledger
    merge_lessons
    echo "[state] pulled $(wc -l < "$LEDGER" | tr -d ' ') ledger entries into $STATE"
    ;;
  push)
    [ -f "$LEDGER" ] || { echo "[state] nothing to push: $LEDGER missing"; exit 0; }
    fetch_state || echo "[state] no remote state yet — creating $BRANCH"
    merge_ledger   # fold in anything another run pushed since our pull
    merge_lessons
    local_parent=""
    if git rev-parse --verify --quiet "$REMOTE_REF" >/dev/null; then
      local_parent="$(git rev-parse "$REMOTE_REF")"
    fi
    blobs=("ledger.jsonl")
    [ -f "$LESSONS" ] && blobs+=("lessons.md")
    build_tree() {
      local sub="" f sha
      for f in "${blobs[@]}"; do
        [ -f "$STATE/$f" ] || continue
        sha=$(git hash-object -w "$STATE/$f")
        sub+="100644 blob $sha"$'\t'"$f"$'\n'
      done
      local subtree
      subtree=$(printf '%s' "$sub" | git mktree)
      printf '040000 tree %s\t.selfbuild\n' "$subtree" | git mktree
    }
    tree=$(build_tree)
    if [ -n "$local_parent" ]; then
      commit=$(printf 'self-build state sync %s\n' "$(date -u +%FT%TZ)" | git commit-tree "$tree" -p "$local_parent")
    else
      commit=$(printf 'self-build state sync %s\n' "$(date -u +%FT%TZ)" | git commit-tree "$tree")
    fi
    if ! git push --quiet origin "$commit:refs/heads/${BRANCH#refs/heads/}" 2>/tmp/selfbuild-state-push.err; then
      # Another run raced us: re-pull, re-merge, retry once.
      fetch_state || true
      merge_ledger
      merge_lessons
      tree=$(build_tree)
      parent="$(git rev-parse "$REMOTE_REF" 2>/dev/null || true)"
      if [ -n "$parent" ]; then
        commit=$(printf 'self-build state sync %s\n' "$(date -u +%FT%TZ)" | git commit-tree "$tree" -p "$parent")
      else
        commit=$(printf 'self-build state sync %s\n' "$(date -u +%FT%TZ)" | git commit-tree "$tree")
      fi
      git push --quiet origin "$commit:refs/heads/${BRANCH#refs/heads/}" || {
        echo "[state] push failed after retry:"; cat /tmp/selfbuild-state-push.err; exit 1; }
    fi
    echo "[state] pushed $(wc -l < "$LEDGER" | tr -d ' ') ledger entries to $BRANCH"
    ;;
  *)
    echo "usage: $0 pull|push" >&2
    exit 2
    ;;
esac
