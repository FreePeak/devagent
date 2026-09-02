#!/usr/bin/env bash
# DevAgent reviewer loop: periodically evaluate open PRs against the
# objective gates (CI green + mergeable + hazard scan) and merge the ones
# that pass. This is the "reviewer" agent of the self-build factory:
#   scout/PO -> queue -> orchestrator (dev) -> PRs -> reviewer (this) -> main
#
# Iron Law: no merge without CI green + MERGEABLE. `automerge` enforces the
# gates itself; this driver only paces it and logs verdicts.
#
# Session-scoped only — does NOT install a LaunchAgent.
#
# Env knobs:
#   REVIEWER_REPO          repo path (default: script's parent)
#   REVIEWER_INTERVAL_SECS poll interval (default 600 = 10m)
#   REVIEWER_MAX_INTERVAL  idle backoff ceiling (default 3600 = 60m)
#   REVIEWER_DRY_RUN       1 = evaluate + log, never merge (default 0)
#
# Backoff (2026-09-01): the task --auto-pr path runs autoReviewAndMergeOne
# inline on publish, so most PRs merge before a reviewer cycle ever sees
# them — idle cycles burn a gh call every 10m and log 0/0 noise forever.
# Cycles with zero PRs double the interval up to REVIEWER_MAX_INTERVAL; any
# cycle that sees PRs resets to the base interval.
set -euo pipefail

REPO="${REVIEWER_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
INTERVAL="${REVIEWER_INTERVAL_SECS:-600}"
MAX_INTERVAL="${REVIEWER_MAX_INTERVAL:-3600}"
DRY_RUN="${REVIEWER_DRY_RUN:-0}"
DEVAGENT=(node "$REPO/dist/src/cli.js")
LOG="$REPO/.devagent/logs/reviewer-loop.log"

cd "$REPO"
mkdir -p "$(dirname "$LOG")"
echo "[reviewer] loop start repo=$REPO interval=${INTERVAL}s max=${MAX_INTERVAL}s dry_run=$DRY_RUN" >> "$LOG"

sleep_secs="$INTERVAL"
while :; do
  ARGS=(automerge --base main)
  [ "$DRY_RUN" = "1" ] && ARGS+=(--dry-run)
  echo "=== reviewer cycle $(date -u +%FT%TZ) (next in ${sleep_secs}s) ===" >> "$LOG"
  # Operator preflight (Q40): probe the provider before the review cycle. On
  # failure the operator-degraded ledger row is written and the cycle is
  # skipped with idle backoff - visible instead of a red herring in the log.
  if [ "$DRY_RUN" != 1 ] && ! "${DEVAGENT[@]}" preflight --role reviewer --repo "$REPO"; then
    echo "[preflight] provider degraded - skipping review cycle (ledger row written)" >> "$LOG"
    sleep_secs=$(( sleep_secs * 2 ))
    [ "$sleep_secs" -gt "$MAX_INTERVAL" ] && sleep_secs="$MAX_INTERVAL"
    sleep "$sleep_secs"
    continue
  fi
  set +e
  OUT="$("${DEVAGENT[@]}" "${ARGS[@]}" 2>&1)"
  RC=$?
  set -e
  echo "$OUT" | tail -10 >> "$LOG"
  [ "$RC" -ne 0 ] && echo "[reviewer] cycle exited $RC (some PRs may be blocked; see gates above)" >> "$LOG"
  # Backoff: count PRs evaluated from the batch summary line "N/M merged".
  total="$(printf '%s\n' "$OUT" | sed -n 's/^\([0-9]*\)\/[0-9]* merged.*/\1/p' | tail -1)"
  total="${total:-0}"
  if [ "$total" -gt 0 ]; then
    sleep_secs="$INTERVAL"
  else
    sleep_secs=$(( sleep_secs * 2 ))
    [ "$sleep_secs" -gt "$MAX_INTERVAL" ] && sleep_secs="$MAX_INTERVAL"
  fi
  sleep "$sleep_secs"
done
