#!/usr/bin/env bash
# DevAgent builder loop (self-build factory, role 3).
# Claims queued tasks (written by the scout role) and drives each through
# `devagent consume --auto-pr --auto-merge`: worktree -> gates -> PR -> merge.
# Appends to .selfbuild/ledger.jsonl so the tracker role + starvation gate see it.
#
# Env knobs:
#   BUILDER_REPO            repo path           (default: script's parent)
#   BUILDER_MAX_FAILS       circuit breaker     (default 3 consecutive failures)
#   BUILDER_STARVATION      halt when last N ledger entries are all non-ok across runs (default 5)
#   BUILDER_MAX_LOOPS       per-task repair budget passed to consume (default 2)
#   BUILDER_POLL_SECS       idle wait when queue empty (default 300)
#   BUILDER_DRY_RUN         1 = no consume; append a dry-run ledger line per iteration
#   BUILDER_WORKER          claude-code | opencode (default from devagent.json via consume)
#   BUILDER_NO_MERGE        1 = --auto-pr only, skip --auto-merge
set -euo pipefail

REPO="${BUILDER_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
STATE="$REPO/.selfbuild"
LEDGER="$STATE/ledger.jsonl"
MAX_FAILS="${BUILDER_MAX_FAILS:-3}"
STARVATION="${BUILDER_STARVATION:-5}"
MAX_LOOPS="${BUILDER_MAX_LOOPS:-2}"
POLL_SECS="${BUILDER_POLL_SECS:-300}"
DRY_RUN="${BUILDER_DRY_RUN:-0}"
NO_MERGE="${BUILDER_NO_MERGE:-0}"
DEVAGENT=(node "$REPO/dist/src/cli.js")

mkdir -p "$STATE"
cd "$REPO"

ledger_lines() { [ -f "$LEDGER" ] && wc -l < "$LEDGER" | tr -d ' ' || echo 0; }

record() { # record <status> <goal> [extra-json]
  local status="$1" goal="$2" extra="${3:-}"
  printf '{"loop":%s,"ts":"%s","status":"%s","goal":"%s"%s}\n' \
    "$(($(ledger_lines) + 1))" "$(date -u +%FT%TZ)" "$status" \
    "$(printf '%s' "$goal" | tr -d '"' | cut -c1-160)" "$extra" >> "$LEDGER"
}

oldest_pending() { # prints "id	title" of the oldest pending task, or nothing
  local f oldest=""
  for f in "$REPO"/.devagent/queue/*.json; do
    [ -f "$f" ] || continue
    if grep -q '"status": *"pending"' "$f" 2>/dev/null || grep -q '"status":"pending"' "$f" 2>/dev/null; then
      ts=$(sed -n 's/.*"createdAt": *"\([^"]*\)".*/\1/p' "$f")
      id=$(sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' "$f")
      title=$(sed -n 's/.*"title": *"\([^"]*\)".*/\1/p' "$f")
      if [ -z "$oldest" ] || [[ "$ts" < "$oldest_ts" ]]; then
        oldest="$id"; oldest_ts="$ts"; oldest_title="$title"
      fi
    fi
  done
  [ -n "$oldest" ] && printf '%s\t%s' "$oldest" "${oldest_title:-}"
}

starved() {
  [ -f "$LEDGER" ] || return 1
  local count
  count=$(awk -v lim="$STARVATION" '
    { lines[NR] = $0 }
    END {
      c = 0
      for (i = NR; i >= 1; i--) {
        if (lines[i] ~ /"status":"ok"/ || lines[i] ~ /"status":"merged"/ || lines[i] ~ /"status":"pr-open"/) break
        if (++c >= lim) break
      }
      print c
    }' "$LEDGER")
  [ "${count:-0}" -ge "$STARVATION" ]
}

echo "[builder] loop start repo=$REPO dry_run=$DRY_RUN poll=${POLL_SECS}s"
fails=0
while :; do
  if starved; then
    echo "[starvation] $STARVATION consecutive non-productive iterations — halting builder"
    exit 1
  fi

  PENDING_LINE="$(oldest_pending || true)"
  if [ -z "$PENDING_LINE" ]; then
    echo "[idle] no pending tasks; sleeping ${POLL_SECS}s"
    sleep "$POLL_SECS"
    continue
  fi

  TASK_ID="$(printf '%s' "$PENDING_LINE" | cut -f1)"
  TASK_TITLE="$(printf '%s' "$PENDING_LINE" | cut -f2)"

  echo "=== builder iteration for $TASK_ID: $TASK_TITLE ==="

  if [ "$DRY_RUN" = "1" ]; then
    echo "[dry-run] would consume $TASK_ID (--auto-pr$( [ "$NO_MERGE" = "1" ] || echo ' --auto-merge'))"
    record ok "(dry-run) $TASK_ID $TASK_TITLE" ',"dry_run":true'
    sleep 1
    continue
  fi

  CONSUME_ARGS=(consume --repo "$REPO" --auto-pr --max-loops "$MAX_LOOPS")
  [ "$NO_MERGE" = "0" ] && CONSUME_ARGS+=(--auto-merge)

  OUT="$("${DEVAGENT[@]}" "${CONSUME_ARGS[@]}" 2>&1 || true)"
  echo "$OUT" | tail -5

  if echo "$OUT" | grep -q "^done:"; then
    PR_URL="$(echo "$OUT" | sed -n 's/^PR: //p' | head -1)"
    EXTRA=""
    [ -n "$PR_URL" ] && EXTRA=",\"pr\":\"$PR_URL\""
    record ok "$TASK_ID $TASK_TITLE" "$EXTRA"
    echo "[ok] $TASK_ID complete${PR_URL:+ -> $PR_URL}"
    fails=0
  else
    record failed "$TASK_ID $TASK_TITLE"
    fails=$(( fails + 1 ))
    echo "[fail] $TASK_ID ($fails/$MAX_FAILS)"
    if [ "$fails" -ge "$MAX_FAILS" ]; then
      echo "circuit breaker: $fails consecutive failures — halting builder"
      exit 1
    fi
  fi

  # brief cool-down between tasks so the scout/tracker can interleave
  sleep 10
done
